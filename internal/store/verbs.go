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

// Op is a filter comparison. The set is deliberately closed: six operators,
// flat and ANDed, one sort field. Anything richer belongs in a script, not in
// a query language grafted onto the store.
type Op string

const (
	OpEq  Op = "eq"
	OpNe  Op = "ne"
	OpGt  Op = "gt"
	OpGte Op = "gte"
	OpLt  Op = "lt"
	OpLte Op = "lte"
	OpIn  Op = "in"
)

func Ops() []string { return []string{"eq", "ne", "gt", "gte", "lt", "lte", "in"} }

var sqlOp = map[Op]string{
	OpEq: "=", OpNe: "!=", OpGt: ">", OpGte: ">=", OpLt: "<", OpLte: "<=",
}

// Filter is one predicate on one field.
type Filter struct {
	Field  string
	Op     Op
	Value  string
	Values []string // OpIn only
}

// ListOptions describes a page of a collection.
type ListOptions struct {
	Filters []Filter
	OrderBy string // a document field; empty means recency
	Desc    bool
	Limit   int
	Offset  int
}

var filterSyntax = []struct {
	token string
	op    Op
}{
	{":in=", OpIn},
	{">=", OpGte},
	{"<=", OpLte},
	{"!=", OpNe},
	{">", OpGt},
	{"<", OpLt},
	{"=", OpEq},
}

// ParseFilter reads "field=value", "field>20", "field:in=a,b" and friends.
//
// The operator is the one that appears EARLIEST in the string, with the
// longest token winning a tie. Scanning operator-by-operator instead would
// split "note=a>b" on the ">" it finds inside the value, because ">" is
// checked before "=" and Cut looks anywhere in the string.
func ParseFilter(s string) (Filter, error) {
	best := -1
	var bestToken string
	var bestOp Op
	for _, syntax := range filterSyntax {
		at := strings.Index(s, syntax.token)
		if at <= 0 {
			continue // not present, or nothing before it to name a field
		}
		if best == -1 || at < best || (at == best && len(syntax.token) > len(bestToken)) {
			best, bestToken, bestOp = at, syntax.token, syntax.op
		}
	}
	if best >= 0 {
		syntax := struct {
			token string
			op    Op
		}{bestToken, bestOp}
		field, value := s[:best], s[best+len(bestToken):]
		f := Filter{Field: field, Op: syntax.op, Value: value}
		if syntax.op == OpIn {
			for _, v := range strings.Split(value, ",") {
				if v = strings.TrimSpace(v); v != "" {
					f.Values = append(f.Values, v)
				}
			}
			if len(f.Values) == 0 {
				return Filter{}, fmt.Errorf("%q: :in= needs at least one value", s)
			}
		}
		return f, nil
	}
	return Filter{}, fmt.Errorf("filter must be field=value, field>value, field:in=a,b, "+
		"or use != >= <=, got %q", s)
}

// bindValue converts a filter's text value to the type json_extract returns
// for that field, so `--where age=30` matches the number 30 and
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

// whereClause builds the predicate shared by List, Find and Count.
func (s *Store) whereClause(c Collection, filters []Filter) (string, []any, error) {
	var sb strings.Builder
	var args []any

	// The collection's normalizer applies to filter values too, so a lookup
	// by email finds the row a normalized write created.
	normalizeValue := func(field, value string) (string, error) {
		rule, ok := c.Normalize[field]
		if !ok {
			return value, nil
		}
		return normalize(rule, value)
	}

	for _, f := range filters {
		// The id is a column, not a document field: splitID strips it before
		// writing and decode merges it back on read. A filter on it therefore
		// has to target the column, or json_extract returns NULL and nothing
		// ever matches - which silently broke every attempt to batch-resolve
		// a set of ids.
		if f.Field == "id" {
			if f.Op == OpIn {
				placeholders := make([]string, 0, len(f.Values))
				for _, v := range f.Values {
					placeholders = append(placeholders, "?")
					args = append(args, v)
				}
				sb.WriteString(" AND id IN (" + strings.Join(placeholders, ",") + ")")
				continue
			}
			operator, ok := sqlOp[f.Op]
			if !ok {
				return "", nil, fmt.Errorf("unsupported operator %q", f.Op)
			}
			sb.WriteString(" AND id " + operator + " ?")
			args = append(args, f.Value)
			continue
		}

		path := "$." + f.Field
		if f.Op == OpIn {
			placeholders := make([]string, 0, len(f.Values))
			args = append(args, path)
			for _, raw := range f.Values {
				v, err := normalizeValue(f.Field, raw)
				if err != nil {
					return "", nil, err
				}
				placeholders = append(placeholders, "?")
				args = append(args, bindValue(v))
			}
			sb.WriteString(" AND json_extract(doc, ?) IN (" + strings.Join(placeholders, ",") + ")")
			continue
		}
		operator, ok := sqlOp[f.Op]
		if !ok {
			return "", nil, fmt.Errorf("unsupported operator %q", f.Op)
		}
		v, err := normalizeValue(f.Field, f.Value)
		if err != nil {
			return "", nil, err
		}
		sb.WriteString(" AND json_extract(doc, ?) " + operator + " ?")
		args = append(args, path, bindValue(v))
	}
	return sb.String(), args, nil
}

