package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeCTY serves /api/cty/lookup: callsigns starting with "G" or "DL" resolve,
// anything else is a 404.
func fakeCTY(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.URL.Query().Get("callsign")
		var country, code, cont string
		switch {
		case strings.HasPrefix(call, "DL"):
			country, code, cont = "Fed. Rep. of Germany", "DE", "EU"
		case strings.HasPrefix(call, "G"):
			country, code, cont = "England", "GB", "EU"
		default:
			http.NotFound(w, r)
			return
		}
		data, _ := json.Marshal(ctyLookupResult{Callsign: call, Country: country, CountryCode: code, Continent: cont})
		_ = json.NewEncoder(w).Encode(ctyAPIResponse{Success: true, Data: data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sstvImage(call string, freqHz int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":           "x",
		"sstv_mode":    "Scottie 1",
		"callsign":     call,
		"frequency_hz": freqHz,
		"audio_mode":   "usb",
		"rx_end":       "2026-10-04T12:34:56Z",
		"snr_avg_db":   41.6,
		"snr_series":   []map[string]any{{"t": 1, "snr": 40}},
	})
	return b
}

func TestSSTVParserSpot(t *testing.T) {
	cty := fakeCTY(t)
	parse := newSSTVParser(cty.URL, 0)

	sp, err := parse(sstvImage("dl1abc", 14230000))
	if err != nil || sp == nil {
		t.Fatalf("parse: spot=%v err=%v", sp, err)
	}
	if sp.Stream != StreamSSTV || sp.Callsign != "DL1ABC" || sp.FreqHz != 14230000 ||
		sp.Band != "20m" || sp.Mode != "SSTV" || sp.Comment != "SSTV Scottie 1" ||
		sp.CountryCode != "DE" || sp.Continent != "EU" || sp.SNR != 41.6 {
		t.Fatalf("unexpected spot: %+v", *sp)
	}
	if want := time.Date(2026, 10, 4, 12, 34, 56, 0, time.UTC); !sp.Timestamp.Equal(want) {
		t.Fatalf("timestamp %v, want %v", sp.Timestamp, want)
	}
	line := sp.FormatDXCluster("M9PSY")
	if !strings.HasPrefix(line, "DX de M9PSY-#:") || !strings.Contains(line, "SSTV Scottie 1 41 dB") ||
		!strings.HasSuffix(line, "1234Z") {
		t.Fatalf("telnet line: %q", line)
	}
}

func TestSSTVParserRejects(t *testing.T) {
	cty := fakeCTY(t)
	parse := newSSTVParser(cty.URL, 0)

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"no callsign", sstvImage("", 14230000)},
		{"no frequency", sstvImage("DL1ABC", 0)},
		{"free text", sstvImage("HELLO WORLD", 14230000)},
		{"digits", sstvImage("12345", 14230000)},
		{"no CTY match", sstvImage("ZZ9ZZ", 14230000)},
	} {
		if sp, err := parse(tc.data); err != nil || sp != nil {
			t.Errorf("%s: spot=%v err=%v, want neither", tc.name, sp, err)
		}
	}
}

func TestSSTVParserDedup(t *testing.T) {
	cty := fakeCTY(t)
	parse := newSSTVParser(cty.URL, time.Hour)

	if sp, _ := parse(sstvImage("G4ABC", 14230000)); sp == nil {
		t.Fatal("first picture: no spot")
	}
	if sp, _ := parse(sstvImage("G4ABC", 14233000)); sp != nil {
		t.Fatal("second picture on the same band: spotted again")
	}
	if sp, _ := parse(sstvImage("G4ABC", 7171000)); sp == nil {
		t.Fatal("same call on another band: no spot")
	}
	if sp, _ := parse(sstvImage("G4XYZ", 14230000)); sp == nil {
		t.Fatal("another call on the same band: no spot")
	}
}

// TestSSTVParserCTYDown keeps a well-formed callsign when UberSDR can't be
// asked, rather than losing the spot.
func TestSSTVParserCTYDown(t *testing.T) {
	parse := newSSTVParser("http://127.0.0.1:1", 0)
	sp, err := parse(sstvImage("DL1ABC", 14230000))
	if err != nil || sp == nil {
		t.Fatalf("spot=%v err=%v", sp, err)
	}
	if sp.Country != "" {
		t.Fatalf("country %q, want empty", sp.Country)
	}
}

// TestReadStreamSSTVEvents feeds readStream the mix the SSTV addon's /api/live
// sends — rail thumbnails, SNR ticks, deletes and images — and checks only the
// images become spots.
func TestReadStreamSSTVEvents(t *testing.T) {
	cty := fakeCTY(t)
	big := strings.Repeat("A", 200*1024) // a rail thumbnail past bufio's default limit

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: rail_thumb\ndata: {\"callsign\":\"G0RAIL\",\"frequency_hz\":14230000,\"jpeg_b64\":%q}\n\n", big)
		fmt.Fprintf(w, "event: snr\ndata: {\"callsign\":\"G0SNR\",\"frequency_hz\":14230000}\n\n")
		fmt.Fprintf(w, "event: image\ndata: %s\n\n", sstvImage("G4ABC", 14230000))
		fmt.Fprintf(w, "event: delete\ndata: {\"id\":\"x\"}\n\n")
		fmt.Fprintf(w, "event: image\ndata: %s\n\n", sstvImage("", 14230000))
		fmt.Fprintf(w, "event: image\ndata: %s\n\n", sstvImage("DL1ABC", 7171000))
	}))
	defer feed.Close()

	hub := NewHub(nil)
	ch := make(chan Spot, 16)
	hub.in = ch

	sc := StreamConfig{Name: "sstv", Path: "/api/live", Parse: newSSTVParser(cty.URL, 0),
		Stream: StreamSSTV, BaseURL: feed.URL, Event: "image"}
	if err := readStream(context.Background(), feed.URL+sc.Path, sc, hub); err != nil {
		t.Fatalf("readStream: %v", err)
	}
	close(ch)

	var got []string
	for sp := range ch {
		got = append(got, sp.Callsign)
	}
	if strings.Join(got, ",") != "G4ABC,DL1ABC" {
		t.Fatalf("spots %v, want [G4ABC DL1ABC]", got)
	}
}

func TestReceiverHasAddon(t *testing.T) {
	rx := ReceiverInfo{Addons: []string{"hfdl", "sstv", "dxcluster"}}
	if !rx.HasAddon("sstv") || rx.HasAddon("navtex") || (ReceiverInfo{}).HasAddon("sstv") {
		t.Fatal("HasAddon wrong")
	}
}
