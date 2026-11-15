package main

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedManifestLoads(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.GetPluginId() != "zorcerer.arrmada" {
		t.Fatalf("plugin_id = %q", m.GetPluginId())
	}
	got := map[string]string{}
	for _, c := range m.GetCapabilities() {
		got[c.GetType()] = c.GetId()
	}
	want := map[string]string{"scan_source.v1": "autoscan", "request_router.v1": "requests"}
	if len(got) != len(want) || got["scan_source.v1"] != want["scan_source.v1"] || got["request_router.v1"] != want["request_router.v1"] {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
	for _, c := range m.GetCapabilities() {
		if c.GetType() == "request_router.v1" && !c.GetRequestRouter().GetReportsDownloadProgress() {
			t.Error("request router does not declare reports_download_progress")
		}
	}
	if len(m.GetChecksum()) != 64 {
		t.Fatalf("checksum = %q", m.GetChecksum())
	}
}

// Silo builds the Add-source flow from this block without starting the
// plugin, and reads a value of the wrong type as absent.
func TestScanSourceDescriptor(t *testing.T) {
	var raw struct {
		Capabilities []struct {
			Metadata struct {
				ScanSource struct {
					DeliveryModes   []string `json:"delivery_modes"`
					Connection      string   `json:"connection"`
					ConnectionKinds []string `json:"connection_kinds"`
				} `json:"scan_source"`
			} `json:"metadata"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(manifestJSON, &raw); err != nil {
		t.Fatal(err)
	}
	d := raw.Capabilities[0].Metadata.ScanSource // the scan source is listed first
	if d.Connection != "required" {
		t.Errorf("connection = %q, want required", d.Connection)
	}
	if len(d.DeliveryModes) != 1 || d.DeliveryModes[0] != "poll" {
		t.Errorf("delivery_modes = %v", d.DeliveryModes)
	}
	// Silo's connection form offers only the Sonarr and Radarr kinds, and an
	// Arrmada connection is stored as one of them. Restricting the kinds
	// would hide those connections or let the wrong ones through.
	if len(d.ConnectionKinds) != 0 {
		t.Errorf("connection_kinds = %v, want none", d.ConnectionKinds)
	}
}
