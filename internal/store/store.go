// Package store is the collection primitive.
//
// Scope is deliberate. An audit of every real consumer of the Node backend
// (polybot, la-chatiere-portal, git-grep-api, enbauges-platform) found exactly
// six operations in use and zero aggregations, joins, or server-side sorts.
// This package implements those six. Anything richer belongs in a script, not
// in the core.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrNoCollection  = errors.New("collection does not exist")
	ErrBadRef        = errors.New("collection ref must be <namespace>/<collection>")
	ErrBadDoc        = errors.New("document must be a JSON object")
	ErrBadNormalizer = errors.New("unknown normalizer")
)

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// Ref is a namespaced collection address.
//
// Requirement R1: every collection lives under an owning namespace, so no
// consumer ever has to probe candidate table names to find its own data.
type Ref struct {
	NS   string
	Coll string
}

// ParseRef parses "app/collection".
func ParseRef(s string) (Ref, error) {
	ns, coll, ok := strings.Cut(s, "/")
	if !ok {
		return Ref{}, ErrBadRef
	}
	if !identRe.MatchString(ns) || !identRe.MatchString(coll) {
		return Ref{}, fmt.Errorf("%w: segments must match %s", ErrBadRef, identRe)
	}
	return Ref{NS: ns, Coll: coll}, nil
}

func (r Ref) String() string { return r.NS + "/" + r.Coll }

// Store is the six-verb collection API over SQLite.
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

// Ping checks the datastore is reachable. It is deliberately trivial: a
// liveness probe should notice a missing database, not measure it.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Collection describes a collection and its enforced field normalizers.
type Collection struct {
	Ref       string            `json:"ref"`
	Normalize map[string]string `json:"normalize"`
	CreatedAt string            `json:"created_at"`
	Count     int               `json:"count,omitempty"`
}

// --- normalization (requirement R4) ---------------------------------------
//
// The Node backend lowercased user emails in the server AND again in every
// consumer, by convention. Duplicated invariants drift. Here the rule is
// declared once on the collection and applied by the store on every write and
// on every filter value for the same field, so a caller cannot get it wrong.

func normalize(rule, v string) (string, error) {
	switch rule {
	case "lower":
		return strings.ToLower(v), nil
	case "upper":
		return strings.ToUpper(v), nil
	case "trim":
		return strings.TrimSpace(v), nil
	case "trim_lower":
		return strings.ToLower(strings.TrimSpace(v)), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrBadNormalizer, rule)
	}
}

func ValidNormalizers() []string { return []string{"lower", "upper", "trim", "trim_lower"} }

func applyNormalizers(rules map[string]string, doc map[string]any) error {
	for field, rule := range rules {
		raw, ok := doc[field]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue // normalizers apply to strings only; other types pass through
		}
		n, err := normalize(rule, s)
		if err != nil {
			return err
		}
		doc[field] = n
	}
	return nil
}

// --- collections ----------------------------------------------------------

// EnsureCollection creates the collection if absent and returns its rules.
// Passing normalize rules on an existing collection updates them.
func (s *Store) EnsureCollection(r Ref, rules map[string]string) (Collection, error) {
	for _, rule := range rules {
		if _, err := normalize(rule, ""); err != nil {
			return Collection{}, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if len(rules) > 0 {
		b, _ := json.Marshal(rules)
		_, err := s.db.Exec(`
			INSERT INTO collections (ns, name, normalize, created_at) VALUES (?,?,?,?)
			ON CONFLICT(ns, name) DO UPDATE SET normalize = excluded.normalize`,
			r.NS, r.Coll, string(b), now)
		if err != nil {
			return Collection{}, err
		}
	} else {
		_, err := s.db.Exec(`
			INSERT INTO collections (ns, name, normalize, created_at) VALUES (?,?,'{}',?)
			ON CONFLICT(ns, name) DO NOTHING`, r.NS, r.Coll, now)
		if err != nil {
			return Collection{}, err
		}
	}
	return s.getCollection(r)
}

func (s *Store) getCollection(r Ref) (Collection, error) {
	var raw, created string
	err := s.db.QueryRow(`SELECT normalize, created_at FROM collections WHERE ns=? AND name=?`,
		r.NS, r.Coll).Scan(&raw, &created)
	if err == sql.ErrNoRows {
		return Collection{}, ErrNoCollection
	}
	if err != nil {
		return Collection{}, err
	}
	rules := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &rules)
	return Collection{Ref: r.String(), Normalize: rules, CreatedAt: created}, nil
}

// Collections lists every collection, optionally filtered to one namespace.
func (s *Store) Collections(ns string) ([]Collection, error) {
	q := `SELECT c.ns, c.name, c.normalize, c.created_at,
	             (SELECT COUNT(*) FROM records r WHERE r.ns=c.ns AND r.coll=c.name)
	      FROM collections c`
	var args []any
	if ns != "" {
		q += ` WHERE c.ns = ?`
		args = append(args, ns)
	}
	q += ` ORDER BY c.ns, c.name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Collection{}
	for rows.Next() {
		var n, name, raw, created string
		var count int
		if err := rows.Scan(&n, &name, &raw, &created, &count); err != nil {
			return nil, err
		}
		rules := map[string]string{}
		_ = json.Unmarshal([]byte(raw), &rules)
		out = append(out, Collection{Ref: n + "/" + name, Normalize: rules, CreatedAt: created, Count: count})
	}
	return out, rows.Err()
}
