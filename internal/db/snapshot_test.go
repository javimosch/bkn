package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/javimosch/bkn/internal/db"
)

// A snapshot taken while the database is open for writing must contain the
// writes that were committed before it started. This is the exact property
// the C sqlite3 CLI's read-only .backup does not guarantee against a hot WAL,
// and losing it produces a backup that restores cleanly having lost a day.
func TestSnapshotSeesCommittedWrites(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BKN_DATA", filepath.Join(dir, "bkn.db"))

	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(`INSERT INTO kv (key, value, type, updated_at) VALUES ('k', 'committed', 'string', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("write: %v", err)
	}

	dest := filepath.Join(dir, "snap.db")
	if err := db.Snapshot(dest); err != nil { // source still open, WAL still hot
		t.Fatalf("snapshot: %v", err)
	}

	got, err := db.Verify(dest)
	if err != nil || got != "ok" {
		t.Fatalf("integrity: %q %v", got, err)
	}

	t.Setenv("BKN_DATA", dest)
	snap, err := db.Open()
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	var value string
	if err := snap.QueryRow(`SELECT value FROM kv WHERE key = 'k'`).Scan(&value); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if value != "committed" {
		t.Fatalf("snapshot lost the write: got %q", value)
	}
}

func TestSnapshotRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BKN_DATA", filepath.Join(dir, "bkn.db"))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	dest := filepath.Join(dir, "taken.db")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.Snapshot(dest); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if b, _ := os.ReadFile(dest); string(b) != "precious" {
		t.Fatal("the existing file was modified")
	}
}
