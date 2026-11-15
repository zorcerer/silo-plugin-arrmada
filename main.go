// Command silo-plugin-arrmada connects Silo to Arrmada with two capabilities.
//
// request_router.v1 sends Silo's movie and series requests to Arrmada's own
// requests and reports their progress (internal/router).
//
// scan_source.v1 reads Arrmada's movie and series history on each poll, looks
// up the files of every title that changed, and returns those paths as
// Arrmada sees them. Silo applies the source's path rewrites and queues the
// scans.
//
// The address and credential arrive with every call; the plugin stores
// neither.
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/zorcerer/silo-plugin-arrmada/internal/arrmada"
	"github.com/zorcerer/silo-plugin-arrmada/internal/router"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	manifest *pluginv1.PluginManifest
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure accepts nothing: the plugin has no settings of its own.
func (s *runtimeServer) Configure(context.Context, *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	return &pluginv1.ConfigureResponse{}, nil
}

type scanSourceServer struct {
	pluginv1.UnimplementedScanSourceServer
}

func (s *scanSourceServer) PollChanges(ctx context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error) {
	conn := req.GetConnection()
	if conn.GetBaseUrl() == "" {
		return nil, fmt.Errorf("no Arrmada connection is bound to this source")
	}
	marker, haveMarker, err := arrmada.ParseMarker(req.GetMarker())
	if err != nil {
		// A marker from another plugin or version: start from Arrmada's
		// current position, as on a first poll.
		haveMarker = false
	}
	cred, err := arrmada.ParseCredential(conn.GetApiKey())
	if err != nil {
		return nil, err
	}
	changes, next, err := arrmada.Poll(ctx, conn.GetBaseUrl(), cred, marker, haveMarker, nil)
	if err != nil {
		return nil, err
	}
	resp := &pluginv1.PollChangesResponse{NextMarker: next.String()}
	for _, c := range changes {
		scope := pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE
		if c.Scope == arrmada.ScopeSubtree {
			scope = pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_SUBTREE
		}
		resp.Changes = append(resp.Changes, &pluginv1.ScanSourceChange{SourcePath: c.Path, Scope: scope})
	}
	return resp, nil
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}
	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:       &runtimeServer{manifest: manifest},
			ScanSource:    &scanSourceServer{},
			RequestRouter: router.New(),
		},
	})
}

// loadManifest parses the embedded manifest, sets the build's version, and
// stamps the checksum of the running binary, which Silo checks on upload.
func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", exe, err)
	}
	sum := sha256.Sum256(data)
	manifest.Checksum = hex.EncodeToString(sum[:])
	return manifest, nil
}
