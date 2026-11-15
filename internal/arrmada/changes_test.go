package arrmada

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeArrmada serves the three endpoints the plugin reads, from in-memory
// history (newest first, per kind) and library detail.
type fakeArrmada struct {
	mu   sync.Mutex
	user string
	pass string
	// logins counts sign-ins; expire drops the session, as a sign-out would.
	logins  int
	session string
	history map[string][]historyEvent
	movies  map[int64]any
	series  map[int64]any
	// failing answers 500 for these movie ids, as Arrmada does when a
	// lookup fails.
	failing map[int64]bool
	// requests records each path and query, for asserting what was read.
	requests []string
}

func newFake() *fakeArrmada {
	return &fakeArrmada{
		user:    "silo",
		pass:    "pa:ss",
		history: map[string][]historyEvent{},
		movies:  map[int64]any{},
		series:  map[int64]any{},
	}
}

func (f *fakeArrmada) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.RequestURI())
	// Tests give some calls a base address with a path of its own.
	if i := strings.Index(r.URL.Path, "/api/"); i > 0 {
		r.URL.Path = r.URL.Path[i:]
	}
	if r.URL.Path == "/api/v1/auth/login" {
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || body.Username != f.user || body.Password != f.pass {
			http.Error(w, `{"status":"error","message":"invalid username or password"}`, http.StatusUnauthorized)
			return
		}
		f.logins++
		f.session = fmt.Sprintf("s%d", f.logins)
		http.SetCookie(w, &http.Cookie{Name: "arrmada_session", Value: f.session, Path: "/"})
		_, _ = w.Write([]byte(`{"user":{"username":"silo","role":"manager"}}`))
		return
	}
	if ck, err := r.Cookie("arrmada_session"); err != nil || f.session == "" || ck.Value != f.session {
		http.Error(w, `{"status":"error","message":"not signed in"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/history":
		q := r.URL.Query()
		if q.Get("event") != "all" {
			http.Error(w, "unexpected event filter "+q.Get("event"), http.StatusBadRequest)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		events := f.history[q.Get("kind")]
		start := 0
		if b := q.Get("before"); b != "" {
			id, _ := strconv.ParseInt(b, 10, 64)
			for start < len(events) && events[start].ID >= id {
				start++
			}
		}
		end := min(start+limit, len(events))
		page := historyPage{Events: append([]historyEvent{}, events[start:end]...)}
		if end-start == limit && end < len(events) {
			// Arrmada's cursor is <at_ms>:<kind>:<id>; the plugin treats it
			// as opaque, so the fake uses the id alone.
			page.NextBefore = strconv.FormatInt(events[end-1].ID, 10)
		}
		_ = json.NewEncoder(w).Encode(page)
	case strings.HasPrefix(r.URL.Path, "/api/v1/movies/") && f.failing[mustID(r.URL.Path)]:
		http.Error(w, `{"status":"error","message":"could not load movie"}`, http.StatusInternalServerError)
	case strings.HasPrefix(r.URL.Path, "/api/v1/movies/"):
		f.item(w, f.movies, strings.TrimPrefix(r.URL.Path, "/api/v1/movies/"))
	case strings.HasPrefix(r.URL.Path, "/api/v1/series/"):
		f.item(w, f.series, strings.TrimPrefix(r.URL.Path, "/api/v1/series/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeArrmada) item(w http.ResponseWriter, items map[int64]any, idText string) {
	id, _ := strconv.ParseInt(idText, 10, 64)
	v, ok := items[id]
	if !ok {
		http.Error(w, `{"status":"error","message":"movie not found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// add puts an event at the head of kind's history.
func (f *fakeArrmada) add(kind string, id, item int64, event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history[kind] = append([]historyEvent{{Kind: kind, ID: id, ItemID: item, Event: event}}, f.history[kind]...)
}

func (f *fakeArrmada) cred() Credential {
	return Credential{Username: f.user, Password: f.pass}
}

func movie(paths ...string) map[string]any {
	m := map[string]any{}
	if len(paths) > 0 {
		m["movie_file_path"] = paths[0]
		m["file"] = map[string]any{"path": paths[0]}
	}
	var versions []map[string]any
	for _, p := range paths {
		versions = append(versions, map[string]any{"file_path": p, "file": map[string]any{"path": p}})
	}
	m["versions"] = versions
	return m
}

func show(seasons ...[]string) map[string]any {
	var out []map[string]any
	for _, files := range seasons {
		var eps []map[string]any
		for _, f := range files {
			eps = append(eps, map[string]any{"file_path": f})
		}
		out = append(out, map[string]any{"episodes": eps})
	}
	return map[string]any{"seasons": out}
}

func pollFake(t *testing.T, f *fakeArrmada, m Marker, have bool) ([]Change, Marker) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	changes, next, err := Poll(context.Background(), srv.URL+"/", f.cred(), m, have, srv.Client())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return changes, next
}

func TestFirstPollStartsAtNewestEventAndReportsNothing(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 1, 10, "imported")
	f.add(kindMovie, 2, 11, "imported")
	f.add(kindSeries, 5, 20, "imported")
	f.movies[10] = movie("/media/movies/A (2001)/A.mkv")

	changes, next := pollFake(t, f, Marker{}, false)
	if len(changes) != 0 {
		t.Fatalf("first poll returned %v", changes)
	}
	if next != (Marker{Movie: 2, Series: 5}) {
		t.Fatalf("next = %+v", next)
	}
	for _, r := range f.requests {
		if !strings.Contains(r, "limit=1") && !strings.HasSuffix(r, "/auth/login") {
			t.Errorf("first poll read more than the newest event: %s", r)
		}
	}
}

func TestPollReportsOnlyEventsAfterTheMarker(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 1, 10, "imported")
	f.add(kindMovie, 2, 11, "imported")
	f.add(kindMovie, 3, 11, "renamed")
	f.add(kindMovie, 4, 12, "deleted")
	f.movies[10] = movie("/media/movies/Old (1999)/Old.mkv")
	f.movies[11] = movie("/media/movies/New (2024)/New.mkv", "/media/movies/New (2024)/New - 4K.mkv")
	f.movies[12] = movie() // its only file was deleted

	changes, next := pollFake(t, f, Marker{Movie: 1}, true)
	want := []Change{
		{Path: "/media/movies/New (2024)/New - 4K.mkv", Scope: ScopeFile},
		{Path: "/media/movies/New (2024)/New.mkv", Scope: ScopeFile},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
	if next != (Marker{Movie: 4}) {
		t.Fatalf("next = %+v", next)
	}
}

func TestPollIsRepeatable(t *testing.T) {
	f := newFake()
	f.add(kindSeries, 8, 30, "imported")
	f.series[30] = show([]string{"/tv/Show/Season 01/S01E01.mkv"}, []string{"/tv/Show/Season 02/S02E01.mkv"})

	a, nextA := pollFake(t, f, Marker{Series: 7}, true)
	b, nextB := pollFake(t, f, Marker{Series: 7}, true)
	if !reflect.DeepEqual(a, b) || nextA != nextB {
		t.Fatalf("two polls of one window differ: %v %+v / %v %+v", a, nextA, b, nextB)
	}
	c, _ := pollFake(t, f, nextA, true)
	if len(c) != 0 {
		t.Fatalf("poll after the window returned %v", c)
	}
}

func TestPollPagesUntilTheMarker(t *testing.T) {
	f := newFake()
	for id := int64(1); id <= 450; id++ {
		f.add(kindMovie, id, id, "imported")
		f.movies[id] = movie(fmt.Sprintf("/m/%d/f.mkv", id))
	}
	changes, next := pollFake(t, f, Marker{Movie: 30}, true)
	if len(changes) != 420 || next.Movie != 450 {
		t.Fatalf("got %d changes, next %+v; want 420 and 450", len(changes), next)
	}
}

func TestPollSkipsTitlesRemovedFromArrmada(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 99, "deleted")
	f.add(kindSeries, 2, 98, "deleted")
	changes, next := pollFake(t, f, Marker{Movie: 1, Series: 1}, true)
	if len(changes) != 0 || next != (Marker{Movie: 2, Series: 2}) {
		t.Fatalf("changes %v next %+v", changes, next)
	}
}

