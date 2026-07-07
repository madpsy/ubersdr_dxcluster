package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// TestPreferencesPersist verifies that editing the callsign and telnet-port
// fields writes through to the app preferences (uppercasing the callsign), and
// that a fresh UI loads those saved values back into the fields.
func TestPreferencesPersist(t *testing.T) {
	a := test.NewApp()
	prefs := a.Preferences()

	// First UI: user types a callsign and a custom port.
	ui := newAppUI(a.NewWindow("t"), prefs)
	ui.build()

	ui.callsign.SetText("m0abc")
	if got := prefs.String(prefCallsign); got != "M0ABC" {
		t.Fatalf("saved callsign = %q, want %q", got, "M0ABC")
	}

	ui.portEntry.SetText("7301")
	if got := prefs.String(prefTelnetPort); got != "7301" {
		t.Fatalf("saved port = %q, want %q", got, "7301")
	}

	// Second UI sharing the same preferences: fields should preload.
	ui2 := newAppUI(a.NewWindow("t2"), prefs)
	ui2.build()
	if got := ui2.callsign.Text; got != "M0ABC" {
		t.Fatalf("reloaded callsign = %q, want %q", got, "M0ABC")
	}
	if got := ui2.portEntry.Text; got != "7301" {
		t.Fatalf("reloaded port = %q, want %q", got, "7301")
	}
}
