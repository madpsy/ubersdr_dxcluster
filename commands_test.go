package main

import "testing"

func TestSpotRangeKHz(t *testing.T) {
	tests := []struct {
		name             string
		rx               ReceiverInfo
		wantMin, wantMax float64
	}{
		{
			name:    "tuning range reported",
			rx:      ReceiverInfo{TuneMinHz: 10000, TuneMaxHz: 60000000},
			wantMin: 10, wantMax: 60000,
		},
		{
			name:    "not reported falls back to default",
			rx:      ReceiverInfo{},
			wantMin: spotMinKHz, wantMax: spotMaxKHz,
		},
		{
			name:    "nonsensical range falls back to default",
			rx:      ReceiverInfo{TuneMinHz: 30000000, TuneMaxHz: 10000},
			wantMin: spotMinKHz, wantMax: spotMaxKHz,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			min, max := spotRangeKHz(tc.rx)
			if min != tc.wantMin || max != tc.wantMax {
				t.Errorf("spotRangeKHz() = %v, %v; want %v, %v", min, max, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestFormatKHz(t *testing.T) {
	tests := []struct {
		khz  float64
		want string
	}{
		{10, "10 kHz"},
		{137.5, "137.5 kHz"},
		{30000, "30 MHz"},
		{60000, "60 MHz"},
		{14350.5, "14.3505 MHz"},
	}
	for _, tc := range tests {
		if got := formatKHz(tc.khz); got != tc.want {
			t.Errorf("formatKHz(%v) = %q; want %q", tc.khz, got, tc.want)
		}
	}
}

func TestSpotLimitsGuardsZeroValue(t *testing.T) {
	// A server built without NewTelnetServer has zero limits; it must fall
	// back to the defaults rather than rejecting every spot.
	var zero TelnetServer
	if min, max := zero.spotLimits(); min != spotMinKHz || max != spotMaxKHz {
		t.Errorf("zero-value spotLimits() = %v, %v; want defaults %v, %v",
			min, max, spotMinKHz, spotMaxKHz)
	}

	srv := &TelnetServer{spotMinKHz: 10, spotMaxKHz: 60000}
	if min, max := srv.spotLimits(); min != 10 || max != 60000 {
		t.Errorf("spotLimits() = %v, %v; want 10, 60000", min, max)
	}
}
