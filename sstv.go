package main

// sstv.go — spots from the SSTV addon (ubersdr_qsstv).
//
// The SSTV addon publishes every saved picture as an "image" event on its
// public GET /api/live SSE feed. A picture whose sender appended an FSK ID
// carries the decoded callsign; those become SSTV spots on the channel's dial
// frequency. Pictures without a callsign are ignored.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// sstvImageEvent is the subset of the SSTV addon's image record we use.
type sstvImageEvent struct {
	SSTVMode    string    `json:"sstv_mode"`
	Callsign    string    `json:"callsign"`
	FrequencyHz int64     `json:"frequency_hz"`
	AudioMode   string    `json:"audio_mode"`
	RxEnd       time.Time `json:"rx_end"`
	SNRAvgDB    float64   `json:"snr_avg_db"`
}

// newSSTVParser returns the Parse function for the SSTV stream.
//
// A station usually sends several pictures in a row, each with its FSK ID, so
// a callsign is spotted at most once per band per dedup window. Unlike the DB
// dedup for voice and digital spots this also holds back the live feed: a
// telnet user wants one spot for the station, not one per picture.
//
// The FSK ID is free text the sender types in, so anything that is not a
// callsign, or that UberSDR's CTY database cannot place, is dropped.
func newSSTVParser(ubersdrURL string, dedup time.Duration) func([]byte) (*Spot, error) {
	var mu sync.Mutex
	lastSeen := map[string]time.Time{}

	return func(data []byte) (*Spot, error) {
		var e sstvImageEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		call := strings.ToUpper(strings.TrimSpace(e.Callsign))
		if call == "" || e.FrequencyHz <= 0 {
			return nil, nil
		}
		if !isValidCallsign(call) {
			log.Printf("[sstv] ignoring FSK ID %q: not a callsign", call)
			return nil, nil
		}

		freqHz := float64(e.FrequencyHz)
		band := bandForSpot(freqHz)

		if dedup > 0 {
			key := call + "|" + band
			now := time.Now()
			mu.Lock()
			if last, ok := lastSeen[key]; ok && now.Sub(last) < dedup {
				mu.Unlock()
				return nil, nil
			}
			lastSeen[key] = now
			for k, t := range lastSeen {
				if now.Sub(t) >= dedup {
					delete(lastSeen, k)
				}
			}
			mu.Unlock()
		}

		var country, countryCode, continent string
		r, err := lookupCTY(ubersdrURL, call)
		switch {
		case err != nil:
			// UberSDR unreachable or its CTY not loaded: the callsign already
			// passed the format check, so spot it without a country rather than
			// lose it.
			log.Printf("[sstv] cty lookup for %s: %v (country will be empty)", call, err)
		case r == nil:
			log.Printf("[sstv] ignoring FSK ID %q: no CTY match", call)
			return nil, nil
		default:
			country, countryCode, continent = r.Country, r.CountryCode, r.Continent
		}

		ts := e.RxEnd.UTC()
		if ts.IsZero() {
			ts = time.Now().UTC()
		}

		comment := "SSTV"
		if e.SSTVMode != "" {
			comment = fmt.Sprintf("SSTV %s", e.SSTVMode)
		}

		log.Printf("[sstv] spot: %s on %.1f kHz (%s, %.0f dB)", call, freqHz/1000, comment, e.SNRAvgDB)

		return &Spot{
			Stream:      StreamSSTV,
			Timestamp:   ts,
			Band:        band,
			Callsign:    call,
			FreqHz:      freqHz,
			SNR:         e.SNRAvgDB,
			Mode:        "SSTV",
			Comment:     comment,
			Country:     country,
			CountryCode: countryCode,
			Continent:   continent,
		}, nil
	}
}
