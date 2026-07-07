package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestHandleClients verifies the desktop-client download endpoint serves the
// known binaries with an attachment disposition and rejects anything else.
func TestHandleClients(t *testing.T) {
	dir := t.TempDir()
	want := []byte("fake-linux-binary")
	if err := os.WriteFile(filepath.Join(dir, "dxcluster"), want, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIENTS_DIR", dir)

	w := &WebServer{}

	// Known file → 200 with attachment headers and correct body.
	rec := httptest.NewRecorder()
	w.handleClients(rec, httptest.NewRequest(http.MethodGet, "/clients/dxcluster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.Bytes(); string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="dxcluster"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// Known name but file absent → 404.
	rec = httptest.NewRecorder()
	w.handleClients(rec, httptest.NewRequest(http.MethodGet, "/clients/dxcluster.exe", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d, want 404", rec.Code)
	}

	// Unknown name → 404 (no traversal / listing).
	rec = httptest.NewRecorder()
	w.handleClients(rec, httptest.NewRequest(http.MethodGet, "/clients/../etc/passwd", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown name status = %d, want 404", rec.Code)
	}
}

// TestHandleClientDownload checks OS detection and callsign-named downloads.
func TestHandleClientDownload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dxcluster"), []byte("lin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dxcluster.exe"), []byte("win"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIENTS_DIR", dir)

	w := &WebServer{rxCallsign: "M9PSY"}

	check := func(name string, req *http.Request, wantBody, wantFilename string) {
		rec := httptest.NewRecorder()
		w.handleClientDownload(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, rec.Code)
		}
		if rec.Body.String() != wantBody {
			t.Fatalf("%s: body %q, want %q", name, rec.Body.String(), wantBody)
		}
		want := `attachment; filename="` + wantFilename + `"`
		if got := rec.Header().Get("Content-Disposition"); got != want {
			t.Fatalf("%s: disposition %q, want %q", name, got, want)
		}
	}

	// Explicit os override.
	check("os=windows", httptest.NewRequest(http.MethodGet, "/client/download?os=windows", nil),
		"win", "dxcluster_m9psy.exe")
	check("os=linux", httptest.NewRequest(http.MethodGet, "/client/download?os=linux", nil),
		"lin", "dxcluster_m9psy")

	// Sec-CH-UA-Platform client hint.
	rq := httptest.NewRequest(http.MethodGet, "/client/download", nil)
	rq.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	check("hint Windows", rq, "win", "dxcluster_m9psy.exe")

	// User-Agent fallback (Windows).
	rq = httptest.NewRequest(http.MethodGet, "/client/download", nil)
	rq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	check("ua Windows", rq, "win", "dxcluster_m9psy.exe")

	// User-Agent fallback (Linux → linux build).
	rq = httptest.NewRequest(http.MethodGet, "/client/download", nil)
	rq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	check("ua Linux", rq, "lin", "dxcluster_m9psy")

	// Empty callsign falls back to a generic name.
	w2 := &WebServer{rxCallsign: ""}
	rec := httptest.NewRecorder()
	w2.handleClientDownload(rec, httptest.NewRequest(http.MethodGet, "/client/download?os=linux", nil))
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="dxcluster_client"` {
		t.Fatalf("empty callsign disposition = %q", got)
	}
}
