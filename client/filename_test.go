package main

import "testing"

func TestCallsignFromFilename(t *testing.T) {
	cases := map[string]string{
		"dxcluster_m9psy":                      "M9PSY",
		"dxcluster_m9psy.exe":                  "M9PSY",
		"/opt/bin/dxcluster_m9psy":             "M9PSY",
		`C:\Tools\dxcluster_m9psy.exe`:         "M9PSY",
		"ubersdr_dxcluster_ei4hq":              "EI4HQ",   // callsign is after the LAST underscore
		"dxcluster_m0xdk-1":                    "M0XDK-1", // SSID suffix preserved
		"dxcluster_M9PSY":                      "M9PSY",
		"ubersdr-dxcluster-client-linux-amd64": "", // no underscore → none
		"dxcluster":                            "",
		"dxcluster_":                           "", // nothing after delimiter
	}
	for in, want := range cases {
		if got := callsignFromFilename(in); got != want {
			t.Errorf("callsignFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstanceByCallsign(t *testing.T) {
	list := []Instance{
		{ID: "1", Callsign: "M9PSY", Name: "A"},
		{ID: "2", Callsign: "EI4HQ", Name: "B"},
	}
	if got := instanceByCallsign(list, "m9psy"); got == nil || got.ID != "1" {
		t.Fatalf("case-insensitive match failed: %+v", got)
	}
	if got := instanceByCallsign(list, "K1JFJ"); got != nil {
		t.Fatalf("expected no match, got %+v", got)
	}
}
