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

CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  role          TEXT NOT NULL DEFAULT 'user',
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orgs (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memberships (
  org_id     TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  role       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS memberships_user ON memberships(user_id);

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  org_id     TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  revoked_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sessions_hash ON sessions(token_hash);

CREATE TABLE IF NOT EXISTS scripts (
  name        TEXT PRIMARY KEY,
  code        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  timeout_ms  INTEGER NOT NULL DEFAULT 5000,
  allow_net   TEXT NOT NULL DEFAULT '[]',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS script_runs (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,
  input       TEXT NOT NULL DEFAULT '',
  result      TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT '',
  logs        TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL,
  started_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS script_runs_name ON script_runs(name, started_at DESC);

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
