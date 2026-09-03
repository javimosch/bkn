// Package hooks is the inbound counterpart to bkn.http.fetch: it lets an
// external system deliver an event to a script.
//
// Without it no third-party integration can ever be userland. Stripe, GitHub,
// Slack and everything like them authenticate with a signature header rather
// than a bearer token, so they cannot use an authenticated route - and if the
// core cannot receive them, every integration has to be Go code, which is
// exactly what the script primitive exists to avoid.
//
// The route is therefore deliberately public, and the script is responsible
// for verifying the signature. bkn.crypto exists so it can.
package hooks

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
	ErrNotFound = errors.New("hook not found")
	ErrExists   = errors.New("a hook with that name already exists")
	ErrBadName  = errors.New("hook name must match [a-z][a-z0-9_-]{0,62}")
	ErrDisabled = errors.New("hook is disabled")
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// DefaultMaxBytes bounds a delivery. Webhook payloads are small; anything
// larger is a mistake or an attack.
const DefaultMaxBytes = 1 << 20

// Hook binds a public URL path to a script.
type Hook struct {
	Name        string   `json:"name"`
	Script      string   `json:"script"`
	Enabled     bool     `json:"enabled"`
	MaxBytes    int64    `json:"max_bytes"`
	AllowOrigin []string `json:"allow_origin"`
	RateLimit   int      `json:"rate_limit"`
	Path        string   `json:"path"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// OriginAllowed reports whether a browser at origin may call this hook.
// An empty list means no cross-origin access at all, which is the right
// default for a webhook: only a form posted from a page needs CORS.
func (h Hook) OriginAllowed(origin string) bool {
	for _, allowed := range h.AllowOrigin {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// Registry stores hook bindings.
type Registry struct{ db *sql.DB }

func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func withPath(h Hook) Hook {
	h.Path = "/v1/hooks/" + h.Name
	return h
}

// Create registers a hook.
func (r *Registry) Create(h Hook) (Hook, error) {
	if !nameRe.MatchString(h.Name) {
		return Hook{}, fmt.Errorf("%w: got %q", ErrBadName, h.Name)
	}
	if h.MaxBytes <= 0 {
		h.MaxBytes = DefaultMaxBytes
	}
	if h.AllowOrigin == nil {
		h.AllowOrigin = []string{}
	}
	h.Enabled = true
	h.CreatedAt, h.UpdatedAt = now(), now()
	origins, _ := json.Marshal(h.AllowOrigin)

	_, err := r.db.Exec(`
		INSERT INTO hooks (name, script, enabled, max_bytes, allow_origin, rate_limit, created_at, updated_at)
		VALUES (?,?,1,?,?,?,?,?)`,
		h.Name, h.Script, h.MaxBytes, string(origins), h.RateLimit, h.CreatedAt, h.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Hook{}, ErrExists
		}
		return Hook{}, err
	}
	return withPath(h), nil
}

const cols = `name, script, enabled, max_bytes, allow_origin, rate_limit, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (Hook, error) {
	var h Hook
	var enabled int
	var origins string
	err := row.Scan(&h.Name, &h.Script, &enabled, &h.MaxBytes, &origins, &h.RateLimit,
		&h.CreatedAt, &h.UpdatedAt)
	if err == sql.ErrNoRows {
		return Hook{}, ErrNotFound
	}
	if err != nil {
		return Hook{}, err
	}
	h.Enabled = enabled == 1
	h.AllowOrigin = []string{}
	_ = json.Unmarshal([]byte(origins), &h.AllowOrigin)
	return withPath(h), nil
}

// Get returns one hook.
func (r *Registry) Get(name string) (Hook, error) {
	return scan(r.db.QueryRow(`SELECT `+cols+` FROM hooks WHERE name = ?`, name))
}

// List returns every hook.
func (r *Registry) List() ([]Hook, error) {
	rows, err := r.db.Query(`SELECT ` + cols + ` FROM hooks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Hook{}
	for rows.Next() {
		h, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Update changes only the fields whose pointers are non-nil.
func (r *Registry) Update(name string, script *string, maxBytes *int64, enabled *bool,
	allowOrigin *[]string, rateLimit *int) (Hook, error) {
	h, err := r.Get(name)
	if err != nil {
		return Hook{}, err
	}
	if script != nil {
		h.Script = *script
	}
	if maxBytes != nil && *maxBytes > 0 {
		h.MaxBytes = *maxBytes
	}
	if enabled != nil {
		h.Enabled = *enabled
	}
	if allowOrigin != nil {
		h.AllowOrigin = *allowOrigin
	}
	if rateLimit != nil {
		h.RateLimit = *rateLimit
	}
	h.UpdatedAt = now()

	en := 0
	if h.Enabled {
		en = 1
	}
	origins, _ := json.Marshal(h.AllowOrigin)
	_, err = r.db.Exec(`
		UPDATE hooks SET script=?, max_bytes=?, enabled=?, allow_origin=?, rate_limit=?, updated_at=?
		WHERE name=?`,
		h.Script, h.MaxBytes, en, string(origins), h.RateLimit, h.UpdatedAt, name)
	if err != nil {
		return Hook{}, err
	}
	return h, nil
}

// Delete removes a hook.
func (r *Registry) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM hooks WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delivery is what a hook script receives as its input.
type Delivery struct {
	Hook       string            `json:"hook"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Query      map[string]string `json:"query"`
	Body       string            `json:"body"`
	BodyBase64 string            `json:"body_base64"`
	ReceivedAt string            `json:"received_at"`
}

// Response is what a hook script may return to shape the HTTP reply.
type Response struct {
	Status  int               `json:"status"`
	Body    any               `json:"body"`
	Headers map[string]string `json:"headers"`
}
