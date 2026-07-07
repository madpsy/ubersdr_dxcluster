package main

import (
	"math"
	"strings"
	"testing"
)

// TestBearingDistance sanity-checks the great-circle math used by show/heading.
func TestBearingDistance(t *testing.T) {
	// London (51.5, -0.13) to New York (40.71, -74.0): ~5570 km, bearing ~288°
	b, d := bearingDistance(51.5, -0.13, 40.71, -74.0)
	if math.Abs(d-5570) > 100 {
		t.Errorf("London→NY distance: got %.0f km, want ~5570", d)
	}
	if b < 280 || b > 300 {
		t.Errorf("London→NY bearing: got %.0f°, want ~288°", b)
	}
	// Zero distance to self
	_, d0 := bearingDistance(51.5, -0.13, 51.5, -0.13)
	if d0 > 1 {
		t.Errorf("self distance: got %.2f km, want ~0", d0)
	}
}

// TestLatLonFormat checks the Spider-style lat/lon formatters.
func TestLatLonFormat(t *testing.T) {
	if got := slat(52.0); got != "52 N" {
		t.Errorf("slat(52) = %q, want %q", got, "52 N")
	}
	if got := slat(-33.0); got != "33 S" {
		t.Errorf("slat(-33) = %q, want %q", got, "33 S")
	}
	if got := slong(-0.1); !strings.HasSuffix(got, "W") {
		t.Errorf("slong(-0.1) = %q, want W suffix", got)
	}
	if got := slong(139.0); got != "139 E" {
		t.Errorf("slong(139) = %q, want %q", got, "139 E")
	}
}

// mkSpot builds a Spot with common fields for filter testing.
func mkSpot(stream StreamType, freqHz float64, call, cont, cc, mode, comment string) Spot {
	return Spot{
		Stream:      stream,
		FreqHz:      freqHz,
		Callsign:    call,
		Continent:   cont,
		CountryCode: cc,
		Mode:        mode,
		Comment:     comment,
		Band:        freqToBand(freqHz),
	}
}

func mustExpr(t *testing.T, s string) *exprNode {
	t.Helper()
	n, err := parseFilterExpr(s)
	if err != nil {
		t.Fatalf("parseFilterExpr(%q) error: %v", s, err)
	}
	return n
}

// TestDecodeFreqTerm checks band/region/segment/range decoding.
func TestDecodeFreqTerm(t *testing.T) {
	cases := []struct {
		term    string
		freqKHz float64
		want    bool
	}{
		{"20m", 14020, true},
		{"20m", 7000, false},
		{"hf", 14020, true},
		{"hf", 50000, false},
		{"hf/cw", 14020, true},  // 14000-14070 CW segment
		{"hf/cw", 14200, false}, // above CW segment
		{"20m/cw", 14020, true},
		{"20m/ssb", 14200, true},
		{"20m/ssb", 14020, false},
		{"14000/14070", 14050, true},
		{"14000/14070", 14100, false},
	}
	for _, c := range cases {
		frs := decodeFreqTerm(c.term)
		if frs == nil {
			t.Errorf("decodeFreqTerm(%q) returned nil", c.term)
			continue
		}
		khz := c.freqKHz
		matched := false
		for _, fr := range frs {
			if khz >= fr.Min && khz <= fr.Max {
				matched = true
				break
			}
		}
		if matched != c.want {
			t.Errorf("decodeFreqTerm(%q) freq %g kHz: got match=%v want %v", c.term, c.freqKHz, matched, c.want)
		}
	}
}

// TestSimpleExpr checks single-field expressions.
func TestSimpleExpr(t *testing.T) {
	dl20 := mkSpot(StreamDXCluster, 14020000, "DL1ABC", "EU", "DE", "SSB", "CQ DX")
	w40 := mkSpot(StreamDXCluster, 7050000, "W1AW", "NA", "K", "SSB", "TEST")

	cases := []struct {
		expr string
		spot Spot
		want bool
	}{
		{"on 20m", dl20, true},
		{"on 20m", w40, false},
		{"call DL", dl20, true},
		{"call DL", w40, false},
		{"cont EU", dl20, true},
		{"cont NA", dl20, false},
		{"country DE", dl20, true},
		{"info CQ", dl20, true},
		{"info XYZZY", dl20, false},
	}
	for _, c := range cases {
		n := mustExpr(t, c.expr)
		if got := n.eval(c.spot); got != c.want {
			t.Errorf("expr %q on spot %s: got %v want %v", c.expr, c.spot.Callsign, got, c.want)
		}
	}
}

