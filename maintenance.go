package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// ── Database maintenance ───────────────────────────────────────────────────
//
// Maintenance runs a few minutes after startup and then nightly. Each pass:
//
//   - purges spots older than the retention window
//   - runs PRAGMA quick_check; a failure exits the process so the container
//     restarts and entrypoint.sh repairs the file with the sqlite3 CLI
//   - checkpoints and truncates the WAL
//   - returns freed pages to the OS (incremental_vacuum)
//   - once a week, or when the free list is large, rebuilds the file with a
//     full VACUUM to undo fragmentation
//   - refreshes query-planner statistics (PRAGMA optimize)
//
// Writes are paused (writeMu) for the duration; incoming spots queue in the
// store's channel and are flushed as soon as maintenance finishes.

const (
	maintStartupDelay = 5 * time.Minute
	maintHour         = 3 // local time of the nightly pass (03:30)
	maintMinute       = 30
	maintFullVacuumOn = time.Sunday
	// Run a full VACUUM early if more than this fraction of the file is free
	// pages that incremental_vacuum could not hand back.
	maintFreeListRatio = 0.10
)

// sqliteCorrupt and sqliteNotADB are the primary result codes that mean the
// file itself is damaged (extended codes carry these in their low byte).
const (
	sqliteCorrupt = 11
	sqliteNotADB  = 26
)

// isCorrupt reports whether err means the database file is damaged.
func isCorrupt(err error) bool {
	if err == nil {
		return false
	}
	var ce interface{ Code() int }
	if errors.As(err, &ce) {
		c := ce.Code() & 0xff
		return c == sqliteCorrupt || c == sqliteNotADB
	}
	msg := err.Error()
	return strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database")
}

// fatalIfCorrupt exits the process when err indicates database corruption.
// Carrying on would silently drop every spot until someone notices; exiting
// lets the container restart and entrypoint.sh's startup check repair the DB.
func fatalIfCorrupt(op string, err error) {
	if isCorrupt(err) {
		log.Printf("[store] %s: database corruption detected: %v", op, err)
		log.Printf("[store] exiting so the startup integrity check can repair the database")
		os.Exit(1)
	}
}

// RunMaintenance purges once immediately, runs a full maintenance pass
// maintStartupDelay after startup, then nightly at maintHour:maintMinute.
// Call in a goroutine.
func (s *SpotStore) RunMaintenance(retainDays int) {
	// Purge straight away in case the process was down for a while.
	s.writeMu.Lock()
	s.purge(retainDays)
	s.writeMu.Unlock()

	time.Sleep(maintStartupDelay)
	s.maintain(retainDays, false)

	for {
		next := nextMaintTime(time.Now())
		time.Sleep(time.Until(next))
		s.maintain(retainDays, next.Weekday() == maintFullVacuumOn)
	}
}

// nextMaintTime returns the next maintHour:maintMinute after now.
func nextMaintTime(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), maintHour, maintMinute, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// maintain runs one maintenance pass. fullVacuum forces a full VACUUM.
func (s *SpotStore) maintain(retainDays int, fullVacuum bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	start := time.Now()
	sizeBefore := s.fileSizeBytes()

	s.purge(retainDays)

	if err := s.quickCheck(); err != nil {
		fatalIfCorrupt("quick_check", err)
		log.Printf("[store] maintenance: quick_check error: %v", err)
	}

	s.checkpoint()

	if _, err := s.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		fatalIfCorrupt("incremental_vacuum", err)
		log.Printf("[store] maintenance: incremental_vacuum: %v", err)
	}

	var pages, free int64
	_ = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = s.db.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	if !fullVacuum && pages > 0 && float64(free)/float64(pages) > maintFreeListRatio {
		log.Printf("[store] maintenance: %d of %d pages free — running full VACUUM", free, pages)
		fullVacuum = true
	}
	if fullVacuum {
		s.vacuum()
	}

	if _, err := s.db.Exec(`PRAGMA optimize`); err != nil {
		log.Printf("[store] maintenance: optimize: %v", err)
	}

	log.Printf("[store] maintenance done in %s (db %s → %s, full vacuum: %v)",
		time.Since(start).Round(time.Millisecond),
		humanBytes(sizeBefore), humanBytes(s.fileSizeBytes()), fullVacuum)
}

// purge deletes spots older than retainDays. Caller holds writeMu.
func (s *SpotStore) purge(retainDays int) {
	cutoff := time.Now().AddDate(0, 0, -retainDays).Unix()
	res, err := s.db.Exec(`DELETE FROM spots WHERE ts < ?`, cutoff)
	if err != nil {
		fatalIfCorrupt("purge", err)
		log.Printf("[store] purge error: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[store] purged %d spots older than %d days", n, retainDays)
	}
	s.checkpoint()
	if _, err := s.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		fatalIfCorrupt("incremental_vacuum", err)
		log.Printf("[store] incremental_vacuum: %v", err)
	}
}

// quickCheck runs PRAGMA quick_check and returns an error describing the
// first problems found, or nil if the database is healthy.
func (s *SpotStore) quickCheck() error {
	rows, err := s.db.Query(`PRAGMA quick_check(20)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return nil
	}
	return &corruptError{detail: strings.Join(msgs, "; ")}
}

type corruptError struct{ detail string }

func (e *corruptError) Error() string { return "database disk image is malformed: " + e.detail }
func (e *corruptError) Code() int     { return sqliteCorrupt }

// checkpoint copies the WAL into the main file and truncates it.
func (s *SpotStore) checkpoint() {
	var busy, logFrames, done int
	err := s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done)
	if err != nil {
		fatalIfCorrupt("wal_checkpoint", err)
		log.Printf("[store] wal_checkpoint: %v", err)
		return
	}
	if busy != 0 {
		log.Printf("[store] wal_checkpoint: blocked by a reader (%d/%d frames checkpointed)", done, logFrames)
	}
}

// vacuum rebuilds the database file. It needs free disk roughly equal to the
// DB size for the temporary copy; temp_store=FILE keeps that copy on disk
// rather than in RAM.
func (s *SpotStore) vacuum() {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		log.Printf("[store] vacuum: %v", err)
		return
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, `PRAGMA temp_store = FILE`)
	start := time.Now()
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		fatalIfCorrupt("vacuum", err)
		log.Printf("[store] vacuum: %v", err)
		return
	}
	log.Printf("[store] full VACUUM took %s", time.Since(start).Round(time.Millisecond))
	_, _ = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

// fileSizeBytes returns the main database file size (excluding the WAL).
func (s *SpotStore) fileSizeBytes() int64 {
	var pages, size int64
	_ = s.db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = s.db.QueryRow(`PRAGMA page_size`).Scan(&size)
	return pages * size
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
