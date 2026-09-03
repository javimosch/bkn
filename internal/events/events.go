// Package events is the append-only log primitive: errors, audit trails and
// counters, which in the system this replaces were three separate admin
// domains with three separate schemas and three separate query UIs.
//
// They are one thing. An event has a stream, a type, a level, a subject and a
// JSON body; "an error happened", "someone changed a setting" and "a request
// took 40ms" differ only in which of those fields you care about.
package events

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/javimosch/bkn/internal/store"
)

var (
	ErrBadStream   = errors.New("stream must match [a-z][a-z0-9_-]{0,62}")
	ErrBadLevel    = errors.New("level must be one of: debug, info, warn, error")
	ErrBadDuration = errors.New(`duration must look like 30m, 24h, 7d or an RFC3339 timestamp`)
	ErrBadGroupBy  = errors.New("group must be one of: type, level, source, subject")
)

const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

func Levels() []string   { return []string{LevelDebug, LevelInfo, LevelWarn, LevelError} }
func GroupBys() []string { return []string{"type", "level", "source", "subject"} }

var streamRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Event is one record. Its id is a ULID, so ordering by id is ordering by
// time and pagination stays stable while new events arrive.
type Event struct {
	ID        string         `json:"id"`
	Stream    string         `json:"stream"`
	Type      string         `json:"type"`
	Level     string         `json:"level"`
	Source    string         `json:"source,omitempty"`
	Subject   string         `json:"subject,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt string         `json:"created_at"`
}

// Query filters a listing. Zero values mean "no constraint".
type Query struct {
	Type    string
	Level   string
	Source  string
	Subject string
	Since   string // RFC3339
	Until   string // RFC3339
	Limit   int
	Offset  int
}

// Log owns the event table.
type Log struct{ db *sql.DB }

func New(db *sql.DB) *Log { return &Log{db: db} }

func validLevel(level string) bool {
	for _, l := range Levels() {
		if l == level {
			return true
		}
	}
	return false
}

var relativeRe = regexp.MustCompile(`^(\d+)([smhd])$`)

// ParseWhen accepts an RFC3339 timestamp or a relative age like "24h" or
// "7d", which is what anyone actually types when asking for recent events.
func ParseWhen(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if m := relativeRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %q", ErrBadDuration, s)
		}
		unit := map[string]time.Duration{
			"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour,
		}[m[2]]
		return time.Now().UTC().Add(-time.Duration(n) * unit), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q", ErrBadDuration, s)
	}
	return t.UTC(), nil
}

// Emit appends an event.
func (l *Log) Emit(e Event) (Event, error) {
	if !streamRe.MatchString(e.Stream) {
		return Event{}, fmt.Errorf("%w: got %q", ErrBadStream, e.Stream)
	}
	if e.Type == "" {
		return Event{}, errors.New("event type is required")
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if !validLevel(e.Level) {
		return Event{}, fmt.Errorf("%w: got %q", ErrBadLevel, e.Level)
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		return Event{}, err
	}
	e.ID = store.NewID()
	e.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	_, err = l.db.Exec(`
		INSERT INTO events (id, ns, type, level, source, subject, data, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		e.ID, e.Stream, e.Type, e.Level, e.Source, e.Subject, string(data), e.CreatedAt)
	if err != nil {
		return Event{}, err
	}
	return e, nil
}

// where builds the shared filter clause.
func (q Query) where(stream string) (string, []any, error) {
	clause := " WHERE ns = ?"
	args := []any{stream}

	add := func(column, value string) {
		if value != "" {
			clause += " AND " + column + " = ?"
			args = append(args, value)
		}
	}
	add("type", q.Type)
	add("source", q.Source)
	add("subject", q.Subject)
	if q.Level != "" {
		if !validLevel(q.Level) {
			return "", nil, fmt.Errorf("%w: got %q", ErrBadLevel, q.Level)
		}
		clause += " AND level = ?"
		args = append(args, q.Level)
	}
	for column, raw := range map[string]string{">=": q.Since, "<=": q.Until} {
		if raw == "" {
			continue
		}
		t, err := ParseWhen(raw)
		if err != nil {
			return "", nil, err
		}
		clause += " AND created_at " + column + " ?"
		args = append(args, t.Format(time.RFC3339))
	}
	return clause, args, nil
}

// List returns matching events, newest first.
func (l *Log) List(stream string, q Query) ([]Event, error) {
	clause, args, err := q.where(stream)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.db.Query(`
		SELECT id, ns, type, level, source, subject, data, created_at
		FROM events`+clause+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, q.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.ID, &e.Stream, &e.Type, &e.Level, &e.Source,
			&e.Subject, &data, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Data = map[string]any{}
		_ = json.Unmarshal([]byte(data), &e.Data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Bucket is one row of a stats rollup.
type Bucket struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// Stats counts events grouped by one field.
//
// The group column is chosen from a fixed set rather than interpolated from
// the caller, because it is the one part of these queries that cannot be a
// bound parameter.
func (l *Log) Stats(stream, groupBy string, q Query) ([]Bucket, error) {
	column := map[string]string{
		"type": "type", "level": "level", "source": "source", "subject": "subject",
	}[groupBy]
	if column == "" {
		return nil, fmt.Errorf("%w: got %q", ErrBadGroupBy, groupBy)
	}
	clause, args, err := q.where(stream)
	if err != nil {
		return nil, err
	}
	rows, err := l.db.Query(`
		SELECT `+column+` AS k, COUNT(*) FROM events`+clause+`
		GROUP BY k ORDER BY COUNT(*) DESC, k`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Key, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Streams lists every stream with its size and most recent event.
func (l *Log) Streams() ([]map[string]any, error) {
	rows, err := l.db.Query(`
		SELECT ns, COUNT(*), MAX(created_at),
		       SUM(CASE WHEN level = 'error' THEN 1 ELSE 0 END)
		FROM events GROUP BY ns ORDER BY ns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var name, latest string
		var count, errCount int
		if err := rows.Scan(&name, &count, &latest, &errCount); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"stream": name, "count": count, "errors": errCount, "latest": latest,
		})
	}
	return out, rows.Err()
}

// Prune deletes events older than the given age and reports how many went.
//
// An append-only log with no retention is a disk-space incident waiting for a
// quiet weekend. This is deliberately explicit rather than automatic, so the
// deletion is always something someone asked for - typically from a cron job.
func (l *Log) Prune(stream, olderThan string) (int, error) {
	cutoff, err := ParseWhen(olderThan)
	if err != nil {
		return 0, err
	}
	if cutoff.IsZero() {
		return 0, fmt.Errorf("%w: an age is required", ErrBadDuration)
	}
	q := `DELETE FROM events WHERE created_at < ?`
	args := []any{cutoff.Format(time.RFC3339)}
	if stream != "" {
		q += ` AND ns = ?`
		args = append(args, stream)
	}
	res, err := l.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
