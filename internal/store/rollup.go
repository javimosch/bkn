package store

import (
	"fmt"
	"strings"
)

// Counting documents by a field is the one summarisation a control plane
// cannot do without: "how many runs in each status" is a dashboard's first
// question, and the alternative is listing the collection and counting in the
// caller.
//
// That alternative is exactly what rule 2 already rejected when it admitted
// ordering and ranges: "doing them in a script means loading the collection
// into the VM". A rollup is admitted on the same ground, and `events` made the
// same call first - Stats() groups a log by one field. A store is no less
// entitled to a rollup than a log is.
//
// It stays a summarisation rather than a query language: one field, one
// aggregate, no expressions, no nesting, no having, no joins. It returns
// counts, never documents.

// Bucket is one group and its size. Key is nil when the documents in the group
// have no value for the field - absent and empty string are different answers
// and are reported differently.
type Bucket struct {
	Key   *string `json:"key"`
	Count int     `json:"count"`
}

// Rollup is a grouped count.
type Rollup struct {
	By      string   `json:"by"`
	Total   int      `json:"total"`   // documents matching the filters
	Groups  int      `json:"groups"`  // distinct values, before any limit
	Buckets []Bucket `json:"buckets"` // largest first, capped by limit
}

// Truncated reports whether buckets omits groups the caller did not see.
func (r Rollup) Truncated() bool { return len(r.Buckets) < r.Groups }

// ErrBadGroupBy is a field that cannot be grouped by.
var ErrBadGroupBy = fmt.Errorf("invalid group-by field")

// DefaultRollupLimit caps the buckets returned. Unlike an event log's fixed
// columns, a document field can hold a distinct value per document - grouping
// by an id would otherwise return a bucket per row. The cap keeps a mistake
// cheap; Groups still reports the true cardinality, so the caller can tell.
const DefaultRollupLimit = 1000

func validGroupField(field string) error {
	if field == "" || strings.ContainsAny(field, "'\"$[]. ") {
		return fmt.Errorf("%w: %q is not a usable field name", ErrBadGroupBy, field)
	}
	return nil
}

// CountBy groups matching documents by one field and counts each group.
func (s *Store) CountBy(r Ref, filters []Filter, field string, limit int) (Rollup, error) {
	if err := validGroupField(field); err != nil {
		return Rollup{}, err
	}
	if limit <= 0 {
		limit = DefaultRollupLimit
	}
	out := Rollup{By: field, Buckets: []Bucket{}}

	c, err := s.getCollection(r)
	if err == ErrNoCollection {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	where, args, err := s.whereClause(c, filters)
	if err != nil {
		return out, err
	}

	// CAST to TEXT because json_extract returns SQL types: without it a
	// numeric field and its string form would land in different groups.
	key := `CAST(json_extract(doc, ?) AS TEXT)`
	path := "$." + field
	// Args bind by position. The bucket query has one path placeholder before
	// ns/coll; the cardinality query has two, because the key expression
	// appears twice in its SELECT list.
	bucketArgs := append([]any{path, r.NS, r.Coll}, args...)
	countArgs := append([]any{path, path, r.NS, r.Coll}, args...)

	// Placeholders in order: the JSON path, ns, coll, then the filter args.
	//
	// COUNT(DISTINCT x) does not count NULL, so documents with no value for
	// the field form a bucket that the cardinality would miss. COUNT(x)
	// likewise skips them, so comparing it against COUNT(*) detects the group
	// and the comparison yields 1 or 0 to add.
	err = s.db.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT `+key+`) + (COUNT(*) > COUNT(`+key+`))
		 FROM records WHERE ns=? AND coll=?`+where,
		countArgs...,
	).Scan(&out.Total, &out.Groups)
	if err != nil {
		return out, err
	}

	rows, err := s.db.Query(
		`SELECT `+key+` AS k, COUNT(*) FROM records WHERE ns=? AND coll=?`+where+`
		 GROUP BY k ORDER BY COUNT(*) DESC, k LIMIT ?`,
		append(bucketArgs, limit)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	for rows.Next() {
		var k *string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return out, err
		}
		out.Buckets = append(out.Buckets, Bucket{Key: k, Count: n})
	}
	return out, rows.Err()
}
