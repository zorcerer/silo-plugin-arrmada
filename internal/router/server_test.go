package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/zorcerer/silo-plugin-arrmada/internal/arrmada"
)

const key = "arrk_router"

// fakeArrmada serves the requests routes a personal API key reaches.
type fakeArrmada struct {
	mu sync.Mutex
	// role is what /auth/me says; approveForbidden answers /approve with 403.
	role             string
	approveForbidden bool
	// createStatus and createBody replace the create answer when set.
	createStatus int
	createBody   string
	// requests by id, as GET /api/v1/requests/{id} returns them.
	requests map[int64]map[string]any
	created  []map[string]any
	approved []map[string]any
}

func newFake() *fakeArrmada {
	return &fakeArrmada{role: "manager", requests: map[int64]map[string]any{}}
}

func (f *fakeArrmada) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != key {
		http.Error(w, `{"status":"error","message":"invalid API key"}`, http.StatusUnauthorized)
		return
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var id int64
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
		write(map[string]any{"user": map[string]any{"username": "silo", "role": f.role}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/requests":
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "json only", http.StatusUnsupportedMediaType)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(f.createBody))
			return
		}
		req := map[string]any{"id": 7, "status": "pending", "tracking": map[string]any{"stage": "pending"}}
		f.requests[7] = req
		write(map[string]any{"request": req, "subscribed": false})
	case r.Method == http.MethodPost && scan(r.URL.Path, "/api/v1/requests/%d/approve", &id):
		if f.approveForbidden {
			http.Error(w, `{"status":"error","message":"forbidden"}`, http.StatusForbidden)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.approved = append(f.approved, body)
		f.requests[id]["status"] = "approved"
		f.requests[id]["tracking"] = map[string]any{"stage": "searching"}
		write(f.requests[id])
	case r.Method == http.MethodGet && scan(r.URL.Path, "/api/v1/requests/%d", &id):
		req, ok := f.requests[id]
		if !ok {
			http.Error(w, `{"status":"error","message":"request not found"}`, http.StatusNotFound)
			return
		}
		write(map[string]any{"request": req})
	default:
		http.NotFound(w, r)
	}
}

// scan matches path against format exactly.
func scan(path, format string, id *int64) bool {
	n, err := fmt.Sscanf(path, format, id)
	return err == nil && n == 1 && fmt.Sprintf(format, *id) == path
}

func conn(t *testing.T, url string, cfg map[string]any) *pluginv1.RouterConnection {
	t.Helper()
	st, err := structpb.NewStruct(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &pluginv1.RouterConnection{Id: "c1", BaseUrl: url, ApiKey: key, Config: st}
}

func movieRequest(qualities ...*pluginv1.RequestedQuality) *pluginv1.FulfillRequest {
	return &pluginv1.FulfillRequest{
		CapabilityId: "requests",
		Request: &pluginv1.RequestDescriptor{
			MediaType:         "movie",
			Title:             "Arrival",
			Year:              2016,
			ExternalIds:       map[string]string{"tmdb": "329865", "imdb": "tt2543164"},
			RequesterUsername: "alex",
		},
		Qualities: qualities,
	}
}

var (
	hd  = &pluginv1.RequestedQuality{Id: "1080p"}
	uhd = &pluginv1.RequestedQuality{Id: "2160p", Is4K: true}
)

func fulfill(t *testing.T, f *fakeArrmada, cfg map[string]any, req *pluginv1.FulfillRequest) *pluginv1.FulfillResponse {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	req.Connections = []*pluginv1.RouterConnection{conn(t, srv.URL, cfg)}
	resp, err := New().Fulfill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fulfill: %v", err)
	}
	return resp
}

