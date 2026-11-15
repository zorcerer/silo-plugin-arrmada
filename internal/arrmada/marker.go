package arrmada

import (
	"fmt"
	"strconv"
	"strings"
)

// Marker is the newest history event already handled, per kind. Arrmada keeps
// each kind's events in a table of its own (movie_events, series_events), and
// a row id there only grows, so "id above the marker" is "new since the last
// poll" without comparing clocks.
type Marker struct {
	Movie  int64
	Series int64
}

const markerPrefix = "v1"

// String is the form Silo stores: v1;m=<id>;s=<id>.
func (m Marker) String() string {
	return fmt.Sprintf("%s;m=%d;s=%d", markerPrefix, m.Movie, m.Series)
}

// ParseMarker reads a stored marker. The empty string is a first poll, which
// ok=false reports.
func ParseMarker(s string) (m Marker, ok bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Marker{}, false, nil
	}
	parts := strings.Split(s, ";")
	if len(parts) != 3 || parts[0] != markerPrefix {
		return Marker{}, false, fmt.Errorf("marker %q is not one this plugin wrote", s)
	}
	for _, part := range parts[1:] {
		key, value, found := strings.Cut(part, "=")
		n, perr := strconv.ParseInt(value, 10, 64)
		if !found || perr != nil || n < 0 {
			return Marker{}, false, fmt.Errorf("marker %q is not one this plugin wrote", s)
		}
		switch key {
		case "m":
			m.Movie = n
		case "s":
			m.Series = n
		default:
			return Marker{}, false, fmt.Errorf("marker %q is not one this plugin wrote", s)
		}
	}
	return m, true, nil
}

func (m Marker) get(kind string) int64 {
	if kind == kindMovie {
		return m.Movie
	}
	return m.Series
}

func (m *Marker) set(kind string, id int64) {
	if kind == kindMovie {
		m.Movie = id
	} else {
		m.Series = id
	}
}
