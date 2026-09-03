package cron

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
	ErrNotFound = errors.New("cron job not found")
	ErrExists   = errors.New("a cron job with that name already exists")
	ErrBadName  = errors.New("job name must match [a-z][a-z0-9_-]{0,62}")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Job binds a schedule to a script.
type Job struct {
	Name       string         `json:"name"`
	Schedule   string         `json:"schedule"`
	Script     string         `json:"script"`
	Input      map[string]any `json:"input,omitempty"`
	Enabled    bool           `json:"enabled"`
	NextRunAt  string         `json:"next_run_at,omitempty"`
	LastRunAt  string         `json:"last_run_at,omitempty"`
	LastStatus string         `json:"last_status,omitempty"`
	LastRunID  string         `json:"last_run_id,omitempty"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
}

// Registry stores cron jobs.
type Registry struct{ db *sql.DB }

func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Create registers a job. The schedule is parsed up front, so an unparseable
// expression fails at creation rather than silently never firing.
func (r *Registry) Create(j Job) (Job, error) {
	if !nameRe.MatchString(j.Name) {
		return Job{}, fmt.Errorf("%w: got %q", ErrBadName, j.Name)
	}
	schedule, err := Parse(j.Schedule)
	if err != nil {
		return Job{}, err
	}
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	input, err := json.Marshal(j.Input)
	if err != nil {
		return Job{}, err
	}
	j.Enabled = true
	j.CreatedAt, j.UpdatedAt = now(), now()
	j.NextRunAt = stamp(schedule.Next(time.Now()))

	_, err = r.db.Exec(`
		INSERT INTO cron_jobs (name, schedule, script, input, enabled, next_run_at, created_at, updated_at)
		VALUES (?,?,?,?,1,?,?,?)`,
		j.Name, j.Schedule, j.Script, string(input), j.NextRunAt, j.CreatedAt, j.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Job{}, ErrExists
		}
		return Job{}, err
	}
	return j, nil
}

const jobCols = `name, schedule, script, input, enabled, next_run_at, last_run_at, last_status, last_run_id, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var input string
	var enabled int
	err := row.Scan(&j.Name, &j.Schedule, &j.Script, &input, &enabled, &j.NextRunAt,
		&j.LastRunAt, &j.LastStatus, &j.LastRunID, &j.CreatedAt, &j.UpdatedAt)
	if err == sql.ErrNoRows {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.Enabled = enabled == 1
	j.Input = map[string]any{}
	_ = json.Unmarshal([]byte(input), &j.Input)
	return j, nil
}

// Get returns one job.
func (r *Registry) Get(name string) (Job, error) {
	return scanJob(r.db.QueryRow(`SELECT `+jobCols+` FROM cron_jobs WHERE name = ?`, name))
}

// List returns every job, soonest first.
func (r *Registry) List() ([]Job, error) {
	rows, err := r.db.Query(`SELECT ` + jobCols + ` FROM cron_jobs ORDER BY next_run_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Update changes only the fields whose pointers are non-nil. Changing the
// schedule recomputes the next firing time.
func (r *Registry) Update(name string, schedule, script *string, input *map[string]any, enabled *bool) (Job, error) {
	j, err := r.Get(name)
	if err != nil {
		return Job{}, err
	}
	if schedule != nil {
		if _, err := Parse(*schedule); err != nil {
			return Job{}, err
		}
		j.Schedule = *schedule
	}
	if script != nil {
		j.Script = *script
	}
	if input != nil {
		j.Input = *input
	}
	if enabled != nil {
		j.Enabled = *enabled
	}
	j.UpdatedAt = now()

	// A job that was disabled, or whose schedule changed, must not fire on a
	// stale next_run_at computed under the old rules.
	parsed, err := Parse(j.Schedule)
	if err != nil {
		return Job{}, err
	}
	if j.Enabled {
		j.NextRunAt = stamp(parsed.Next(time.Now()))
	} else {
		j.NextRunAt = ""
	}

	raw, err := json.Marshal(j.Input)
	if err != nil {
		return Job{}, err
	}
	en := 0
	if j.Enabled {
		en = 1
	}
	_, err = r.db.Exec(`
		UPDATE cron_jobs SET schedule=?, script=?, input=?, enabled=?, next_run_at=?, updated_at=?
		WHERE name=?`,
		j.Schedule, j.Script, string(raw), en, j.NextRunAt, j.UpdatedAt, name)
	if err != nil {
		return Job{}, err
	}
	return j, nil
}

// Delete removes a job.
func (r *Registry) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM cron_jobs WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// disable turns a job off without going through Update.
//
// Update re-parses the schedule and refuses an invalid one, which is right for
// a caller editing a job and exactly wrong for the scheduler trying to switch
// off a job *because* its schedule no longer parses. Routing that through
// Update left the job enabled and retried on every tick, forever.
func (r *Registry) disable(name string) error {
	_, err := r.db.Exec(`UPDATE cron_jobs SET enabled = 0, next_run_at = '', updated_at = ? WHERE name = ?`,
		now(), name)
	return err
}

// due returns enabled jobs whose next run has arrived.
func (r *Registry) due(at time.Time) ([]Job, error) {
	rows, err := r.db.Query(`
		SELECT `+jobCols+` FROM cron_jobs
		WHERE enabled = 1 AND next_run_at != '' AND next_run_at <= ?
		ORDER BY next_run_at`, stamp(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// claim advances a job's next run, but only if nobody else has already done
// so. The compare-and-set is what stops two tickers - two processes sharing
// one database, or a manual `cron tick` racing a running daemon - from firing
// the same job twice.
func (r *Registry) claim(j Job, next time.Time) (bool, error) {
	res, err := r.db.Exec(`
		UPDATE cron_jobs SET next_run_at = ?, updated_at = ?
		WHERE name = ? AND next_run_at = ?`,
		stamp(next), now(), j.Name, j.NextRunAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// recordRun stores the outcome of one execution.
func (r *Registry) recordRun(name, status, runID string, at time.Time) error {
	_, err := r.db.Exec(`
		UPDATE cron_jobs SET last_run_at = ?, last_status = ?, last_run_id = ?, updated_at = ?
		WHERE name = ?`, stamp(at), status, runID, now(), name)
	return err
}
