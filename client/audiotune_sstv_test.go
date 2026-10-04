package main

import "testing"

func TestModeFromSpotLineSSTV(t *testing.T) {
	for _, tc := range []struct {
		freqKHz float64
		comment string
		want    string
	}{
		{14230.0, "SSTV Scottie 1 38 dB", "usb"},
		{7171.0, "SSTV PD120 24 dB", "lsb"},
		{14230.0, "SSTV 38 dB", "usb"},
		{14074.0, "FT8 -12 dB", ""},
	} {
		if got := modeFromSpotLine(tc.freqKHz, tc.comment); got != tc.want {
			t.Errorf("modeFromSpotLine(%v, %q) = %q, want %q", tc.freqKHz, tc.comment, got, tc.want)
		}
	}
}
