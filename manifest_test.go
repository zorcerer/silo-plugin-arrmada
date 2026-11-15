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
	if m.GetPluginId() != "zorcerer.autoscan.arrmada" {
		t.Fatalf("plugin_id = %q", m.GetPluginId())
	}
	caps := m.GetCapabilities()
	if len(caps) != 1 || caps[0].GetType() != "scan_source.v1" || caps[0].GetId() != "arrmada" {
		t.Fatalf("capabilities = %+v", caps)
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
	d := raw.Capabilities[0].Metadata.ScanSource
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
