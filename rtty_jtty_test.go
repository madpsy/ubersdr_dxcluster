package main

// RTTY and JTTY are modes of their own, not part of CW: the skimmer's CW
// stream carries all three, saying which in its mode field, and every filter,
// picker, query and line here must tell them apart.  Spots stored before the
// stream said its mode have none, and are CW.

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func cwStreamSpot(call, mode string) Spot {
	return mkSpot(StreamCWSkimmer, 14085000, call, "EU", "FI", mode, "CQ")
}

func TestParseCWSpotKeepsItsMode(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`"mode":"CW",`, "CW"},
		{`"mode":"RTTY",`, "RTTY"},
		{`"mode":"JTTY",`, "JTTY"},
		{`"mode":"rtty",`, "RTTY"}, // as written, upper-cased
		{``, "CW"},                 // an UberSDR from before the field: CW
	} {
		raw := []byte(`{"type":"cw_spot",` + tc.raw + `"band":"20m","frequency":14085000,"callsign":"OH2BH",
			"spotter":"M9PSY","snr":12,"wpm":45,"timestamp":"2026-10-12T12:00:00Z","comment":"CQ RTTY"}`)
		sp, err := parseCWSpot(raw)
		if err != nil || sp == nil {
			t.Fatalf("%s: %v %v", tc.raw, sp, err)
		}
		if sp.Mode != tc.want || sp.Stream != StreamCWSkimmer {
			t.Errorf("%q: mode %q stream %q, want %q", tc.raw, sp.Mode, sp.Stream, tc.want)
		}
	}
}

// set/filter mode: each of CW, RTTY and JTTY selects itself alone.
func TestSimpleModeFilterSeparatesRTTYAndJTTY(t *testing.T) {
	cw, rtty, jtty := cwStreamSpot("G4ABC", "CW"), cwStreamSpot("OH2BH", "RTTY"), cwStreamSpot("HA5X", "JTTY")
	legacy := cwStreamSpot("DL3CCC", "") // stored before the stream said its mode
	ft8 := mkSpot(StreamDecoder, 14074000, "K1ABC", "NA", "US", "FT8", "")

	for _, tc := range []struct {
		modes []string
		want  map[string]bool
	}{
		{[]string{"RTTY"}, map[string]bool{"OH2BH": true}},
		{[]string{"JTTY"}, map[string]bool{"HA5X": true}},
		{[]string{"CW"}, map[string]bool{"G4ABC": true, "DL3CCC": true}},
		{[]string{"rtty", "JTTY"}, map[string]bool{"OH2BH": true, "HA5X": true}},
		{[]string{"CW", "RTTY", "JTTY"}, map[string]bool{"G4ABC": true, "OH2BH": true, "HA5X": true, "DL3CCC": true}},
		{[]string{"FT8"}, map[string]bool{"K1ABC": true}},
	} {
		f := &ClientFilter{Modes: upperAll(tc.modes)}
		for _, sp := range []Spot{cw, rtty, jtty, legacy, ft8} {
			if got := f.matchSimple(sp); got != tc.want[sp.Callsign] {
				t.Errorf("mode %v on %s (%q): %v, want %v", tc.modes, sp.Callsign, sp.Mode, got, tc.want[sp.Callsign])
			}
		}
	}
}

// accept/reject expressions: "mode RTTY" is RTTY, not CW; "type cw" (the
// stream) still takes all three.
func TestModeExprSeparatesRTTYAndJTTY(t *testing.T) {
	cw, rtty, jtty := cwStreamSpot("G4ABC", "CW"), cwStreamSpot("OH2BH", "RTTY"), cwStreamSpot("HA5X", "JTTY")
	legacy := cwStreamSpot("DL3CCC", "")
	for _, tc := range []struct {
		expr string
		want map[string]bool
	}{
		{"mode RTTY", map[string]bool{"OH2BH": true}},
		{"mode JTTY", map[string]bool{"HA5X": true}},
		{"mode CW", map[string]bool{"G4ABC": true, "DL3CCC": true}},
		{"not mode CW", map[string]bool{"OH2BH": true, "HA5X": true}},
		{"mode RTTY or mode JTTY", map[string]bool{"OH2BH": true, "HA5X": true}},
	} {
		n := mustExpr(t, tc.expr)
		for _, sp := range []Spot{cw, rtty, jtty, legacy} {
			if got := n.eval(sp); got != tc.want[sp.Callsign] {
				t.Errorf("%q on %s (%q): %v, want %v", tc.expr, sp.Callsign, sp.Mode, got, tc.want[sp.Callsign])
			}
		}
	}
}

