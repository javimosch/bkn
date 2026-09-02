// Package script is the escape hatch.
//
// The Node backend it replaces had grown roughly forty admin domains, each a
// route plus a controller plus a service plus a view, because there was no
// supported way to add behaviour without adding Go-equivalent code. Most of
// those domains are a scheduled HTTP call, a transform over stored records, or
// a webhook handler. Given a sandboxed runtime with access to store, kv and
// outbound HTTP, they are scripts - userland, editable at runtime, and not the
// core's problem.
//
// This is the primitive that keeps the core small, so it is deliberately the
// most capable one.
package script

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("script not found")
	ErrExists   = errors.New("script already exists")
	ErrBadName  = errors.New("script name must match [a-z][a-z0-9_-]{0,62}")
	ErrDisabled = errors.New("script is disabled")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// DefaultTimeoutMS bounds a run. A script with no bound can wedge the daemon,
// so there is no way to disable this - only to raise it.
const DefaultTimeoutMS = 5000

// Script is a stored program.
type Script struct {
	Name        string   `json:"name"`
	Code        string   `json:"code,omitempty"`
	Description string   `json:"description"`
	TimeoutMS   int      `json:"timeout_ms"`
	AllowNet    []string `json:"allow_net"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// Run is one execution record.
type Run struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"` // ok | error | timeout
	Input      string `json:"input,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	Logs       string `json:"logs,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	StartedAt  string `json:"started_at"`
}

const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusTimeout = "timeout"
)

// Registry stores script definitions and their run history.
type Registry struct{ db *sql.DB }

func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: got %q", ErrBadName, name)
	}
	return nil
}

// Create stores a new script.
func (r *Registry) Create(s Script) (Script, error) {
	if err := ValidateName(s.Name); err != nil {
		return Script{}, err
	}
	if s.TimeoutMS <= 0 {
		s.TimeoutMS = DefaultTimeoutMS
	}
	if s.AllowNet == nil {
		s.AllowNet = []string{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	s.CreatedAt, s.UpdatedAt, s.Enabled = now, now, true

	net, _ := json.Marshal(s.AllowNet)
	_, err := r.db.Exec(`
		INSERT INTO scripts (name, code, description, timeout_ms, allow_net, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,1,?,?)`,
		s.Name, s.Code, s.Description, s.TimeoutMS, string(net), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return Script{}, ErrExists
		}
		return Script{}, err
	}
	return s, nil
}

func isUniqueViolation(err error) bool {
	// modernc's driver reports this in the message; there is no typed sentinel.
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Update replaces the mutable fields of an existing script. Nil pointers leave
// a field untouched, so a code edit does not silently reset a timeout.
func (r *Registry) Update(name string, code, description *string, timeoutMS *int, allowNet *[]string, enabled *bool) (Script, error) {
	cur, err := r.Get(name)
	if err != nil {
		return Script{}, err
	}
	if code != nil {
		cur.Code = *code
	}
	if description != nil {
		cur.Description = *description
	}
	if timeoutMS != nil && *timeoutMS > 0 {
		cur.TimeoutMS = *timeoutMS
	}
	if allowNet != nil {
		cur.AllowNet = *allowNet
	}
	if enabled != nil {
		cur.Enabled = *enabled
	}
	cur.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	net, _ := json.Marshal(cur.AllowNet)
	en := 0
	if cur.Enabled {
		en = 1
	}
	_, err = r.db.Exec(`
		UPDATE scripts SET code=?, description=?, timeout_ms=?, allow_net=?, enabled=?, updated_at=?
		WHERE name=?`,
		cur.Code, cur.Description, cur.TimeoutMS, string(net), en, cur.UpdatedAt, name)
	if err != nil {
		return Script{}, err
	}
	return cur, nil
}

// Get returns one script including its code.
func (r *Registry) Get(name string) (Script, error) {
	var s Script
	var net string
	var enabled int
	err := r.db.QueryRow(`
		SELECT name, code, description, timeout_ms, allow_net, enabled, created_at, updated_at
		FROM scripts WHERE name = ?`, name).
		Scan(&s.Name, &s.Code, &s.Description, &s.TimeoutMS, &net, &enabled, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return Script{}, ErrNotFound
	}
	if err != nil {
		return Script{}, err
	}
	s.Enabled = enabled == 1
	s.AllowNet = []string{}
	_ = json.Unmarshal([]byte(net), &s.AllowNet)
	return s, nil
}

// List returns every script without its code, which is usually large.
func (r *Registry) List() ([]Script, error) {
	rows, err := r.db.Query(`
		SELECT name, description, timeout_ms, allow_net, enabled, created_at, updated_at
		FROM scripts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Script{}
	for rows.Next() {
		var s Script
		var net string
		var enabled int
		if err := rows.Scan(&s.Name, &s.Description, &s.TimeoutMS, &net, &enabled, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled == 1
		s.AllowNet = []string{}
		_ = json.Unmarshal([]byte(net), &s.AllowNet)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delete removes a script and its run history.
func (r *Registry) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM scripts WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	_, err = r.db.Exec(`DELETE FROM script_runs WHERE name = ?`, name)
	return err
}

// RecordRun stores an execution record.
func (r *Registry) RecordRun(run Run) error {
	_, err := r.db.Exec(`
		INSERT INTO script_runs (id, name, status, input, result, error, logs, duration_ms, started_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		run.ID, run.Name, run.Status, run.Input, run.Result, run.Error, run.Logs, run.DurationMS, run.StartedAt)
	return err
}

// Runs returns the most recent executions of a script, newest first.
func (r *Registry) Runs(name string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.Query(`
		SELECT id, name, status, input, result, error, logs, duration_ms, started_at
		FROM script_runs WHERE name = ? ORDER BY started_at DESC, id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Run{}
	for rows.Next() {
		var run Run
		if err := rows.Scan(&run.ID, &run.Name, &run.Status, &run.Input, &run.Result,
			&run.Error, &run.Logs, &run.DurationMS, &run.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
