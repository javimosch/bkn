package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Record is a stored document. The id is merged into the returned document
// under the "id" key so an agent gets one flat object, never a wrapper it has
// to unpick.
type Record map[string]any

// Filter is one field equality predicate: the only comparison any observed
// consumer used.
type Filter struct {
	Field string
	Value string
}

// ParseFilter parses "field=value".
func ParseFilter(s string) (Filter, error) {
	f, v, ok := strings.Cut(s, "=")
	if !ok || f == "" {
		return Filter{}, fmt.Errorf("filter must be field=value, got %q", s)
	}
	return Filter{Field: f, Value: v}, nil
}

// bindValue converts a filter's text value to the type json_extract will
// return for that field, so `--where age=30` matches the number 30 and
// `--where active=true` matches the boolean.
func bindValue(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return n
	}
	return v
}

func (s *Store) whereClause(c Collection, filters []Filter) (string, []any, error) {
	var sb strings.Builder
	var args []any
	for _, f := range filters {
		val := f.Value
		// The collection's normalizer applies to the filter value too, so a
		// lookup by email finds the row a normalized write created.
		if rule, ok := c.Normalize[f.Field]; ok {
			n, err := normalize(rule, val)
			if err != nil {
				return "", nil, err
			}
			val = n
		}
		sb.WriteString(" AND json_extract(doc, ?) = ?")
		args = append(args, "$."+f.Field, bindValue(val))
	}
	return sb.String(), args, nil
}

func decode(id, doc string) (Record, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	m["id"] = id
	return m, nil
}

// splitID pulls a caller-supplied id out of the document body. Requirement R3:
// a caller may mint the id itself, either via the explicit id argument or by
// including "id" in the document.
func splitID(doc map[string]any, explicit string) (string, map[string]any) {
	id := explicit
	if v, ok := doc["id"]; ok {
		if id == "" {
			if s, ok := v.(string); ok {
				id = s
			}
		}
		delete(doc, "id")
	}
	if id == "" {
		id = NewID()
	}
	return id, doc
}

// --- the six verbs --------------------------------------------------------

// Put inserts or replaces a whole document. The collection is created on first
// write.
func (s *Store) Put(r Ref, id string, doc map[string]any) (Record, error) {
	if doc == nil {
		return nil, ErrBadDoc
	}
	c, err := s.EnsureCollection(r, nil)
	if err != nil {
		return nil, err
	}
	id, doc = splitID(doc, id)
	if err := applyNormalizers(c.Normalize, doc); err != nil {
		return nil, err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, ErrBadDoc
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(`
		INSERT INTO records (ns, coll, id, doc, created_at, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(ns, coll, id) DO UPDATE SET doc = excluded.doc, updated_at = excluded.updated_at`,
		r.NS, r.Coll, id, string(b), now, now)
	if err != nil {
		return nil, err
	}
	return decode(id, string(b))
}

// Get returns one document by id.
func (s *Store) Get(r Ref, id string) (Record, error) {
	var doc string
	err := s.db.QueryRow(`SELECT doc FROM records WHERE ns=? AND coll=? AND id=?`,
		r.NS, r.Coll, id).Scan(&doc)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(id, doc)
}

// Find returns the first document matching every filter, or ErrNotFound.
func (s *Store) Find(r Ref, filters []Filter) (Record, error) {
	recs, err := s.List(r, filters, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, ErrNotFound
	}
	return recs[0], nil
}

// List returns documents matching every filter, newest first.
func (s *Store) List(r Ref, filters []Filter, limit, offset int) ([]Record, error) {
	c, err := s.getCollection(r)
	if err == ErrNoCollection {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	where, args, err := s.whereClause(c, filters)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, doc FROM records WHERE ns=? AND coll=?` + where +
		` ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`
	full := append([]any{r.NS, r.Coll}, args...)
	full = append(full, limit, offset)

	rows, err := s.db.Query(q, full...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Record{}
	for rows.Next() {
		var id, doc string
		if err := rows.Scan(&id, &doc); err != nil {
			return nil, err
		}
		rec, err := decode(id, doc)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Patch shallow-merges fields into an existing document ($set semantics).
// Nested objects are replaced wholesale, not deep-merged - the observed
// consumers only ever set top-level fields.
func (s *Store) Patch(r Ref, id string, fields map[string]any) (Record, error) {
	if fields == nil {
		return nil, ErrBadDoc
	}
	c, err := s.getCollection(r)
	if err != nil {
		return nil, err
	}
	cur, err := s.Get(r, id)
	if err != nil {
		return nil, err
	}
	doc := map[string]any(cur)
	delete(doc, "id")
	for k, v := range fields {
		if k == "id" {
			continue // ids are immutable; use Put with a new id instead
		}
		doc[k] = v
	}
	if err := applyNormalizers(c.Normalize, doc); err != nil {
		return nil, err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, ErrBadDoc
	}
	_, err = s.db.Exec(`UPDATE records SET doc=?, updated_at=? WHERE ns=? AND coll=? AND id=?`,
		string(b), time.Now().UTC().Format(time.RFC3339), r.NS, r.Coll, id)
	if err != nil {
		return nil, err
	}
	return decode(id, string(b))
}

// Delete removes one document by id.
func (s *Store) Delete(r Ref, id string) error {
	res, err := s.db.Exec(`DELETE FROM records WHERE ns=? AND coll=? AND id=?`, r.NS, r.Coll, id)
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
	return nil
}