// The mode pickers (web search and stats, from ModeGroups) offer RTTY and JTTY
// as modes of their own under the CW source.
func TestModeGroupsOfferRTTYAndJTTY(t *testing.T) {
	var cw []string
	for _, g := range ModeGroups() {
		if g["stream"] == string(StreamCWSkimmer) {
			cw, _ = g["modes"].([]string)
		}
	}
	if strings.Join(cw, " ") != "CW RTTY JTTY" {
		t.Errorf("CW source modes = %v, want CW RTTY JTTY", cw)
	}
}

// storeWithCWStreamModes is the stats fixture plus an RTTY and a JTTY spot,
// and a CW one with its mode stored.
func storeWithCWStreamModes(t *testing.T) *SpotStore {
	t.Helper()
	s := newTestStore(t)
	at := time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour).Add(20 * time.Hour)
	extra := []Spot{
		{Stream: StreamCWSkimmer, Timestamp: at, Band: "20m", Callsign: "OH2BH", FreqHz: 14085000, SNR: 12, Mode: "RTTY", WPM: 45, Spotter: "M9PSY", CountryCode: "FI", Continent: "EU"},
		{Stream: StreamCWSkimmer, Timestamp: at, Band: "20m", Callsign: "HA5X", FreqHz: 14079500, SNR: 8, Mode: "JTTY", WPM: 50, Spotter: "M9PSY", CountryCode: "HU", Continent: "EU"},
		{Stream: StreamCWSkimmer, Timestamp: at, Band: "20m", Callsign: "G4ABC", FreqHz: 14025000, SNR: 20, Mode: "CW", WPM: 25, Spotter: "M9PSY", CountryCode: "GB", Continent: "EU"},
	}
	if err := s.insertBatch(extra); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
	return s
}

// The database: RTTY and JTTY stored as themselves, counted and searched
// apart from CW; the CW source still holds all three, and a CW row from
// before the stream said its mode still counts as CW.
func TestStatsSeparateRTTYAndJTTY(t *testing.T) {
	s := storeWithCWStreamModes(t)
	count := func(q url.Values) int64 {
		t.Helper()
		sum, err := s.Summary(dayFilter(t, q))
		if err != nil {
			t.Fatalf("Summary(%v): %v", q, err)
		}
		return sum["spots"].(int64)
	}
	if n := count(url.Values{"mode": {"RTTY"}}); n != 1 {
		t.Errorf("mode=RTTY: %d spots, want 1", n)
	}
	if n := count(url.Values{"mode": {"JTTY"}}); n != 1 {
		t.Errorf("mode=JTTY: %d spots, want 1", n)
	}
	if n := count(url.Values{"mode": {"CW"}}); n != 2 { // G4ABC, and the fixture's mode-less DL3CCC
		t.Errorf("mode=CW: %d spots, want 2 (RTTY and JTTY are not CW)", n)
	}
	if n := count(url.Values{"stream": {"cwskimmer"}}); n != 4 {
		t.Errorf("source=CW: %d spots, want 4 (all three modes)", n)
	}

	rows, err := s.Breakdown(dayFilter(t, nil), "mode", "count", 20)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Key] = r.Count
	}
	if got["RTTY"] != 1 || got["JTTY"] != 1 || got["CW"] != 2 {
		t.Errorf("mode groups = %v, want RTTY 1, JTTY 1, CW 2", got)
	}
}

