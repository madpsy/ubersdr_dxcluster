package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StreamType identifies which upstream SSE stream a spot came from.
type StreamType string

const (
	StreamDecoder       StreamType = "decoder"
	StreamCWSkimmer     StreamType = "cwskimmer"
	StreamVoiceActivity StreamType = "voice"
	StreamDXCluster     StreamType = "dxcluster"
)

// Spot is the unified internal representation of any incoming event.
// Fields are a superset of all three stream types; unused fields are zero.
type Spot struct {
	// Common
	Stream    StreamType `json:"stream"`
	Timestamp time.Time  `json:"timestamp"`
	Band      string     `json:"band"`
	Callsign  string     `json:"callsign"` // dx_callsign for voice; "N0CALL" if absent
	FreqHz    float64    `json:"freq_hz"`
	SNR       float64    `json:"snr"`
	Country   string     `json:"country,omitempty"`
	CountryCode string   `json:"country_code,omitempty"`
	Continent string     `json:"continent,omitempty"`

	// Decoder-specific
	Mode     string `json:"mode,omitempty"`
	Locator  string `json:"locator,omitempty"`
	Message  string `json:"message,omitempty"`

	// CW-specific
	Spotter string `json:"spotter,omitempty"`
	WPM     int    `json:"wpm,omitempty"`
	Comment string `json:"comment,omitempty"`
	CQZone  int    `json:"cq_zone,omitempty"`

	// Voice-specific
	EstDialFreq  int     `json:"est_dial_freq,omitempty"`
	VoiceMode    string  `json:"voice_mode,omitempty"` // USB / LSB
	Confidence   float64 `json:"confidence,omitempty"`
	Bandwidth    int     `json:"bandwidth,omitempty"`
	AvgSignalDB  float64 `json:"avg_signal_db,omitempty"`
	PeakSignalDB float64 `json:"peak_signal_db,omitempty"`

	// Distance / bearing (decoder + CW)
	DistanceKM float64 `json:"distance_km,omitempty"`
	BearingDeg float64 `json:"bearing_deg,omitempty"`
}

// FormatDXCluster returns a standard AR-Cluster/DX Spider spot line.
//
// For our own skimmer spots (digital/CW/voice) the receiver's callsign
// with "-#" suffix is used as the spotter (RBN convention).
//
// For StreamDXCluster spots the original spotter callsign from the upstream
// cluster is preserved, and the comment is passed through as-is:
//
//	DX de DK8MM:    144337.0  DM4KCS/P     JO53 > JO30JM                  1748Z
//	DX de M9PSY-#:   14033.0  R6AU           13 dB  23 WPM  CQ            1701Z
func (s *Spot) FormatDXCluster(defaultSpotter string) string {
	var spotter string
	if s.Stream == StreamDXCluster && s.Spotter != "" {
		// Real DX cluster spot — use the original spotter as-is
		spotter = s.Spotter
	} else {
		// Our own skimmer spots — use receiver callsign with "-#" suffix
		spotter = defaultSpotter + "-#"
	}

	freqKHz := s.FreqHz / 1000.0

	ts := s.Timestamp.UTC()
	timeStr := fmt.Sprintf("%02d%02dZ", ts.Hour(), ts.Minute())

	var comment string
	switch s.Stream {
	case StreamDecoder:
		comment = fmt.Sprintf("%s %d dB", s.Mode, int(s.SNR))
	case StreamCWSkimmer:
		comment = fmt.Sprintf("%2d dB  %2d WPM  %-13s", int(s.SNR), s.WPM, s.Comment)
	case StreamVoiceActivity:
		comment = fmt.Sprintf("%s %d dB", s.VoiceMode, int(s.SNR))
	case StreamDXCluster:
		// Strip any trailing HHMMZ timestamp from the comment — the upstream
		// cluster sometimes includes it in the comment field, but we already
		// append our own timestamp at the end of the line.
		comment = stripTrailingTime(s.Comment)
		if comment == "" {
			comment = "DX"
		}
	}

	// Standard DX cluster line format:
	// DX de SPOTTER:  FFFFF.F  CALLSIGN       COMMENT                    HHMMZ
	return fmt.Sprintf("DX de %-12s %8.1f  %-13s  %-29s %s",
		spotter+":", freqKHz, s.Callsign, comment, timeStr)
}

// --- Raw JSON structs for each upstream stream ---

type decoderEvent struct {
	Type        string  `json:"type"`
	Mode        string  `json:"mode"`
	Band        string  `json:"band"`
	Callsign    string  `json:"callsign"`
	Frequency   int64   `json:"frequency"`
	SNR         int     `json:"snr"`
	Timestamp   string  `json:"timestamp"`
	Locator     string  `json:"locator"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Continent   string  `json:"continent"`
	Message     string  `json:"message"`
	DistanceKM  float64 `json:"distance_km"`
	BearingDeg  float64 `json:"bearing_deg"`
}

type cwSpotEvent struct {
	Type        string  `json:"type"`
	Band        string  `json:"band"`
	Frequency   float64 `json:"frequency"`
	Callsign    string  `json:"callsign"`
	Spotter     string  `json:"spotter"`
	SNR         int     `json:"snr"`
	WPM         int     `json:"wpm"`
	Timestamp   string  `json:"timestamp"`
	Comment     string  `json:"comment"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Continent   string  `json:"continent"`
	CQZone      int     `json:"cq_zone"`
	DistanceKM  float64 `json:"distance_km"`
	BearingDeg  float64 `json:"bearing_deg"`
}

