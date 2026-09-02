// Package db owns the single SQLite datastore. Requirement R12: the binary is
// the only writer. Nothing else opens this file; consumers go through the CLI,
// the HTTP API, or not at all.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure Go, no cgo: keeps the static single binary
)

const schema = `
CREATE TABLE IF NOT EXISTS collections (
  ns         TEXT NOT NULL,
  name       TEXT NOT NULL,
  normalize  TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  PRIMARY KEY (ns, name)
);

CREATE TABLE IF NOT EXISTS records (
  ns         TEXT NOT NULL,
  coll       TEXT NOT NULL,
  id         TEXT NOT NULL,
  doc        TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (ns, coll, id)
);
CREATE INDEX IF NOT EXISTS records_ns_coll ON records(ns, coll, updated_at DESC);

CREATE TABLE IF NOT EXISTS kv (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  type        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  public      INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT NOT NULL
);
`

// Path resolves the datastore location. Requirement R11: one resolution rule,
// one default, surfaced in help-json - never a hardcoded fallback per consumer.
func Path() string {
	if p := os.Getenv("BKN_DATA"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "bkn.db"
	}
	return filepath.Join(home, ".bkn", "bkn.db")
}

// Open opens (and migrates) the datastore at Path.
func Open() (*sql.DB, error) {
	p := Path()
	if dir := filepath.Dir(p); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	// WAL keeps readers from blocking the writer; busy_timeout absorbs the
	// brief contention between a running `serve` and a one-shot CLI call.
	dsn := p + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	if _, err := conn.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return conn, nil
}
