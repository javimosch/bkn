package store

import (
	"fmt"
	"strconv"
	"strings"
)

// Retention is a collection's bounded size, declared once rather than trimmed
// by every caller.
//
// The pattern this replaces, from a 131k-line control plane that ran it twice:
//
//	DELETE FROM repo_memories WHERE tag=? AND repo_id=? AND user_id=?
//	  AND id NOT IN (SELECT id FROM repo_memories WHERE tag=? AND repo_id=?
//	                 AND user_id=? ORDER BY created_at DESC LIMIT ?)
//
// plus a scheduled job to run it. Declaring the bound on the collection deletes
// both: the store enforces it on write, so no caller writes the subquery and
// nobody schedules the sweep. That is rule 4 - invariants live in one place -
// applied to lifetime, and it clears the admission rule in VISION.md because it
// removes application code rather than moving a query into bkn.
type Retention struct {
	// Last keeps at most N documents, newest by created_at. Zero is unbounded.
	Last int `json:"last,omitempty"`
	// Per partitions the bound: with Per ["tag"], each distinct tag keeps its
	// own N. Empty bounds the collection as a whole.
	Per []string `json:"per,omitempty"`
}

// IsZero lets `omitzero` drop an unset policy from JSON, so a collection
// without retention looks exactly as it did before this existed.
func (r Retention) IsZero() bool { return r.Last == 0 && len(r.Per) == 0 }

// ErrBadRetention is a policy that cannot be enforced as written.
var ErrBadRetention = fmt.Errorf("invalid retention policy")

// Validate rejects a policy that would silently do nothing or destroy
// everything. A partition without a bound is the dangerous shape: it reads
// like a policy and enforces nothing.
func (r Retention) Validate() error {
	if r.Last < 0 {
		return fmt.Errorf("%w: --retain-last must be zero or more, got %d", ErrBadRetention, r.Last)
	}
	if r.Last == 0 && len(r.Per) > 0 {
		return fmt.Errorf("%w: --retain-per needs --retain-last, or it bounds nothing", ErrBadRetention)
	}
	for _, field := range r.Per {
		if field == "" || strings.ContainsAny(field, "'\"$[]. ") {
			return fmt.Errorf("%w: %q is not a usable partition field", ErrBadRetention, field)
		}
	}
	return nil
}

func (r Retention) encodePer() string { return strings.Join(r.Per, ",") }

func decodeRetention(last int, per string) Retention {
	r := Retention{Last: last}
	for _, f := range strings.Split(per, ",") {
		if f = strings.TrimSpace(f); f != "" {
			r.Per = append(r.Per, f)
		}
	}
	return r
}

// ParseRetention builds a policy from CLI/HTTP text.
func ParseRetention(last string, per []string) (Retention, error) {
	var r Retention
	if last != "" {
		n, err := strconv.Atoi(last)
		if err != nil {
			return r, fmt.Errorf("%w: --retain-last must be a number, got %q", ErrBadRetention, last)
		}
		r.Last = n
	}
	for _, group := range per {
		for _, f := range strings.Split(group, ",") {
			if f = strings.TrimSpace(f); f != "" {
				r.Per = append(r.Per, f)
			}
		}
	}
	return r, r.Validate()
}

// enforce trims the collection - or just the partition the new document landed
// in - down to the bound. It runs after a write, so the newest document is
// always kept and the collection can never sit above its bound for longer than
// one statement.
//
// Ordering is created_at DESC with rowid as the tiebreak, because created_at
// has second resolution: without the tiebreak, documents written inside the
// same second would be evicted in an arbitrary order.
func (s *Store) enforce(r Ref, c Collection, doc map[string]any) (int, error) {
	if c.Retain.Last <= 0 {
		return 0, nil
	}
	where := "ns=? AND coll=?"
	args := []any{r.NS, r.Coll}
	for _, field := range c.Retain.Per {
		value, present := doc[field]
		if !present || value == nil {
			// A document missing the partition field forms its own group,
			// rather than being lumped in with every other incomplete one.
			where += " AND json_extract(doc, ?) IS NULL"
			args = append(args, "$."+field)
			continue
		}
		// Compared as text because json_extract returns SQL types: a numeric
		// field yields INTEGER, and INTEGER 5 is not TEXT '5' in SQLite.
		where += " AND CAST(json_extract(doc, ?) AS TEXT) = ?"
		args = append(args, "$."+field, asText(value))
	}

	query := `DELETE FROM records WHERE ` + where + `
		AND id NOT IN (SELECT id FROM records WHERE ` + where + `
			ORDER BY created_at DESC, rowid DESC LIMIT ?)`
	full := append(append([]any{}, args...), args...)
	full = append(full, c.Retain.Last)

	res, err := s.db.Exec(query, full...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
