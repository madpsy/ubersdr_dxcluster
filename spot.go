package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// StreamType identifies which upstream SSE stream a spot came from.
type StreamType string

const (
	StreamDecoder      StreamType = "decoder"
	StreamCWSkimmer    StreamType = "cwskimmer"
	StreamVoiceActivity StreamType = "voice"
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

// FormatDXCluster returns a standard AR-Cluster/DX Spider spot line:
//
//	DX de <spotter>:  <freq kHz>  <callsign>    <comment>    <HHMM>Z <DD> <Mon>
func (s *Spot) FormatDXCluster(defaultSpotter string) string {
	spotter := defaultSpotter
	if s.Spotter != "" {
		spotter = s.Spotter
	}

	freqKHz := s.FreqHz / 1000.0

	var comment string
	switch s.Stream {
	case StreamDecoder:
		comment = fmt.Sprintf("%s %ddB", s.Mode, int(s.SNR))
	case StreamCWSkimmer:
		comment = fmt.Sprintf("CQ %dwpm %ddB", s.WPM, int(s.SNR))
		if s.Comment != "" {
			comment = fmt.Sprintf("%s %dwpm %ddB", s.Comment, s.WPM, int(s.SNR))
		}
	case StreamVoiceActivity:
		comment = fmt.Sprintf("%s SNR%.1f conf%.2f", s.VoiceMode, s.SNR, s.Confidence)
	}

	ts := s.Timestamp.UTC()
	timeStr := fmt.Sprintf("%02d%02dZ %02d %s",
		ts.Hour(), ts.Minute(),
		ts.Day(), ts.Format("Jan"))

	// Standard DX cluster line format (80 chars wide):
	// DX de SPOTTER:   FFFFF.F  CALLSIGN      COMMENT                HHMMZ DD Mon
	return fmt.Sprintf("DX de %-9s %8.1f  %-12s  %-22s %s",
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