type voiceActivityEvent struct {
	Type            string  `json:"type"`
	Band            string  `json:"band"`
	Timestamp       string  `json:"timestamp"`
	EstimatedDialFreq int   `json:"estimated_dial_freq"`
	Mode            string  `json:"mode"`
	SNR             float64 `json:"snr"`
	Confidence      float64 `json:"confidence"`
	AvgSignalDB     float64 `json:"avg_signal_db"`
	PeakSignalDB    float64 `json:"peak_signal_db"`
	Bandwidth       int     `json:"bandwidth"`
	DXCallsign      string  `json:"dx_callsign"`
	DXCountry       string  `json:"dx_country"`
	DXCountryCode   string  `json:"dx_country_code"`
	DXContinent     string  `json:"dx_continent"`
}

// stripTrailingTime removes a trailing HHMMZ time token from a comment string.
// e.g. "SR-13 SR-42 1748Z" → "SR-13 SR-42"
// Some upstream DX clusters include the time in the comment field; we strip it
// because we already append our own timestamp at the end of the telnet line.
func stripTrailingTime(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 5 {
		tail := s[len(s)-5:]
		if tail[4] == 'Z' &&
			tail[0] >= '0' && tail[0] <= '2' &&
			tail[1] >= '0' && tail[1] <= '9' &&
			tail[2] >= '0' && tail[2] <= '5' &&
			tail[3] >= '0' && tail[3] <= '9' {
			s = strings.TrimSpace(s[:len(s)-5])
		}
	}
	return s
}

// dxSpotEvent is the raw JSON from /api/dxcluster/stream.
type dxSpotEvent struct {
	Type        string  `json:"type"`
	Frequency   float64 `json:"frequency"`
	DXCall      string  `json:"dx_call"`
	Spotter     string  `json:"spotter"`
	Band        string  `json:"band"`
	Timestamp   string  `json:"timestamp"`
	Comment     string  `json:"comment"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Continent   string  `json:"continent"`
	TimeOffset  float64 `json:"time_offset"`
}

// parseDXSpot converts a raw DX cluster JSON data line into a Spot.
func parseDXSpot(data []byte) (*Spot, error) {
	var e dxSpotEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Type != "dx_spot" {
		return nil, nil
	}
	ts, _ := time.Parse(time.RFC3339, e.Timestamp)
	return &Spot{
		Stream:      StreamDXCluster,
		Timestamp:   ts,
		Band:        e.Band,
		Callsign:    e.DXCall,
		FreqHz:      e.Frequency,
		Spotter:     e.Spotter,
		Comment:     e.Comment,
		Country:     e.Country,
		CountryCode: e.CountryCode,
		Continent:   e.Continent,
	}, nil
}

// parseDecoder converts a raw decoder JSON data line into a Spot.
func parseDecoder(data []byte) (*Spot, error) {
	var e decoderEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Type != "decode" {
		return nil, nil
	}
	ts, _ := time.Parse(time.RFC3339, e.Timestamp)
	return &Spot{
		Stream:      StreamDecoder,
		Timestamp:   ts,
		Band:        e.Band,
		Callsign:    e.Callsign,
		FreqHz:      float64(e.Frequency),
		SNR:         float64(e.SNR),
		Mode:        e.Mode,
		Locator:     e.Locator,
		Country:     e.Country,
		CountryCode: e.CountryCode,
		Continent:   e.Continent,
		Message:     e.Message,
		DistanceKM:  e.DistanceKM,
		BearingDeg:  e.BearingDeg,
	}, nil
}

// parseCWSpot converts a raw CW skimmer JSON data line into a Spot.
func parseCWSpot(data []byte) (*Spot, error) {
	var e cwSpotEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Type != "cw_spot" {
		return nil, nil
	}
	ts, _ := time.Parse(time.RFC3339, e.Timestamp)
	return &Spot{
		Stream:      StreamCWSkimmer,
		Timestamp:   ts,
		Band:        e.Band,
		Callsign:    e.Callsign,
		FreqHz:      e.Frequency,
		SNR:         float64(e.SNR),
		Spotter:     e.Spotter,
		WPM:         e.WPM,
		Comment:     e.Comment,
		Country:     e.Country,
		CountryCode: e.CountryCode,
		Continent:   e.Continent,
		CQZone:      e.CQZone,
		DistanceKM:  e.DistanceKM,
		BearingDeg:  e.BearingDeg,
	}, nil
}

// parseVoiceActivity converts a raw voice activity JSON data line into a Spot.
func parseVoiceActivity(data []byte) (*Spot, error) {
	var e voiceActivityEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Type != "voice_activity" {
		return nil, nil
	}
	ts, _ := time.Parse(time.RFC3339, e.Timestamp)

	callsign := e.DXCallsign
	if callsign == "" {
		callsign = "N0CALL"
	}

	return &Spot{
		Stream:       StreamVoiceActivity,
		Timestamp:    ts,
		Band:         e.Band,
		Callsign:     callsign,
		FreqHz:       float64(e.EstimatedDialFreq),
		SNR:          e.SNR,
		VoiceMode:    e.Mode,
		Confidence:   e.Confidence,
		Bandwidth:    e.Bandwidth,
		AvgSignalDB:  e.AvgSignalDB,
		PeakSignalDB: e.PeakSignalDB,
		EstDialFreq:  e.EstimatedDialFreq,
		Country:      e.DXCountry,
		CountryCode:  e.DXCountryCode,
		Continent:    e.DXContinent,
	}, nil
}