// TestBooleanExpr checks and/or/not and parentheses.
func TestBooleanExpr(t *testing.T) {
	dlEU := mkSpot(StreamDXCluster, 14020000, "DL1ABC", "EU", "DE", "SSB", "iota EU-005")
	wNA := mkSpot(StreamDXCluster, 14020000, "W1AW", "NA", "K", "SSB", "test")
	dlCW := mkSpot(StreamCWSkimmer, 14020000, "DL1ABC", "EU", "DE", "", "")

	cases := []struct {
		expr string
		spot Spot
		want bool
	}{
		{"on 20m and cont EU", dlEU, true},
		{"on 20m and cont EU", wNA, false},
		{"cont EU or cont NA", dlEU, true},
		{"cont EU or cont NA", wNA, true},
		{"on 20m and (cont EU or cont NA)", wNA, true},
		{"on 40m and (cont EU or cont NA)", wNA, false},
		{"not cont EU", wNA, true},
		{"not cont EU", dlEU, false},
		{"info iota", dlEU, true},
		{"not on hf/cw or info iota", dlEU, true},  // dlEU is SSB, not CW → not(cw)=true
		{"not on hf/cw or info iota", dlCW, false}, // dlCW is CW, no iota → not(cw)=false, no iota=false
	}
	for _, c := range cases {
		n := mustExpr(t, c.expr)
		if got := n.eval(c.spot); got != c.want {
			t.Errorf("expr %q on spot %s (%s): got %v want %v", c.expr, c.spot.Callsign, c.spot.Stream, got, c.want)
		}
	}
}

// TestSlotSemantics verifies Spider's reject-then-accept slot evaluation.
func TestSlotSemantics(t *testing.T) {
	hfcw := mkSpot(StreamCWSkimmer, 14020000, "DL1ABC", "EU", "DE", "", "")
	hfssb := mkSpot(StreamDXCluster, 14200000, "DL1ABC", "EU", "DE", "SSB", "CQ")
	sixm := mkSpot(StreamDXCluster, 50120000, "DL1ABC", "EU", "DE", "SSB", "CQ")

	// Reject-only filter: reject hf/cw → everything passes except HF CW
	t.Run("reject only", func(t *testing.T) {
		f := &ClientFilter{}
		f.Slots[1] = &FilterSlot{Reject: mustExpr(t, "on hf/cw")}
		if f.Match(hfcw) {
			t.Error("hf/cw spot should be rejected")
		}
		if !f.Match(hfssb) {
			t.Error("hf/ssb spot should pass a reject-hf/cw filter")
		}
	})

	// Accept-only filter: accept 20m → only 20m passes
	t.Run("accept only", func(t *testing.T) {
		f := &ClientFilter{}
		f.Slots[1] = &FilterSlot{Accept: mustExpr(t, "on 20m")}
		if !f.Match(hfssb) {
			t.Error("20m spot should pass accept-20m filter")
		}
		if f.Match(sixm) {
			t.Error("6m spot should NOT pass accept-20m filter")
		}
	})

	// Mixed multi-slot (from Spider docs):
	//   reject/spot 1 on hf/cw
	//   accept/spot 1 on 0/30000    (all HF)
	//   accept/spot 2 on 50000/1400000 (VHF+)
	// hf/cw → rejected; hf/ssb → passes accept slot 1; 6m → passes accept slot 2
	t.Run("mixed multi-slot", func(t *testing.T) {
		f := &ClientFilter{}
		f.Slots[1] = &FilterSlot{
			Reject: mustExpr(t, "on hf/cw"),
			Accept: mustExpr(t, "on 0/30000"),
		}
		f.Slots[2] = &FilterSlot{
			Accept: mustExpr(t, "on 50000/1400000"),
		}
		if f.Match(hfcw) {
			t.Error("hf/cw should be rejected by slot 1 reject")
		}
		if !f.Match(hfssb) {
			t.Error("hf/ssb should be accepted by slot 1 accept")
		}
		if !f.Match(sixm) {
			t.Error("6m should be accepted by slot 2 accept")
		}
	})
}
