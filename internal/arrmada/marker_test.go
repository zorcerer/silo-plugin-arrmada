package arrmada

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	m := Marker{Movie: 42, Series: 7}
	got, ok, err := ParseMarker(m.String())
	if err != nil || !ok || got != m {
		t.Fatalf("ParseMarker(%q) = %+v, %v, %v", m.String(), got, ok, err)
	}
}

func TestParseMarkerEmptyIsFirstPoll(t *testing.T) {
	_, ok, err := ParseMarker("  ")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want a first poll", ok, err)
	}
}

func TestParseMarkerRejectsForeignMarkers(t *testing.T) {
	for _, s := range []string{
		"2026-11-15T10:00:00Z|12", // the Sonarr/Radarr plugin's form
		"v1;m=1",
		"v1;m=1;x=2",
		"v1;m=-1;s=2",
		"v2;m=1;s=2",
	} {
		if _, _, err := ParseMarker(s); err == nil {
			t.Errorf("ParseMarker(%q) accepted it", s)
		}
	}
}
