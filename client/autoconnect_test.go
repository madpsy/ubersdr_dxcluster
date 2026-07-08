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
	// Local prefs must not be set for a public instance.
	if got := prefs.String(prefAutoConnectLocalCallsign); got != "" {
		t.Fatalf("local callsign pref should be empty for public instance, got %q", got)
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

	// Unticking writes "none" (not "") to distinguish "disabled" from "never set".
	ui.autoCheck.SetChecked(false)
	if got := prefs.String(prefAutoConnect); got != "none" {
		t.Fatalf("auto-connect not set to none on uncheck, got %q", got)
	}
	// Local prefs must also be cleared on uncheck.
	if got := prefs.String(prefAutoConnectLocalCallsign); got != "" {
		t.Fatalf("local callsign pref not cleared on uncheck, got %q", got)
	}
}

// TestAutoConnectLocalInstance verifies that choosing a local (mDNS) instance
// stores the sentinel value and the callsign/address prefs, and that
// refreshAutoCheck ticks the checkbox when the current instance matches.
func TestAutoConnectLocalInstance(t *testing.T) {
	a := test.NewApp()
	prefs := a.Preferences()

	ui := newAppUI(a.NewWindow("t"), prefs)
	ui.build()

	local := Instance{
		ID:       "local:192.168.1.10:8080",
		Callsign: "MM3NDH",
		Name:     "Home SDR",
		Host:     "192.168.1.10",
		Port:     8080,
	}
	ui.current = &local
	ui.refreshAutoCheck()
	ui.autoCheck.SetChecked(true) // fires OnChanged → saveAutoConnect

	if got := prefs.String(prefAutoConnect); got != autoConnectLocalSentinel {
		t.Fatalf("prefAutoConnect = %q, want %q", got, autoConnectLocalSentinel)
	}
	if got := prefs.String(prefAutoConnectLocalCallsign); got != "MM3NDH" {
		t.Fatalf("local callsign = %q, want %q", got, "MM3NDH")
	}
	if got := prefs.String(prefAutoConnectLocalAddr); got != "192.168.1.10:8080" {
		t.Fatalf("local addr = %q, want %q", got, "192.168.1.10:8080")
	}

	// refreshAutoCheck should tick the checkbox when the current instance
	// callsign matches the saved local callsign.
	ui.refreshAutoCheck()
	if !ui.autoCheck.Checked {
		t.Fatal("autoCheck should be ticked for matching local instance")
	}

	// A different local instance with a different callsign should not tick.
	other := Instance{
		ID:       "local:192.168.1.99:8080",
		Callsign: "G0XYZ",
		Name:     "Other SDR",
		Host:     "192.168.1.99",
		Port:     8080,
	}
	ui.current = &other
	ui.refreshAutoCheck()
	if ui.autoCheck.Checked {
		t.Fatal("autoCheck should not be ticked for a different local callsign")
	}

	// Unticking clears the sentinel and local prefs.
	ui.current = &local
	ui.autoCheck.SetChecked(true)  // re-save
	ui.autoCheck.SetChecked(false) // clear
	if got := prefs.String(prefAutoConnect); got != "none" {
		t.Fatalf("prefAutoConnect after uncheck = %q, want %q", got, "none")
	}
	if got := prefs.String(prefAutoConnectLocalCallsign); got != "" {
		t.Fatalf("local callsign not cleared on uncheck, got %q", got)
	}
	if got := prefs.String(prefAutoConnectLocalAddr); got != "" {
		t.Fatalf("local addr not cleared on uncheck, got %q", got)
	}
}

// TestSplitHostPort verifies the helper used by doLocalAutoConnect.
func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		addr    string
		host    string
		port    int
		wantErr bool
	}{
		{"192.168.1.4:8080", "192.168.1.4", 8080, false},
		{"localhost:7300", "localhost", 7300, false},
		{"nocolon", "", 0, true},
		{"host:notanumber", "", 0, true},
	}
	for _, tc := range tests {
		h, p, err := splitHostPort(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitHostPort(%q): expected error, got host=%q port=%d", tc.addr, h, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitHostPort(%q): unexpected error: %v", tc.addr, err)
			continue
		}
		if h != tc.host || p != tc.port {
			t.Errorf("splitHostPort(%q) = (%q, %d), want (%q, %d)", tc.addr, h, p, tc.host, tc.port)
		}
	}
}
