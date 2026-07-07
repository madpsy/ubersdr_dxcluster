package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// TestAutoConnectByUUID verifies the startup target is stored by UUID and that
// it resolves against the current directory even when the host/port changes.
func TestAutoConnectByUUID(t *testing.T) {
	a := test.NewApp()
	prefs := a.Preferences()

	ui := newAppUI(a.NewWindow("t"), prefs)
	ui.build()

	// Choose an instance and mark it as the startup target via the checkbox.
	inst := Instance{ID: "uuid-123", Name: "Old Name", Callsign: "M0OLD", Host: "old.example.org", Port: 443, TLS: true}
	ui.current = &inst
	ui.refreshAutoCheck()
	ui.autoCheck.SetChecked(true) // fires OnChanged → save

	if got := prefs.String(prefAutoConnect); got != "uuid-123" {
		t.Fatalf("saved auto-connect uuid = %q, want %q", got, "uuid-123")
	}

	// The same UUID now appears in the directory with a NEW host/port/name.
	dir := []Instance{
		{ID: "other", Name: "Other", Host: "x", Port: 443, TLS: true},
		{ID: "uuid-123", Name: "New Name", Callsign: "M0NEW", Host: "new.example.org", Port: 8443, TLS: false},
	}
	resolved := instanceByID(dir, ui.loadAutoConnect())
	if resolved == nil {
		t.Fatal("expected to resolve saved uuid against the directory")
	}
	if resolved.Host != "new.example.org" || resolved.Port != 8443 {
		t.Fatalf("resolved stale connection info: host=%s port=%d", resolved.Host, resolved.Port)
	}
	if want := "ws://new.example.org:8443/addon/dxcluster/api/terminal"; resolved.TerminalWSURL() != want {
		t.Fatalf("TerminalWSURL = %q, want %q", resolved.TerminalWSURL(), want)
	}

	// Unticking clears the saved target.
	ui.autoCheck.SetChecked(false)
	if got := prefs.String(prefAutoConnect); got != "" {
		t.Fatalf("auto-connect not cleared, got %q", got)
	}
}
