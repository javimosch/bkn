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
	Retain    Retention         `json:"retain,omitzero"`
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

// ErrBadNormalizerField is a normalizer declared on a field path the store
// cannot address.
var ErrBadNormalizerField = errors.New("invalid normalizer field")

// validNormalizerField rejects a path the store could accept and then quietly
// fail to apply. Dots separate object keys; anything else that would need a
// real JSON-path parser - array indices, quoting, wildcards - is refused at
// declaration time rather than ignored at write time.
func validNormalizerField(field string) error {
	if field == "" {
		return fmt.Errorf("%w: empty field name", ErrBadNormalizerField)
	}
	if strings.ContainsAny(field, "$[]'\"* ") {
		return fmt.Errorf("%w: %q - only object keys separated by dots are supported", ErrBadNormalizerField, field)
	}
	for _, segment := range strings.Split(field, ".") {
		if segment == "" {
			return fmt.Errorf("%w: %q has an empty path segment", ErrBadNormalizerField, field)
		}
	}
	return nil
}

// applyNormalizers rewrites declared fields in place.
//
// A field may name a nested key with dots - "declarant.email" - because the
// filter side already addresses documents that way: whereClause builds
// "$."+field for json_extract, and normalizes the filter value using this same
// rule map. Before this walked the path, the two halves disagreed: a lookup
// value was normalized while the stored value was not, so a document was
// unfindable by the very field its collection declared as normalized.
func applyNormalizers(rules map[string]string, doc map[string]any) error {
	for field, rule := range rules {
		// A literal key wins when one exists, so a document that really does
		// hold a key containing a dot keeps behaving as it did.
		if raw, ok := doc[field]; ok {
			n, err := normalizeIfString(rule, raw)
			if err != nil {
				return err
			}
			if n != nil {
				doc[field] = n
			}
			continue
		}

		segments := strings.Split(field, ".")
		if len(segments) == 1 {
			continue // absent top-level field: nothing to normalize
		}
		parent := doc
		for _, segment := range segments[:len(segments)-1] {
			next, ok := parent[segment].(map[string]any)
			if !ok {
				parent = nil // path does not exist, or is not an object
				break
			}
			parent = next
		}
		if parent == nil {
			continue
		}
		leaf := segments[len(segments)-1]
		raw, ok := parent[leaf]
		if !ok {
			continue
		}
		n, err := normalizeIfString(rule, raw)
		if err != nil {
			return err
		}
		if n != nil {
			parent[leaf] = n
		}
	}
	return nil
}

// normalizeIfString returns the normalized value, or nil when the value is not
// a string - normalizers apply to strings only; other types pass through.
func normalizeIfString(rule string, raw any) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return nil, nil
	}
	n, err := normalize(rule, s)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// --- collections ----------------------------------------------------------

// EnsureCollection creates the collection if absent and returns its rules.
// Passing normalize rules on an existing collection updates them.
func (s *Store) EnsureCollection(r Ref, rules map[string]string) (Collection, error) {
	return s.EnsureCollectionWith(r, rules, Retention{}, false)
}

// EnsureCollectionWith also sets a retention policy. setRetain distinguishes
// "no policy given" from "policy explicitly cleared", so an ordinary write -
// which calls EnsureCollection - can never silently drop the bound somebody
// declared.
func (s *Store) EnsureCollectionWith(r Ref, rules map[string]string, retain Retention, setRetain bool) (Collection, error) {
	for field, rule := range rules {
		if err := validNormalizerField(field); err != nil {
			return Collection{}, err
		}
		if _, err := normalize(rule, ""); err != nil {
			return Collection{}, err
		}
	}
	if setRetain {
		if err := retain.Validate(); err != nil {
			return Collection{}, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b := []byte("{}")
	if len(rules) > 0 {
		b, _ = json.Marshal(rules)
	}

	sets := []string{}
	if len(rules) > 0 {
		sets = append(sets, "normalize = excluded.normalize")
	}
	if setRetain {
		sets = append(sets, "retain_last = excluded.retain_last", "retain_per = excluded.retain_per")
	}
	conflict := "DO NOTHING"
	if len(sets) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(sets, ", ")
	}
	_, err := s.db.Exec(`
		INSERT INTO collections (ns, name, normalize, retain_last, retain_per, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(ns, name) `+conflict,
		r.NS, r.Coll, string(b), retain.Last, retain.encodePer(), now)
	if err != nil {
		return Collection{}, err
	}

	c, err := s.getCollection(r)
	if err != nil {
		return c, err
	}
	// A policy set on a collection that already holds documents applies now,
	// not at the next write. Otherwise declaring a bound on a large collection
	// looks like it did nothing.
	if setRetain && c.Retain.Last > 0 && len(c.Retain.Per) == 0 {
		if _, err := s.enforce(r, c, nil); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (s *Store) getCollection(r Ref) (Collection, error) {
	var raw, created, per string
	var last int
	err := s.db.QueryRow(`SELECT normalize, retain_last, retain_per, created_at FROM collections WHERE ns=? AND name=?`,
		r.NS, r.Coll).Scan(&raw, &last, &per, &created)
	if err == sql.ErrNoRows {
		return Collection{}, ErrNoCollection
	}
	if err != nil {
		return Collection{}, err
	}
	rules := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &rules)
	return Collection{Ref: r.String(), Normalize: rules, Retain: decodeRetention(last, per), CreatedAt: created}, nil
}

// Collections lists every collection, optionally filtered to one namespace.
func (s *Store) Collections(ns string) ([]Collection, error) {
	q := `SELECT c.ns, c.name, c.normalize, c.retain_last, c.retain_per, c.created_at,
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
		var n, name, raw, per, created string
		var last, count int
		if err := rows.Scan(&n, &name, &raw, &last, &per, &created, &count); err != nil {
			return nil, err
		}
		rules := map[string]string{}
		_ = json.Unmarshal([]byte(raw), &rules)
		out = append(out, Collection{Ref: n + "/" + name, Normalize: rules,
			Retain: decodeRetention(last, per), CreatedAt: created, Count: count})
	}
	return out, rows.Err()
}