func TestFulfillCreatesAndApproves(t *testing.T) {
	f := newFake()
	resp := fulfill(t, f, map[string]any{"quality_profile": "hd-1080p"}, movieRequest(hd, uhd))
	if len(resp.GetTargets()) != 1 {
		t.Fatalf("targets = %v (%s)", resp.GetTargets(), resp.GetMessage())
	}
	tg := resp.GetTargets()[0]
	if tg.GetQuality() != "1080p" || tg.GetExternalId() != "7" || tg.GetConnectionId() != "c1" || tg.GetStatus() != "queued" || tg.GetExternalStatus() != "searching" {
		t.Fatalf("target = %v", tg)
	}
	c := f.created[0]
	if c["media_type"] != "movie" || c["tmdb_id"] != float64(329865) || c["quality_profile"] != "hd-1080p" || c["note"] != "Requested in Silo by alex" {
		t.Fatalf("created = %v", c)
	}
	if len(f.approved) != 1 || f.approved[0]["quality_profile"] != "hd-1080p" {
		t.Fatalf("approved = %v", f.approved)
	}
}

func TestFulfillLeavesItPendingWhenApprovalIsOffOrRefused(t *testing.T) {
	for name, setup := range map[string]func(*fakeArrmada) map[string]any{
		"approval off":     func(*fakeArrmada) map[string]any { return map[string]any{"approve": false} },
		"approval refused": func(f *fakeArrmada) map[string]any { f.approveForbidden = true; return map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			cfg := setup(f)
			tg := fulfill(t, f, cfg, movieRequest(hd)).GetTargets()[0]
			if tg.GetStatus() != "queued" || tg.GetWording().GetLabel() != "Awaiting approval" || tg.GetExternalId() != "7" {
				t.Fatalf("target = %v", tg)
			}
			if len(f.approved) != 0 {
				t.Fatalf("approved = %v", f.approved)
			}
		})
	}
}

func TestFulfillQualities(t *testing.T) {
	f := newFake()
	resp := fulfill(t, f, map[string]any{}, movieRequest(uhd))
	if len(resp.GetTargets()) != 0 || !strings.Contains(resp.GetMessage(), "4K") || len(f.created) != 0 {
		t.Fatalf("4K without supports_4k: %v %q, created %v", resp.GetTargets(), resp.GetMessage(), f.created)
	}
	f = newFake()
	resp = fulfill(t, f, map[string]any{"supports_4k": true, "quality_profile_4k": "uhd"}, movieRequest(uhd))
	if len(resp.GetTargets()) != 1 || resp.GetTargets()[0].GetQuality() != "2160p" || f.created[0]["quality_profile"] != "uhd" {
		t.Fatalf("4K with supports_4k: %v, created %v", resp.GetTargets(), f.created)
	}
}

func TestFulfillRefusals(t *testing.T) {
	cases := []struct {
		name, body, status, msg string
		code                    int
	}{
		{"already in the library", `{"status":"error","message":"those seasons are already in the library"}`, "completed", "Already in Arrmada's library", 409},
		{"quota", `{"status":"error","message":"You've reached your limit of 5 movies a week.","code":"quota"}`, "failed", "You've reached your limit of 5 movies a week.", 429},
		{"adult", `{"status":"error","message":"That title isn't available."}`, "failed", "That title isn't available.", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			f.createStatus, f.createBody = c.code, c.body
			tg := fulfill(t, f, map[string]any{}, movieRequest(hd)).GetTargets()[0]
			if tg.GetStatus() != c.status || !strings.Contains(tg.GetMessage(), c.msg) {
				t.Fatalf("target = %v", tg)
			}
		})
	}
}

func TestFulfillWithoutTMDBOrConnection(t *testing.T) {
	req := movieRequest(hd)
	req.Request.ExternalIds = map[string]string{"imdb": "tt1"}
	if resp := fulfill(t, newFake(), map[string]any{}, req); len(resp.GetTargets()) != 0 || !strings.Contains(resp.GetMessage(), "TMDB") {
		t.Fatalf("no tmdb: %v", resp)
	}
	resp, err := New().Fulfill(context.Background(), movieRequest(hd))
	if err != nil || len(resp.GetTargets()) != 0 || resp.GetMessage() == "" {
		t.Fatalf("no connection: %v %v", resp, err)
	}
}

