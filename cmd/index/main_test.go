package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// Silo decodes the index with encoding/json into these types; the test does
// the same, so a field that does not survive that round trip fails here.
type siloIndex struct {
	Plugins []struct {
		Manifest *pluginv1.PluginManifest `json:"manifest"`
		RepoURL  string                   `json:"repo_url"`
		Binaries map[string]struct {
			URL      string `json:"url"`
			Checksum string `json:"checksum"`
		} `json:"binaries"`
	} `json:"plugins"`
}

func TestIndexDecodesAsSiloReadsIt(t *testing.T) {
	dist := t.TempDir()
	for _, n := range []string{"plugin-linux-amd64", "plugin-linux-arm64", "plugin-darwin-arm64"} {
		if err := os.WriteFile(filepath.Join(dist, n), []byte(n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := run(&buf, "1.2.3", dist, "../../manifest.json", false); err != nil {
		t.Fatal(err)
	}
	var idx siloIndex
	if err := json.Unmarshal(buf.Bytes(), &idx); err != nil {
		t.Fatalf("Silo could not decode the index: %v\n%s", err, buf.String())
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("plugins = %d", len(idx.Plugins))
	}
	p := idx.Plugins[0]
	m := p.Manifest
	if m.GetPluginId() != "zorcerer.arrmada" || m.GetVersion() != "1.2.3" || m.GetSiloApiVersion() != "v1" || m.GetChecksum() != "" {
		t.Fatalf("manifest = %v", m)
	}
	// Silo runs the first on every catalog entry; Silo's own catalog tool
	// runs the second before listing a plugin.
	if err := publicmanifest.Validate(m); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := publicmanifest.ValidateCatalogPresentation(m, p.RepoURL); err != nil {
		t.Fatalf("ValidateCatalogPresentation: %v", err)
	}
	types := map[string]bool{}
	for _, c := range m.GetCapabilities() {
		types[c.GetType()] = true
		if c.GetType() == "scan_source.v1" {
			ss := c.GetMetadata().GetFields()["scan_source"].GetStructValue().GetFields()
			if ss["connection"].GetStringValue() != "required" {
				t.Errorf("scan_source metadata lost: %v", c.GetMetadata())
			}
		}
		if c.GetType() == "request_router.v1" {
			fields := c.GetConfigSchema()[0].GetAdminForm().GetFields()
			if len(fields) != 4 || fields[0].GetControl() != pluginv1.AdminFormControl_ADMIN_FORM_CONTROL_SWITCH {
				t.Errorf("connection form lost: %v", fields)
			}
			if !c.GetRequestRouter().GetReportsDownloadProgress() {
				t.Error("request_router descriptor lost")
			}
		}
	}
	if !types["scan_source.v1"] || !types["request_router.v1"] {
		t.Fatalf("capabilities = %v", types)
	}
	b := p.Binaries["linux/amd64"]
	if b.URL != "https://github.com/zorcerer/silo-plugin-arrmada/releases/download/v1.2.3/plugin-linux-amd64" || len(b.Checksum) != 64 {
		t.Fatalf("linux/amd64 = %+v", b)
	}
	if len(p.Binaries) != 3 {
		t.Fatalf("binaries = %v", p.Binaries)
	}
}

func TestIndexRefusesMissingBinariesAndBadVersions(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf, "1.2.3", t.TempDir(), "../../manifest.json", false); err == nil || !strings.Contains(err.Error(), "plugin-linux-amd64") {
		t.Fatalf("missing binaries: %v", err)
	}
	if err := run(&buf, "v1.2", t.TempDir(), "../../manifest.json", false); err == nil {
		t.Fatal("a bad version was accepted")
	}
}
