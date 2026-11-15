//go:build integration

// Builds the plugin binary, starts it over go-plugin the way Silo does, and
// polls it twice and sends it a request against a stub Arrmada. Run: go test -tags integration .
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	plugin "github.com/hashicorp/go-plugin"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

func TestPollChangesOverGRPC(t *testing.T) {
	const apiKey = "arrk_e2e"
	const account = "silo:pw"
	const file = "/media/movies/Film (2020)/Film (2020).mkv"

	// The movie import is event 1 before the first poll and event 2 after it.
	var imported atomic.Bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "arrmada_session", Value: "s1", Path: "/"})
			_, _ = w.Write([]byte(`{"user":{"username":"silo","role":"manager"}}`))
			return
		}
		ck, _ := r.Cookie("arrmada_session")
		byKey := r.Header.Get("X-Api-Key") == apiKey
		if (ck == nil || ck.Value != "s1") && !byKey {
			http.Error(w, "{}", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/requests":
			if !byKey {
				http.Error(w, "{}", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"request":{"id":3,"status":"approved","tracking":{"stage":"searching"}}}`))
		case "/api/v1/history":
			if r.URL.Query().Get("kind") != "movie" {
				_, _ = w.Write([]byte(`{"events":[],"next_before":""}`))
				return
			}
			if imported.Load() {
				_, _ = w.Write([]byte(`{"events":[{"kind":"movie","id":2,"item_id":5,"event":"imported"},{"kind":"movie","id":1,"item_id":4,"event":"imported"}],"next_before":""}`))
				return
			}
			_, _ = w.Write([]byte(`{"events":[{"kind":"movie","id":1,"item_id":4,"event":"imported"}],"next_before":""}`))
		case "/api/v1/movies/5":
			_, _ = w.Write([]byte(`{"id":5,"movie_file_path":"` + file + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()

	bin := filepath.Join(t.TempDir(), "plugin")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  runtime.HandshakeConfig(),
		Plugins:          runtime.DefaultPluginSet(runtime.CapabilityServers{}),
		Cmd:              exec.Command(bin),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
	})
	defer client.Kill()
	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("connect to plugin: %v", err)
	}
	raw, err := rpc.Dispense(runtime.PluginSetName)
	if err != nil {
		t.Fatalf("dispense: %v", err)
	}
	rt := raw.(*runtime.Client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := &pluginv1.ResolvedConnection{BaseUrl: stub.URL, ApiKey: account}

	first, err := rt.ScanSource().PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "autoscan", Connection: conn})
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(first.GetChanges()) != 0 || first.GetNextMarker() != "v1;m=1;s=0" {
		t.Fatalf("first poll = %v, marker %q", first.GetChanges(), first.GetNextMarker())
	}

	imported.Store(true)
	second, err := rt.ScanSource().PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "autoscan", Connection: conn, Marker: first.GetNextMarker()})
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	got := second.GetChanges()
	if len(got) != 1 || got[0].GetSourcePath() != file || got[0].GetScope() != pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE {
		t.Fatalf("second poll = %v", got)
	}
	if second.GetNextMarker() != "v1;m=2;s=0" {
		t.Fatalf("second marker = %q", second.GetNextMarker())
	}

	if _, err := rt.ScanSource().PollChanges(ctx, &pluginv1.PollChangesRequest{CapabilityId: "autoscan"}); err == nil {
		t.Fatal("a poll with no connection succeeded")
	}

	ful, err := rt.RequestRouter().Fulfill(ctx, &pluginv1.FulfillRequest{
		CapabilityId: "requests",
		Request:      &pluginv1.RequestDescriptor{MediaType: "movie", Title: "Film", Year: 2020, ExternalIds: map[string]string{"tmdb": "12"}},
		Qualities:    []*pluginv1.RequestedQuality{{Id: "1080p"}},
		Connections:  []*pluginv1.RouterConnection{{Id: "c1", BaseUrl: stub.URL, ApiKey: apiKey}},
	})
	if err != nil {
		t.Fatalf("Fulfill: %v", err)
	}
	if tg := ful.GetTargets(); len(tg) != 1 || tg[0].GetExternalId() != "3" || tg[0].GetStatus() != "queued" {
		t.Fatalf("Fulfill = %v", ful)
	}
}