func TestCheckStatus(t *testing.T) {
	f := newFake()
	f.requests[1] = map[string]any{"id": 1, "status": "approved", "tracking": map[string]any{
		"stage": "downloading", "progress": 0.25, "size_bytes": 4000, "downloads": 1, "eta_seconds": 600}}
	f.requests[2] = map[string]any{"id": 2, "status": "approved", "tracking": map[string]any{"stage": "available"}}
	f.requests[3] = map[string]any{"id": 3, "status": "declined", "tracking": map[string]any{"stage": "declined"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ref := func(id string) *pluginv1.TargetRef {
		return &pluginv1.TargetRef{Quality: "1080p", ConnectionId: "c1", ExternalId: id}
	}
	resp, err := New().CheckStatus(context.Background(), &pluginv1.CheckStatusRequest{
		Targets:     []*pluginv1.TargetRef{ref("1"), ref("2"), ref("3"), ref("99"), ref("")},
		Connections: []*pluginv1.RouterConnection{conn(t, srv.URL, map[string]any{})},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := resp.GetStatuses()
	if len(st) != 5 {
		t.Fatalf("statuses = %v", st)
	}
	p := st[0].GetProgress()
	if st[0].GetStatus() != "downloading" || p.GetPhase() != "downloading" || p.GetBytesTotal() != 4000 || p.GetBytesLeft() != 3000 || p.GetEstimatedCompletion() == nil {
		t.Errorf("downloading = %v", st[0])
	}
	if st[0].GetQuality() != "1080p" || st[0].GetConnectionId() != "c1" {
		t.Errorf("target identity lost: %v", st[0])
	}
	for i, want := range []string{"downloading", "completed", "failed", "failed", "completed"} {
		if st[i].GetStatus() != want {
			t.Errorf("status %d = %q, want %q (%v)", i, st[i].GetStatus(), want, st[i])
		}
	}
	if !strings.Contains(st[3].GetMessage(), "deleted") {
		t.Errorf("gone = %v", st[3])
	}
}

func TestStatusOfStages(t *testing.T) {
	cases := map[string]struct{ status, phase string }{
		"searching":   {"queued", ""},
		"queued":      {"downloading", "queued"},
		"downloading": {"downloading", "downloading"},
		"paused":      {"downloading", "paused"},
		"failed":      {"downloading", "stalled"},
		"importing":   {"downloading", "importing"},
		"adding":      {"downloading", "importing"},
		"partial":     {"downloading", ""},
		"available":   {"completed", ""},
	}
	for stage, want := range cases {
		raw := fmt.Sprintf(`{"id":1,"status":"approved","tracking":{"stage":%q,"size_bytes":10,"downloads":1}}`, stage)
		got := statusOf(mustRequest(t, raw))
		if got.GetStatus() != want.status || got.GetProgress().GetPhase() != want.phase {
			t.Errorf("%s: status %q phase %q, want %q %q", stage, got.GetStatus(), got.GetProgress().GetPhase(), want.status, want.phase)
		}
	}
	nd := statusOf(mustRequest(t, `{"status":"approved","tracking":{"stage":"searching","note":"Not out yet"}}`))
	if nd.GetWording().GetLabel() != "Not out yet" {
		t.Errorf("note wording = %v", nd.GetWording())
	}
}

func TestTestConnection(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	resp, err := New().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{Connection: conn(t, srv.URL, map[string]any{})})
	if err != nil || !resp.GetOk() || !strings.Contains(resp.GetMessage(), "silo (manager)") {
		t.Fatalf("manager: %v %v", resp, err)
	}
	f.role = "requester"
	resp, _ = New().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{Connection: conn(t, srv.URL, map[string]any{})})
	if !resp.GetOk() || !strings.Contains(resp.GetMessage(), "approval") {
		t.Fatalf("requester: %v", resp)
	}
	bad := conn(t, srv.URL, map[string]any{})
	bad.ApiKey = "arrk_wrong"
	resp, _ = New().TestConnection(context.Background(), &pluginv1.TestConnectionRequest{Connection: bad})
	if resp.GetOk() || !strings.Contains(resp.GetMessage(), "401") {
		t.Fatalf("wrong key: %v", resp)
	}
}

func mustRequest(t *testing.T, raw string) (r arrmada.Request) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