func TestPollRestartsWhenIDsGoBackwards(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 3, 10, "imported")
	f.movies[10] = movie("/m/a.mkv")
	changes, next := pollFake(t, f, Marker{Movie: 500}, true)
	if len(changes) != 0 || next.Movie != 3 {
		t.Fatalf("changes %v next %+v; want none and 3", changes, next)
	}
}

func TestPollKeepsMarkerOnError(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "imported")
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := Marker{Movie: 1}
	_, next, err := Poll(context.Background(), srv.URL+"/other", Credential{Username: "silo", Password: "wrong-pw"}, m, true, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401", err)
	}
	if strings.Contains(err.Error(), "wrong-pw") {
		t.Fatalf("error text carries the password: %v", err)
	}
	if next != m {
		t.Fatalf("next = %+v, want the old marker", next)
	}
}

func TestPollNeedsAddressAndKey(t *testing.T) {
	if _, _, err := Poll(context.Background(), "", Credential{Username: "u", Password: "p"}, Marker{}, true, nil); err == nil {
		t.Fatal("no address accepted")
	}
	_, _, err := Poll(context.Background(), "http://x", Credential{APIKey: "arrk_x"}, Marker{}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "account") {
		t.Fatalf("an API key was accepted for Autoscan: %v", err)
	}
}

