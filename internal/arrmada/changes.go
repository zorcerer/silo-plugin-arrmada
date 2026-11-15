package arrmada

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	kindMovie  = "movie"
	kindSeries = "series"
)

// kinds are the history kinds this plugin reads. Arrmada's books and music
// have history too; Silo's audiobook and ebook libraries are a later addition.
var kinds = []string{kindMovie, kindSeries}

// watchedEvents are the history event names that change files on disk, as
// Arrmada writes them. History's own categories file moved, version_added and
// files.repaired under "other", so the plugin reads every event (event=all)
// and picks these out itself.
var watchedEvents = map[string]bool{
	"imported":        true,
	"upgraded":        true,
	"renamed":         true,
	"renumbered":      true,
	"deleted":         true,
	"file.deleted":    true,
	"version_removed": true,
	"version_added":   true,
	"moved":           true, // to another library folder
	"files.repaired":  true, // a show's misplaced files put back in its folder
}

// pollBudget is how long a poll spends looking titles up before it returns
// what it has. Silo gives a poll five minutes and keeps the old marker when
// one times out, so a poll that tried to read 5,000 titles in one go would
// time out on the same window every time.
const pollBudget = 2 * time.Minute

// pageSize is the most Arrmada's /api/v1/history returns at once.
const pageSize = 200

// maxPages bounds one kind's history read to 5,000 events per poll. A poll
// that reaches it reports what it read and moves the marker past it.
const maxPages = 25

// ScopeFile and ScopeSubtree mirror the two scan_source scopes the plugin
// uses, so this package does not import the protobuf types.
type Scope int

const (
	ScopeFile Scope = iota + 1
	ScopeSubtree
)

// Change is one path to scan, as Arrmada sees it.
type Change struct {
	Path  string
	Scope Scope
}

// historyEvent is the part of an /api/v1/history row the plugin reads.
type historyEvent struct {
	Kind   string `json:"kind"`
	ItemID int64  `json:"item_id"`
	ID     int64  `json:"id"`
	Event  string `json:"event"`
}

type historyPage struct {
	Events     []historyEvent `json:"events"`
	NextBefore string         `json:"next_before"`
}

type movieFile struct {
	Path string `json:"path"`
}

type movieDetail struct {
	MovieFilePath string     `json:"movie_file_path"`
	File          *movieFile `json:"file"`
	Versions      []struct {
		FilePath string     `json:"file_path"`
		File     *movieFile `json:"file"`
	} `json:"versions"`
}

type seriesDetail struct {
	Seasons []struct {
		Episodes []struct {
			FilePath string `json:"file_path"`
		} `json:"episodes"`
	} `json:"seasons"`
}

// Poll returns what changed in Arrmada since marker, and the marker to store.
// haveMarker is false on Silo's first poll of a source: the result is then the
// current position and no changes, so the source does not replay history.
//
// Titles are looked up oldest event first. The marker moves past an event only
// once its title has been handled, so a poll cut short by pollBudget, or by a
// lookup that may work next time, resumes where it stopped.
//
// cred must be an account: Arrmada's personal API keys do not reach history
// or the movie and series detail.
func Poll(ctx context.Context, baseURL string, cred Credential, marker Marker, haveMarker bool, httpClient *http.Client) ([]Change, Marker, error) {
	return poll(ctx, baseURL, cred, marker, haveMarker, httpClient, pollBudget)
}

func poll(ctx context.Context, baseURL string, cred Credential, marker Marker, haveMarker bool, httpClient *http.Client, budget time.Duration) ([]Change, Marker, error) {
	if !cred.IsAccount() {
		return nil, marker, errors.New("Autoscan needs an Arrmada account, not an API key: Arrmada's keys cannot read its history. Put username:password in the connection's API key field")
	}
	c, err := newClient(baseURL, cred, httpClient)
	if err != nil {
		return nil, marker, err
	}
	start := time.Now()

	newest := marker
	pending := map[string][]historyEvent{}
	for _, kind := range kinds {
		id, events, err := c.newEvents(ctx, kind, marker.get(kind), haveMarker)
		if err != nil {
			return nil, marker, err
		}
		newest.set(kind, id)
		pending[kind] = events
	}
	if !haveMarker {
		return nil, newest, nil
	}

	// next moves one kind at a time: a kind this poll did not finish keeps
	// the stored marker, or stops just before the event it did not reach.
	next := marker
	var changes []Change
	handled := 0
	for _, kind := range kinds {
		events := pending[kind]
		done := map[int64]bool{}                // titles already looked up this poll
		for i := len(events) - 1; i >= 0; i-- { // oldest first
			ev := events[i]
			if done[ev.ItemID] {
				continue
			}
			if time.Since(start) > budget {
				next.set(kind, ev.ID-1)
				return dedupe(changes), next, nil
			}
			found, err := c.lookUp(ctx, kind, ev.ItemID)
			if err != nil && Transient(err) {
				if handled == 0 {
					return nil, marker, err
				}
				next.set(kind, ev.ID-1)
				return dedupe(changes), next, nil
			}
			// A lookup that keeps failing (an answer it cannot read, a title
			// the account may not see) is skipped, so one title cannot hold
			// back every later change.
			changes = append(changes, found...)
			done[ev.ItemID] = true
			handled++
		}
		next.set(kind, newest.get(kind))
	}
	return dedupe(changes), next, nil
}

