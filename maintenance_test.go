package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintainOnDiskDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spots.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()

	// Pragmas from the DSN must apply to every pooled connection.
	s.db.SetMaxOpenConns(4)
	for i := 0; i < 4; i++ {
		var sync, timeout int
		if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if sync != 1 || timeout != 10000 {
			t.Fatalf("pragmas not applied: synchronous=%d busy_timeout=%d", sync, timeout)
		}
	}
	var av int
	_ = s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av)
	if av != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (INCREMENTAL)", av)
	}

	old := time.Now().AddDate(0, 0, -40)
	var batch []Spot
	for i := 0; i < 2000; i++ {
		ts := time.Now()
		if i%2 == 0 {
			ts = old
		}
		batch = append(batch, Spot{Stream: "decoder", Timestamp: ts, Callsign: "G0ABC", Band: "20m", Comment: string(make([]byte, 200))})
	}
	if err := s.insertBatch(batch); err != nil {
		t.Fatal(err)
	}

	s.maintain(30, true)

	if n := s.Count(); n != 1000 {
		t.Fatalf("after purge Count = %d, want 1000", n)
	}
	if err := s.quickCheck(); err != nil {
		t.Fatalf("quickCheck: %v", err)
	}
}

func TestIsCorrupt(t *testing.T) {
	if !isCorrupt(&corruptError{detail: "x"}) {
		t.Error("corruptError not detected")
	}
	if !isCorrupt(errors.New("database disk image is malformed (11)")) {
		t.Error("malformed message not detected")
	}
	if isCorrupt(errors.New("database is locked (5)")) || isCorrupt(nil) {
		t.Error("false positive")
	}
}

func TestNextMaintTime(t *testing.T) {
	loc := time.UTC
	got := nextMaintTime(time.Date(2026, 11, 18, 2, 0, 0, 0, loc))
	if want := time.Date(2026, 11, 18, 3, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("before: got %v want %v", got, want)
	}
	got = nextMaintTime(time.Date(2026, 11, 18, 3, 30, 0, 0, loc))
	if want := time.Date(2026, 11, 19, 3, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("at: got %v want %v", got, want)
	}
}
