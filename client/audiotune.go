package main

// audiotune.go — spot-line parser and HTTP tuner for the UberSDR audio client.
//
// When the user double-clicks a spot line in the terminal, this file:
//  1. Parses the frequency (kHz) and comment from the fixed-width DX cluster line.
//  2. Deduces the SDR mode from the comment (CW→cwu/cwl, voice→usb/lsb,
//     digital→skip, DX cluster→band-plan fallback).
//  3. Fires a PUT /api/v1/tune to the local ubersdr-audio HTTP API.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// spotLineRe matches the fixed-width DX cluster line produced by FormatDXCluster:
//
//	DX de SPOTTER:   14033.0  R6AU           13 dB  23 WPM  CQ            1701Z
//
// Capture groups:
//
//	1 — frequency in kHz  (e.g. "14033.0")
//	2 — comment field     (e.g. "13 dB  23 WPM  CQ")
var spotLineRe = regexp.MustCompile(`^DX de \S+\s+([\d.]+)\s+\S+\s{2,}(.+?)\s+\d{4}Z\s*$`)

// digitalCommentRe matches decoder spot comments of the form "<MODE> <N> dB",
// e.g. "FT8 13 dB", "FT2 7 dB", "WSPR -24 dB".
// This structure is unique to StreamDecoder spots and catches any future mode
// names without needing an explicit list.
var digitalCommentRe = regexp.MustCompile(`(?i)^\S+\s+-?\d+\s+dB`)

// ParseSpotLine extracts the frequency in Hz and the SDR mode from a DX cluster
// telnet line. Returns (freqHz, mode, true) on success, or (0, "", false) if the
// line is not a spot or the mode indicates no tuning should happen (digital spots).
func ParseSpotLine(line string) (freqHz int, mode string, ok bool) {
	m := spotLineRe.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		return 0, "", false
	}
	freqKHz, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, "", false
	}
	comment := strings.TrimSpace(m[2])
	mode = modeFromSpotLine(freqKHz, comment)
	if mode == "" {
		return 0, "", false // digital spot — skip
	}
	return int(freqKHz * 1000), mode, true
}

// modeFromSpotLine deduces the audio client mode from the spot frequency and
// comment field. Returns "" for digital decoder spots (no tuning desired).
func modeFromSpotLine(freqKHz float64, comment string) string {
	freqHz := freqKHz * 1000

	// CW: "WPM" in comment → cwu/cwl by the 10 MHz boundary (IARU convention).
	if strings.Contains(comment, "WPM") {
		if freqHz >= 10_000_000 {
			return "cwu"
		}
		return "cwl"
	}

	// Voice activity: comment starts with "USB" or "LSB".
	upper := strings.ToUpper(strings.TrimSpace(comment))
	if strings.HasPrefix(upper, "USB") {
		return "usb"
	}
	if strings.HasPrefix(upper, "LSB") {
		return "lsb"
	}

	// Digital decoder spots: "<MODE> <N> dB" pattern — skip silently.
	// Catches FT8, FT4, FT2, WSPR, JS8, JT65, Q65, FST4W, MSK144, etc.
	// without needing an explicit mode name list.
	if digitalCommentRe.MatchString(comment) {
		return ""
	}

	// Fallback for real DX cluster spots with free-text comments:
	// standard SSB band plan convention.
	if freqHz >= 10_000_000 {
		return "usb"
	}
	return "lsb"
}

// TuneAudioClient sends a PUT /api/v1/tune to the ubersdr-audio HTTP API at
// baseURL (e.g. "http://127.0.0.1:9770"), setting the frequency and mode.
// Returns an error if the request fails or the server returns a non-200 status.
func TuneAudioClient(baseURL string, freqHz int, mode string) error {
	body, err := json.Marshal(map[string]any{
		"frequency_hz": freqHz,
		"mode":         mode,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		baseURL+"/api/v1/tune", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT /api/v1/tune: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audio client returned %s", resp.Status)
	}
	return nil
}

// ConnectAudioClient tells the ubersdr-audio client (at baseURL) to connect to
// the UberSDR instance identified by instanceURL, via POST /api/v1/connect.
// Any existing audio-client session is disconnected first by the server.
// Returns an error if the request fails or the server returns a non-200 status.
func ConnectAudioClient(baseURL, instanceURL string) error {
	body, err := json.Marshal(map[string]any{
		"url": instanceURL,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v1/connect", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /api/v1/connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audio client returned %s", resp.Status)
	}
	return nil
}

// TestAudioClient performs a lightweight GET /api/v1/status against baseURL to
// confirm the ubersdr-audio HTTP API is reachable and responding. Returns nil
// on a 200 response, or a descriptive error if the connection fails or the
// server returns a non-200 status.
func TestAudioClient(baseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/api/v1/status", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET /api/v1/status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audio client returned %s", resp.Status)
	}
	return nil
}

// AudioClientBaseURL builds the base URL from host and port strings.
// Falls back to the default if either is empty or port is invalid.
func AudioClientBaseURL(host, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		host = defaultAudioHost
	}
	if port == "" {
		port = defaultAudioPort
	}
	return "http://" + host + ":" + port
}

const (
	defaultAudioHost = "127.0.0.1"
	defaultAudioPort = "9770"
)

// Download URLs for the ubersdr-audio client binary, by platform.
const (
	audioDownloadWindows = "https://github.com/madpsy/ka9q_ubersdr/releases/download/latest/UberSDRAudio.exe"
	audioDownloadLinux   = "https://github.com/madpsy/ka9q_ubersdr/releases/download/latest/UberSDRAudio"
)

// AudioClientDownloadURL returns the download URL for the ubersdr-audio client
// binary appropriate for the current operating system, plus a human-readable
// label for the platform. The second return value is false on unsupported
// platforms (e.g. macOS), where no prebuilt binary is offered.
func AudioClientDownloadURL() (url, platform string, ok bool) {
	switch runtime.GOOS {
	case "windows":
		return audioDownloadWindows, "Windows", true
	case "linux":
		return audioDownloadLinux, "Linux", true
	default:
		return "", runtime.GOOS, false
	}
}
