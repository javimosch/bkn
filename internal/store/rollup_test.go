package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

func rollupSetup(t *testing.T) (*store.Store, store.Ref) {
	t.Helper()
	st := newStore(t)
	r := ref(t, "app/runs")
	seed := []map[string]any{
		{"status": "ok", "repo": "a"},
		{"status": "ok", "repo": "a"},
		{"status": "ok", "repo": "b"},
		{"status": "failed", "repo": "a"},
		{"status": "stale", "repo": "b"},
		{"repo": "b"}, // no status at all
	}
	for i, doc := range seed {
		if _, err := st.Put(r, fmt.Sprintf("r%d", i), doc); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	return st, r
}

func buckets(rollup store.Rollup) map[string]int {
	out := map[string]int{}
	for _, b := range rollup.Buckets {
		if b.Key == nil {
			out["<absent>"] = b.Count
			continue
		}
		out[*b.Key] = b.Count
	}
	return out
}

func TestCountByGroupsAndOrders(t *testing.T) {
	st, r := rollupSetup(t)

	rollup, err := st.CountBy(r, nil, "status", 0)
	if err != nil {
		t.Fatalf("CountBy: %v", err)
	}
	got := buckets(rollup)
	want := map[string]int{"ok": 3, "failed": 1, "stale": 1, "<absent>": 1}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("bucket %q = %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if rollup.Total != 6 {
		t.Errorf("total = %d, want 6", rollup.Total)
	}
	if rollup.Groups != 4 {
		t.Errorf("groups = %d, want 4", rollup.Groups)
	}
	// Largest first, matching `events stats`.
	if rollup.Buckets[0].Key == nil || *rollup.Buckets[0].Key != "ok" {
		t.Errorf("first bucket = %+v, want the largest (ok)", rollup.Buckets[0])
	}
	if rollup.Truncated() {
		t.Error("reported truncated with every group returned")
	}
}

// A document with no value for the field is its own answer, not an empty
// string: "absent" and "" are different and are reported differently.
func TestCountByDistinguishesAbsentFromEmpty(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/x")
	if _, err := st.Put(r, "absent", map[string]any{"other": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(r, "empty", map[string]any{"status": ""}); err != nil {
		t.Fatal(err)
	}
	rollup, err := st.CountBy(r, nil, "status", 0)
	if err != nil {
		t.Fatalf("CountBy: %v", err)
	}
	if rollup.Groups != 2 {
		t.Fatalf("groups = %d, want 2 — absent and empty must not merge: %v", rollup.Groups, buckets(rollup))
	}
	var sawNil, sawEmpty bool
	for _, b := range rollup.Buckets {
		if b.Key == nil {
			sawNil = true
		} else if *b.Key == "" {
			sawEmpty = true
		}
	}
	if !sawNil || !sawEmpty {
		t.Errorf("buckets = %+v, want one null key and one empty-string key", rollup.Buckets)
	}
}

// This is the query the fit-check found: counts by status, narrowed by a
// filter. The filter must apply before the grouping.
func TestCountByComposesWithFilters(t *testing.T) {
	st, r := rollupSetup(t)
	rollup, err := st.CountBy(r, []store.Filter{{Field: "repo", Op: "eq", Value: "a"}}, "status", 0)
	if err != nil {
		t.Fatalf("CountBy: %v", err)
	}
	got := buckets(rollup)
	if got["ok"] != 2 || got["failed"] != 1 {
		t.Errorf("filtered buckets = %v, want ok=2 failed=1", got)
	}
	if rollup.Total != 3 {
		t.Errorf("total = %d, want 3 — the filter must apply before grouping", rollup.Total)
	}
}

// A numeric field and its string form must land in one group, not two.
func TestCountByNormalisesNumbersToText(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/n")
	if _, err := st.Put(r, "a", map[string]any{"repo_id": float64(7)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(r, "b", map[string]any{"repo_id": float64(7)}); err != nil {
		t.Fatal(err)
	}
	rollup, err := st.CountBy(r, nil, "repo_id", 0)
	if err != nil {
		t.Fatalf("CountBy: %v", err)
	}
	if rollup.Groups != 1 || len(rollup.Buckets) != 1 || rollup.Buckets[0].Count != 2 {
		t.Errorf("rollup = %+v, want a single group of 2", rollup)
	}
}

// Grouping by a high-cardinality field is a mistake worth making cheap: the
// buckets are capped but Groups still tells the truth.
func TestCountByCapsBucketsButReportsTrueCardinality(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/many")
	for i := 0; i < 25; i++ {
		if _, err := st.Put(r, fmt.Sprintf("d%d", i), map[string]any{"uniq": fmt.Sprintf("v%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	rollup, err := st.CountBy(r, nil, "uniq", 5)
	if err != nil {
		t.Fatalf("CountBy: %v", err)
	}
	if len(rollup.Buckets) != 5 {
		t.Errorf("returned %d buckets, want the limit of 5", len(rollup.Buckets))
	}
	if rollup.Groups != 25 {
		t.Errorf("groups = %d, want the true cardinality 25", rollup.Groups)
	}
	if !rollup.Truncated() {
		t.Error("Truncated() = false with 5 of 25 groups returned")
	}
}

func TestCountByRejectsUnusableFields(t *testing.T) {
	st, r := rollupSetup(t)
	for _, bad := range []string{"", "a b", "$.status", "a.b", "it's"} {
		if _, err := st.CountBy(r, nil, bad, 0); !errors.Is(err, store.ErrBadGroupBy) {
			t.Errorf("CountBy(%q) = %v, want ErrBadGroupBy", bad, err)
		}
	}
}

func TestCountByOnAMissingCollectionIsEmptyNotAnError(t *testing.T) {
	st := newStore(t)
	rollup, err := st.CountBy(ref(t, "app/nope"), nil, "status", 0)
	if err != nil {
		t.Fatalf("CountBy on a missing collection: %v", err)
	}
	if rollup.Total != 0 || len(rollup.Buckets) != 0 {
		t.Errorf("rollup = %+v, want empty", rollup)
	}
}
