package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// TestWindowTitle checks the title is the plain app name when idle and
// "DXCluster - <instance callsign>" while a session is active.
func TestWindowTitle(t *testing.T) {
	a := test.NewApp()
	w := a.NewWindow(titleDisconnected)
	ui := newAppUI(w, a.Preferences())
	ui.build()

	ui.updateTitle()
	if got := w.Title(); got != titleDisconnected {
		t.Fatalf("idle title = %q, want %q", got, titleDisconnected)
	}

	// Simulate an active session to instance M9PSY.
	ui.current = &Instance{ID: "u1", Callsign: "M9PSY", Name: "Some RX"}
	ui.client.Store(&DXClusterClient{})
	ui.updateTitle()
	if got, want := w.Title(), "DXCluster - M9PSY"; got != want {
		t.Fatalf("connected title = %q, want %q", got, want)
	}

	// Back to idle.
	ui.client.Store(nil)
	ui.updateTitle()
	if got := w.Title(); got != titleDisconnected {
		t.Fatalf("post-disconnect title = %q, want %q", got, titleDisconnected)
	}
}