// orderClause sorts by a document field, or by recency when none is given.
//
// Records missing the field sort last in both directions: SQLite puts NULL
// first ascending, which would otherwise bury every populated row beneath the
// ones that do not have the field at all.
func orderClause(field string, desc bool) (string, []any) {
	direction := "ASC"
	if desc {
		direction = "DESC"
	}
	if field == "" {
		return " ORDER BY updated_at " + direction + ", id " + direction, nil
	}
	path := "$." + field
	return " ORDER BY (json_extract(doc, ?) IS NULL), json_extract(doc, ?) " +
		direction + ", id " + direction, []any{path, path}
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

// PutIfAbsent inserts a document only when its id is free, and reports
// whether it did.
//
// This is the idempotency primitive: a webhook that arrives twice with the
// same provider event id must be processed once, and a get-then-put has a
// window between the two where a retry slips through.
func (s *Store) PutIfAbsent(r Ref, id string, doc map[string]any) (Record, bool, error) {
	if doc == nil {
		return nil, false, ErrBadDoc
	}
	c, err := s.EnsureCollection(r, nil)
	if err != nil {
		return nil, false, err
	}
	id, doc = splitID(doc, id)
	if err := applyNormalizers(c.Normalize, doc); err != nil {
		return nil, false, err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, false, ErrBadDoc
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`
		INSERT INTO records (ns, coll, id, doc, created_at, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(ns, coll, id) DO NOTHING`,
		r.NS, r.Coll, id, string(b), now, now)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := s.Get(r, id)
		return existing, false, err
	}
	rec, err := decode(id, string(b))
	return rec, true, err
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
	recs, err := s.List(r, ListOptions{Filters: filters, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, ErrNotFound
	}
	return recs[0], nil
}

// List returns a page of documents.
func (s *Store) List(r Ref, opts ListOptions) ([]Record, error) {
	c, err := s.getCollection(r)
	if err == ErrNoCollection {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	where, whereArgs, err := s.whereClause(c, opts.Filters)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	order, orderArgs := orderClause(opts.OrderBy, opts.Desc || opts.OrderBy == "")

	args := append([]any{r.NS, r.Coll}, whereArgs...)
	args = append(args, orderArgs...)
	args = append(args, limit, opts.Offset)

	rows, err := s.db.Query(`SELECT id, doc FROM records WHERE ns=? AND coll=?`+
		where+order+` LIMIT ? OFFSET ?`, args...)
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

// Count reports how many documents match, which is what a paginated list needs
// to say "page 1 of 12" rather than only "here are 50".
func (s *Store) Count(r Ref, filters []Filter) (int, error) {
	c, err := s.getCollection(r)
	if err == ErrNoCollection {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	where, args, err := s.whereClause(c, filters)
	if err != nil {
		return 0, err
	}
	var total int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE ns=? AND coll=?`+where,
		append([]any{r.NS, r.Coll}, args...)...).Scan(&total)
	return total, err
}

// Patch shallow-merges fields into an existing document ($set semantics).
// Nested objects are replaced wholesale, not deep-merged - the observed
// consumers only ever set top-level fields.
//
// A field value may instead be an operator expression such as {"$inc": 1} or
// {"$append": "line\n"}, which is computed from the field's current value.
// See atomic.go for why those exist.
func (s *Store) Patch(r Ref, id string, fields map[string]any) (Record, error) {
	return s.PatchWith(r, id, fields, PatchOptions{})
}

// patchAttempts bounds the compare-and-set retry. Contention on one document
// is expected to be rare; a document hot enough to lose five races in a row is
// a design problem in the caller, and reporting that beats spinning.
const patchAttempts = 5

// PatchWith is Patch with preconditions.
//
// The write is a compare-and-set against the exact document the merge was
// computed from, so a concurrent writer cannot be silently overwritten - which
// is what the previous read-merge-write did. On a lost race the document is
// re-read and the merge recomputed, so two patches of different fields both
// survive instead of one erasing the other.
func (s *Store) PatchWith(r Ref, id string, fields map[string]any, opts PatchOptions) (Record, error) {
	if fields == nil {
		return nil, ErrBadDoc
	}
	c, err := s.getCollection(r)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < patchAttempts; attempt++ {
		var prev string
		err := s.db.QueryRow(`SELECT doc FROM records WHERE ns=? AND coll=? AND id=?`,
			r.NS, r.Coll, id).Scan(&prev)
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		cur, err := decode(id, prev)
		if err != nil {
			return nil, err
		}
		doc := map[string]any(cur)
		delete(doc, "id")

		if !opts.empty() {
			if err := opts.check(doc); err != nil {
				return nil, err
			}
		}

		for k, v := range fields {
			if k == "id" {
				continue // ids are immutable; use Put with a new id instead
			}
			if op, operand, isOp := asOperator(v); isOp {
				next, err := applyOperator(op, doc[k], operand)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", k, err)
				}
				doc[k] = next
				continue
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

		// Only write if the document is still the one we merged into. This is
		// the same compare-and-set cron's claim() uses, and it is what makes
		// the operators above atomic across processes rather than merely
		// convenient within one.
		res, err := s.db.Exec(
			`UPDATE records SET doc=?, updated_at=? WHERE ns=? AND coll=? AND id=? AND doc=?`,
			string(b), time.Now().UTC().Format(time.RFC3339), r.NS, r.Coll, id, prev)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			return decode(id, string(b))
		}
		// Somebody else wrote between the read and the write: read again and
		// recompute, so an operator applies to the value that actually won.
	}
	return nil, ErrConcurrent
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