// show/dx's mode filter: the same, against the store directly.
func TestShowDXModeSeparatesRTTYAndJTTY(t *testing.T) {
	s := storeWithCWStreamModes(t)
	calls := func(mode string) string {
		t.Helper()
		spots, err := s.Query(ShowDXParams{Limit: 50, DayFrom: 2, Mode: mode})
		if err != nil {
			t.Fatalf("Query(mode %s): %v", mode, err)
		}
		var out []string
		for _, sp := range spots {
			out = append(out, sp.Callsign)
		}
		return strings.Join(sortedCopy(out), " ")
	}
	if got := calls("RTTY"); got != "OH2BH" {
		t.Errorf("show/dx mode RTTY = %q", got)
	}
	if got := calls("jtty"); got != "HA5X" {
		t.Errorf("show/dx mode JTTY = %q", got)
	}
	if got := calls("CW"); got != "DL3CCC G4ABC" {
		t.Errorf("show/dx mode CW = %q, want DL3CCC G4ABC (RTTY and JTTY not among them)", got)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// The telnet line: RTTY's and JTTY's speed in BPS, as CW Skimmer Server
// writes it; CW's in WPM.
func TestDXClusterLineSpeedUnit(t *testing.T) {
	for _, tc := range []struct{ mode, unit string }{{"CW", "WPM"}, {"", "WPM"}, {"RTTY", "BPS"}, {"JTTY", "BPS"}} {
		sp := cwStreamSpot("OH2BH", tc.mode)
		sp.WPM, sp.SNR, sp.Timestamp = 45, 12, time.Date(2026, 10, 12, 17, 2, 0, 0, time.UTC)
		line := sp.FormatDXCluster("M9PSY")
		if !strings.Contains(line, "45 "+tc.unit) {
			t.Errorf("%q: %q, want 45 %s", tc.mode, line, tc.unit)
		}
	}
}

// The help text (telnet HELP, the web UI's and the desktop client's) names
// RTTY and JTTY as modes to filter on, and the client's copy matches.
func TestHelpNamesRTTYAndJTTY(t *testing.T) {
	if !regexp.MustCompile(`CW:\s+CW RTTY JTTY`).MatchString(helpText) {
		t.Error("help's mode list does not name RTTY and JTTY")
	}
	client, err := os.ReadFile("client/helptext.go")
	if err != nil {
		t.Skipf("client/helptext.go: %v", err)
	}
	server, _ := os.ReadFile("helptext.go")
	if string(client) != string(server) {
		t.Error("client/helptext.go differs from helptext.go")
	}
}

// The web UI: an RTTY and a JTTY checkbox of their own, each mapped to its
// mode, reset with the rest, and the CW stream's rows keeping their mode
// rather than all being CW.
func TestWebUIHasRTTYAndJTTYCheckboxes(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, j := string(html), string(js)

	// Every mode checkbox in the page is mapped to a mode and reset by
	// clearAllFilters, RTTY and JTTY among them.
	ids := regexp.MustCompile(`id="(f-mode-[a-z0-9]+)"`).FindAllStringSubmatch(h, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		id := m[1]
		seen[id] = true
		if !strings.Contains(j, "'"+id+"':") {
			t.Errorf("%s is not in getCheckedModes' map", id)
		}
		if !regexp.MustCompile(`\[[^\]]*'` + id + `'[^\]]*\]\.forEach`).MatchString(j) {
			t.Errorf("%s is not reset by clearAllFilters", id)
		}
	}
	for _, id := range []string{"f-mode-rtty", "f-mode-jtty"} {
		if !seen[id] {
			t.Errorf("no %s checkbox", id)
		}
	}
	if !strings.Contains(j, `'f-mode-rtty': 'RTTY'`) || !strings.Contains(j, `'f-mode-jtty': 'JTTY'`) {
		t.Error("the RTTY and JTTY checkboxes are not mapped to their modes")
	}
	if !strings.Contains(j, `cwskimmer: ['CW','RTTY','JTTY']`) {
		t.Error("STREAM_MODES does not give the CW stream RTTY and JTTY")
	}
	// The rows take the spot's own mode, CW only when it has none.
	if strings.Contains(j, `if (spot.stream === 'cwskimmer')  mode = 'CW';`) || strings.Contains(j, `mode:          'CW',`) {
		t.Error("CW stream rows are still forced to mode CW")
	}
}