// lookUp is the paths of one title: its files for a movie, its folders for a
// show. A title Arrmada no longer has yields none.
func (c *client) lookUp(ctx context.Context, kind string, id int64) ([]Change, error) {
	if kind == kindMovie {
		var m movieDetail
		err := c.getJSON(ctx, "/api/v1/movies/"+strconv.FormatInt(id, 10), &m)
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return movieChanges(m), err
	}
	var s seriesDetail
	err := c.getJSON(ctx, "/api/v1/series/"+strconv.FormatInt(id, 10), &s)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	return seriesChanges(s), err
}

// newEvents reads kind's history from newest to oldest until it reaches an
// event at or below after. It returns the newest event id and the watched
// events above after, newest first. With collect false (a first poll) it
// reads one event, for the id.
func (c *client) newEvents(ctx context.Context, kind string, after int64, collect bool) (int64, []historyEvent, error) {
	newest := after
	var events []historyEvent
	before := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("kind", kind)
		q.Set("event", "all")
		if collect {
			q.Set("limit", strconv.Itoa(pageSize))
		} else {
			q.Set("limit", "1")
		}
		if before != "" {
			q.Set("before", before)
		}
		var p historyPage
		if err := c.getJSON(ctx, "/api/v1/history?"+q.Encode(), &p); err != nil {
			return after, nil, err
		}
		if page == 0 {
			if len(p.Events) == 0 {
				// Nothing of this kind, ever, or the history was cleared.
				return 0, nil, nil
			}
			if p.Events[0].ID < after {
				// Ids went backwards: Arrmada's database was restored or
				// replaced. Start over from its newest event rather than wait
				// for its ids to pass the old ones.
				return p.Events[0].ID, nil, nil
			}
			newest = p.Events[0].ID
		}
		if !collect {
			return newest, nil, nil
		}
		for _, ev := range p.Events {
			if ev.Kind != kind || ev.ID <= after {
				return newest, events, nil
			}
			if watchedEvents[ev.Event] {
				events = append(events, ev)
			}
		}
		if p.NextBefore == "" {
			break
		}
		before = p.NextBefore
	}
	return newest, events, nil
}

// movieChanges is every file the movie has, one per version. Silo widens a
// video file to a scan of its folder, which also finds the file a rename or an
// upgrade replaced.
func movieChanges(m movieDetail) []Change {
	var out []Change
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, Change{Path: p, Scope: ScopeFile})
		}
	}
	add(m.MovieFilePath)
	if m.File != nil {
		add(m.File.Path)
	}
	for _, v := range m.Versions {
		add(v.FilePath)
		if v.File != nil {
			add(v.File.Path)
		}
	}
	return out
}

// seriesChanges is the show's folder. Arrmada's history says how many episodes
// an import placed but not which, so the whole show is scanned. The folder is
// the deepest one that holds every episode file's folder, when each of those is
// it or a folder directly in it (a season folder). A show whose files are
// scattered further apart, which Arrmada lists as misplaced, gets one scan per
// folder instead, so a TV root is never scanned whole.
func seriesChanges(s seriesDetail) []Change {
	dirSet := map[string]bool{}
	for _, season := range s.Seasons {
		for _, ep := range season.Episodes {
			if p := strings.TrimSpace(ep.FilePath); p != "" {
				dirSet[dirOf(p)] = true
			}
		}
	}
	if len(dirSet) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	common := dirs[0]
	for _, d := range dirs[1:] {
		common = commonDir(common, d)
	}
	tight := true
	for _, d := range dirs {
		if d != common && dirOf(d) != common {
			tight = false
			break
		}
	}
	if tight && common != "/" && common != "" {
		return []Change{{Path: common, Scope: ScopeSubtree}}
	}
	out := make([]Change, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, Change{Path: d, Scope: ScopeSubtree})
	}
	return out
}

// dirOf is the folder of p, for paths written with / or \ (an Arrmada on
// Windows). The separator in p is kept.
func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	switch {
	case i < 0:
		return ""
	case i == 0:
		return p[:1]
	}
	return p[:i]
}

// commonDir is the deepest folder that holds both a and b, or "" when they
// share none.
func commonDir(a, b string) string {
	for !within(b, a) {
		parent := dirOf(a)
		if parent == a || parent == "" {
			return ""
		}
		a = parent
	}
	return a
}

// within reports whether p is dir or a path inside it.
func within(p, dir string) bool {
	if p == dir {
		return true
	}
	if dir == "/" || dir == `\` {
		return strings.HasPrefix(p, dir)
	}
	return strings.HasPrefix(p, dir+"/") || strings.HasPrefix(p, dir+`\`)
}

// reportable keeps a path Silo can resolve: absolute (/..., C:\..., or
// \\server\...), with no control characters. Anything else would only fail
// to match a library folder; dropping it here keeps an odd answer from
// Arrmada out of Silo's scan queue.
func reportable(p string) bool {
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\\`):
		return true
	case len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		c := p[0] | 0x20
		return 'a' <= c && c <= 'z'
	}
	return false
}

// dedupe drops repeated changes and sorts the rest, so two polls of the same
// window return the same list.
func dedupe(in []Change) []Change {
	seen := map[Change]bool{}
	out := make([]Change, 0, len(in))
	for _, c := range in {
		if !reportable(c.Path) {
			continue
		}
		if !strings.Contains(c.Path, `\`) {
			c.Path = path.Clean(c.Path)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// String is for test failures.
func (c Change) String() string {
	scope := "file"
	if c.Scope == ScopeSubtree {
		scope = "subtree"
	}
	return fmt.Sprintf("%s %s", scope, c.Path)
}