func TestSeriesChanges(t *testing.T) {
	cases := []struct {
		name string
		show map[string]any
		want []Change
	}{
		{
			name: "season folders scan the show folder",
			show: show([]string{"/tv/Show/Season 01/a.mkv"}, []string{"/tv/Show/Season 02/b.mkv"}),
			want: []Change{{Path: "/tv/Show", Scope: ScopeSubtree}},
		},
		{
			name: "one season folder scans that folder",
			show: show([]string{"/tv/Show/Season 01/a.mkv", "/tv/Show/Season 01/b.mkv"}),
			want: []Change{{Path: "/tv/Show/Season 01", Scope: ScopeSubtree}},
		},
		{
			name: "files in the show folder and a season folder",
			show: show([]string{"/tv/Show/a.mkv"}, []string{"/tv/Show/Season 02/b.mkv"}),
			want: []Change{{Path: "/tv/Show", Scope: ScopeSubtree}},
		},
		{
			name: "misplaced files are scanned folder by folder, not from the TV root",
			show: show([]string{"/tv/Show/Season 01/a.mkv"}, []string{"/tv/Other/Season 02/b.mkv"}),
			want: []Change{
				{Path: "/tv/Other/Season 02", Scope: ScopeSubtree},
				{Path: "/tv/Show/Season 01", Scope: ScopeSubtree},
			},
		},
		{
			name: "windows paths",
			show: show([]string{`D:\TV\Show\Season 01\a.mkv`}, []string{`D:\TV\Show\Season 02\b.mkv`}),
			want: []Change{{Path: `D:\TV\Show`, Scope: ScopeSubtree}},
		},
		{
			name: "no files",
			show: show([]string{""}),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.show)
			var s seriesDetail
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			got := seriesChanges(s)
			if len(got) > 1 {
				got = dedupe(got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCommonDir(t *testing.T) {
	cases := [][3]string{
		{"/a/b/c", "/a/b/d", "/a/b"},
		{"/a/b", "/a/b/c", "/a/b"},
		{"/a/bc", "/a/b", "/a"},
		{"/a", "/b", "/"},
		{`C:\x\y`, `C:\x\z`, `C:\x`},
		{`C:\x`, `D:\x`, ""},
	}
	for _, c := range cases {
		if got := commonDir(c[0], c[1]); got != c[2] {
			t.Errorf("commonDir(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestPollSignsInOnceAndAgainWhenTheSessionEnds(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "imported")
	f.movies[10] = movie("/m/a.mkv")
	srv := httptest.NewServer(f)
	defer srv.Close()
	// A test of its own address, so no session is cached from other tests.
	base := srv.URL + "/relogin"
	f.mu.Lock()
	f.session = ""
	f.mu.Unlock()
	poll := func() {
		t.Helper()
		if _, _, err := Poll(context.Background(), base, f.cred(), Marker{Movie: 1}, true, srv.Client()); err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	poll()
	poll()
	if f.logins != 1 {
		t.Fatalf("two polls signed in %d times, want 1", f.logins)
	}
	f.mu.Lock()
	f.session = "signed-out"
	f.mu.Unlock()
	poll()
	if f.logins != 2 {
		t.Fatalf("after a sign-out: %d sign-ins, want 2", f.logins)
	}
}

func TestParseCredential(t *testing.T) {
	cases := []struct {
		in   string
		want Credential
		ok   bool
	}{
		{"arrk_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst", Credential{APIKey: "arrk_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"}, true},
		{"arrk_abc", Credential{}, false},
		{"arrk_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST", Credential{}, false},
		{" silo:pa:ss ", Credential{Username: "silo", Password: "pa:ss"}, true},
		{"silo:", Credential{}, false},
		{":pw", Credential{}, false},
		{"justakey", Credential{}, false},
		{"", Credential{}, false},
	}
	for _, c := range cases {
		got, err := ParseCredential(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseCredential(%q) = %+v, %v", c.in, got, err)
		}
	}
}

func mustID(path string) int64 {
	id, _ := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
	return id
}

func TestPollPicksEventsByName(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "searched")      // bookkeeping: no scan
	f.add(kindMovie, 3, 11, "moved")         // filed under "other" by Arrmada
	f.add(kindMovie, 4, 12, "version_added") // the same
	f.add(kindMovie, 5, 13, "grabbed")       // nothing on disk yet
	for id := int64(10); id <= 13; id++ {
		f.movies[id] = movie(fmt.Sprintf("/m/%d/f.mkv", id))
	}
	changes, next := pollFake(t, f, Marker{Movie: 1}, true)
	want := []Change{{Path: "/m/11/f.mkv", Scope: ScopeFile}, {Path: "/m/12/f.mkv", Scope: ScopeFile}}
	if !reflect.DeepEqual(changes, want) || next.Movie != 5 {
		t.Fatalf("changes %v next %+v; want %v and 5", changes, next, want)
	}
}

func TestPollOutOfTimeKeepsTheRestForTheNextPoll(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "imported")
	f.add(kindSeries, 9, 30, "imported")
	f.movies[10] = movie("/m/a.mkv")
	f.series[30] = show([]string{"/tv/S/Season 01/e.mkv"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	changes, next, err := poll(context.Background(), srv.URL, f.cred(), Marker{Movie: 1, Series: 8}, true, srv.Client(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing was looked up, so neither marker may move past an event.
	if len(changes) != 0 || next != (Marker{Movie: 1, Series: 8}) {
		t.Fatalf("changes %v next %+v; want none and the old marker", changes, next)
	}
}

func TestPollStopsBeforeATitleThatMayWorkNextTime(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "imported")
	f.add(kindMovie, 3, 11, "imported")
	f.add(kindSeries, 9, 30, "imported")
	f.movies[10] = movie("/m/a.mkv")
	f.movies[11] = movie("/m/b.mkv")
	f.series[30] = show([]string{"/tv/S/Season 01/e.mkv"})
	f.failing = map[int64]bool{11: true}

	changes, next := pollFake(t, f, Marker{Movie: 1, Series: 8}, true)
	if !reflect.DeepEqual(changes, []Change{{Path: "/m/a.mkv", Scope: ScopeFile}}) {
		t.Fatalf("changes = %v", changes)
	}
	// Movie 10 is done, movie 11 is retried next time, and series were not
	// reached.
	if next != (Marker{Movie: 2, Series: 8}) {
		t.Fatalf("next = %+v", next)
	}

	// The first title failing is an error, and the marker stays.
	f.failing = map[int64]bool{10: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := Marker{Movie: 1, Series: 8}
	_, kept, err := Poll(context.Background(), srv.URL, f.cred(), m, true, srv.Client())
	if err == nil || !Transient(err) || kept != m {
		t.Fatalf("err %v kept %+v", err, kept)
	}
}

func TestPollSkipsATitleThatCannotBeRead(t *testing.T) {
	f := newFake()
	f.add(kindMovie, 2, 10, "imported")
	f.add(kindMovie, 3, 11, "imported")
	f.movies[10] = "not a movie" // decodes as a JSON string: unreadable every time
	f.movies[11] = movie("/m/b.mkv")
	changes, next := pollFake(t, f, Marker{Movie: 1}, true)
	if !reflect.DeepEqual(changes, []Change{{Path: "/m/b.mkv", Scope: ScopeFile}}) || next.Movie != 3 {
		t.Fatalf("changes %v next %+v", changes, next)
	}
}

func TestRedirectsAreRefusedAndTheCredentialStays(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.Method == http.MethodPost {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	key := Credential{APIKey: "arrk_" + strings.Repeat("a", 52)}
	r, err := NewRequests(redirect.URL, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Me(context.Background()); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("Me: %v", err)
	}
	_, _, err = Poll(context.Background(), redirect.URL+"/x", Credential{Username: "u", Password: "secret"}, Marker{}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("Poll: %v", err)
	}
	if leaked.Load() {
		t.Fatal("the redirect target received the key or the sign-in")
	}
}

func TestSignInAnswersThatAreNotASession(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"two-factor on":       {`{"two_factor":"code","ticket":"t","expires_in":300}`, "two-factor sign-in on"},
		"two-factor required": {`{"two_factor":"setup","ticket":"t","expires_in":300}`, "must set up two-factor"},
		"some other page":     {`<!doctype html><title>Login</title>`, "not as Arrmada does"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			_, _, err := Poll(context.Background(), srv.URL, Credential{Username: "u", Password: "p"}, Marker{}, false, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestAnotherServicesBodyStaysOutOfErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal-secret-value"))
	}))
	defer srv.Close()
	r, _ := NewRequests(srv.URL, Credential{APIKey: "arrk_" + strings.Repeat("a", 52)}, nil)
	_, err := r.Me(context.Background())
	if err == nil || strings.Contains(err.Error(), "internal-secret-value") {
		t.Fatalf("err = %v", err)
	}
	if !Transient(err) {
		t.Fatalf("a 500 is not transient: %v", err)
	}
	// A 404 that is not Arrmada's is not "the title is gone".
	var se *StatusError
	if errors.As(err, &se) && errors.Is(&StatusError{Code: 404}, errNotFound) {
		t.Fatal("a 404 without Arrmada's body matched errNotFound")
	}
}

func TestBaseURLChecks(t *testing.T) {
	for _, bad := range []string{
		"arrmada:7878",                    // no scheme
		"ftp://arrmada",                   // not http
		"http://",                         // no host
		"http://silo:secret@arrmada:7878", // credentials in the address
		"http://arrmada:7878/?a=b",        // a query bends every path
		"http://arrmada:7878#x",           // so does a fragment
	} {
		if _, err := checkBaseURL(bad); err == nil {
			t.Errorf("checkBaseURL(%q) accepted it", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("checkBaseURL(%q) repeats the password: %v", bad, err)
		}
	}
	for in, want := range map[string]string{
		" http://Arrmada:7878/ ":       "http://Arrmada:7878",
		"https://arr.example.com/arr/": "https://arr.example.com/arr",
	} {
		if got, err := checkBaseURL(in); err != nil || got != want {
			t.Errorf("checkBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestOnlyAbsolutePathsAreReported(t *testing.T) {
	in := []Change{
		{Path: "/m/a.mkv", Scope: ScopeFile},
		{Path: `D:\TV\Show`, Scope: ScopeSubtree},
		{Path: `\\nas\tv\Show`, Scope: ScopeSubtree},
		{Path: "relative/a.mkv", Scope: ScopeFile},
		{Path: "/m/b.mkv\n/etc/passwd", Scope: ScopeFile},
		{Path: "/m/c\x00.mkv", Scope: ScopeFile},
	}
	got := dedupe(in)
	want := []Change{
		{Path: "/m/a.mkv", Scope: ScopeFile},
		{Path: `D:\TV\Show`, Scope: ScopeSubtree},
		{Path: `\\nas\tv\Show`, Scope: ScopeSubtree},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
