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
)

const (
	kindMovie  = "movie"
	kindSeries = "series"
)

// kinds are the history kinds this plugin reads. Arrmada's books and music
// have history too; Silo's audiobook and ebook libraries are a later addition.
var kinds = []string{kindMovie, kindSeries}

// watchedEvents are the History categories that change files on disk. Each one
// covers several event names in Arrmada: "deleted" is deleted, file.deleted
// and version_removed; "renamed" is renamed, renumbered and rename.skipped.
const watchedEvents = "imported,upgraded,renamed,deleted"

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
// cred must be an account: Arrmada's personal API keys do not reach history
// or the movie and series detail.
func Poll(ctx context.Context, baseURL string, cred Credential, marker Marker, haveMarker bool, httpClient *http.Client) ([]Change, Marker, error) {
	if !cred.IsAccount() {
		return nil, marker, errors.New("Autoscan needs an Arrmada account, not an API key: Arrmada's keys cannot read its history. Put username:password in the connection's API key field")
	}
	c, err := newClient(baseURL, cred, httpClient)
	if err != nil {
		return nil, marker, err
	}

	next := marker
	items := map[string][]int64{}
	for _, kind := range kinds {
		newest, ids, err := c.newEvents(ctx, kind, marker.get(kind), haveMarker)
		if err != nil {
			return nil, marker, err
		}
		next.set(kind, newest)
		items[kind] = ids
	}
	if !haveMarker {
		return nil, next, nil
	}

	var changes []Change
	for _, id := range items[kindMovie] {
		var m movieDetail
		err := c.getJSON(ctx, "/api/v1/movies/"+strconv.FormatInt(id, 10), &m)
		if errors.Is(err, errNotFound) {
			continue // removed from Arrmada with its files; nothing left to point at
		}
		if err != nil {
			return nil, marker, err
		}
		changes = append(changes, movieChanges(m)...)
	}
	for _, id := range items[kindSeries] {
		var s seriesDetail
		err := c.getJSON(ctx, "/api/v1/series/"+strconv.FormatInt(id, 10), &s)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, marker, err
		}
		changes = append(changes, seriesChanges(s)...)
	}
	return dedupe(changes), next, nil
}

// newEvents reads kind's history from newest to oldest until it reaches an
// event at or below after. It returns the newest event id and the distinct
// items of the events above after, oldest event's item last. With collect
// false (a first poll) it reads one event, for the id.
func (c *client) newEvents(ctx context.Context, kind string, after int64, collect bool) (int64, []int64, error) {
	newest := after
	seen := map[int64]bool{}
	var ids []int64
	before := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("kind", kind)
		q.Set("event", watchedEvents)
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
				return newest, ids, nil
			}
			if !seen[ev.ItemID] {
				seen[ev.ItemID] = true
				ids = append(ids, ev.ItemID)
			}
		}
		if p.NextBefore == "" {
			break
		}
		before = p.NextBefore
	}
	return newest, ids, nil
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

// dedupe drops repeated changes and sorts the rest, so two polls of the same
// window return the same list.
func dedupe(in []Change) []Change {
	seen := map[Change]bool{}
	out := make([]Change, 0, len(in))
	for _, c := range in {
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
