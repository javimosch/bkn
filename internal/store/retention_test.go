package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

func ids(t *testing.T, st *store.Store, r store.Ref) []string {
	t.Helper()
	recs, err := st.List(r, store.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec["id"].(string))
	}
	return out
}

func TestRetainLastKeepsTheNewest(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/runs")
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 3}, true); err != nil {
		t.Fatalf("EnsureCollectionWith: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := st.Put(r, fmt.Sprintf("r%02d", i), map[string]any{"n": i}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	got := ids(t, st, r)
	if len(got) != 3 {
		t.Fatalf("kept %d documents (%v), want 3", len(got), got)
	}
	// created_at has second resolution, so ten writes share a timestamp and
	// insertion order is what decides. The last three written must survive.
	for _, want := range []string{"r07", "r08", "r09"} {
		found := false
		for _, id := range got {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was evicted; kept %v", want, got)
		}
	}
}

func TestRetainPerPartitionsTheBound(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/memories")
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 2, Per: []string{"tag"}}, true); err != nil {
		t.Fatalf("EnsureCollectionWith: %v", err)
	}

	for _, tag := range []string{"RUN", "PROPOSAL"} {
		for i := 0; i < 5; i++ {
			if _, err := st.Put(r, fmt.Sprintf("%s-%d", tag, i), map[string]any{"tag": tag, "n": i}); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}

	counts := map[string]int{}
	recs, _ := st.List(r, store.ListOptions{Limit: 1000})
	for _, rec := range recs {
		counts[rec["tag"].(string)]++
	}
	if counts["RUN"] != 2 || counts["PROPOSAL"] != 2 {
		t.Errorf("per-tag counts = %v, want 2 each — the bound is per partition, not per collection", counts)
	}
	if len(recs) != 4 {
		t.Errorf("total = %d, want 4", len(recs))
	}
}

// A document with no value for the partition field forms its own group rather
// than being pooled with every other incomplete document.
func TestRetainPerTreatsMissingFieldAsItsOwnGroup(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/memories")
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 1, Per: []string{"tag"}}, true); err != nil {
		t.Fatalf("EnsureCollectionWith: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.Put(r, fmt.Sprintf("tagged-%d", i), map[string]any{"tag": "RUN"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Put(r, fmt.Sprintf("bare-%d", i), map[string]any{"other": i}); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := st.List(r, store.ListOptions{Limit: 100})
	if len(recs) != 2 {
		t.Errorf("kept %d, want 2 (one tagged, one untagged): %v", len(recs), ids(t, st, r))
	}
}

// Numbers survive the text comparison used to match a partition: json_extract
// returns INTEGER for a numeric field, and INTEGER 5 is not TEXT '5' in SQLite.
func TestRetainPerWorksOnNumericFields(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/events")
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 1, Per: []string{"repo_id"}}, true); err != nil {
		t.Fatalf("EnsureCollectionWith: %v", err)
	}
	for _, repo := range []float64{1, 2} {
		for i := 0; i < 3; i++ {
			if _, err := st.Put(r, fmt.Sprintf("r%v-%d", repo, i), map[string]any{"repo_id": repo}); err != nil {
				t.Fatal(err)
			}
		}
	}
	recs, _ := st.List(r, store.ListOptions{Limit: 100})
	if len(recs) != 2 {
		t.Errorf("kept %d, want one per repo_id: %v", len(recs), ids(t, st, r))
	}
}

// Declaring a bound on a collection that already holds documents applies now,
// not at the next write — otherwise it looks like it did nothing.
func TestPolicyAppliesToAnExistingCollection(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/logs")
	for i := 0; i < 8; i++ {
		if _, err := st.Put(r, fmt.Sprintf("l%d", i), map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(ids(t, st, r)); got != 8 {
		t.Fatalf("setup wrote %d, want 8", got)
	}
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 3}, true); err != nil {
		t.Fatalf("EnsureCollectionWith: %v", err)
	}
	if got := len(ids(t, st, r)); got != 3 {
		t.Errorf("after declaring the bound %d remain, want 3", got)
	}
}

// An ordinary write must never drop a declared bound: Put calls
// EnsureCollection, which passes no policy.
func TestAnOrdinaryWriteDoesNotClearThePolicy(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/runs")
	if _, err := st.EnsureCollectionWith(r, nil, store.Retention{Last: 2}, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := st.Put(r, fmt.Sprintf("r%d", i), map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	cols, err := st.Collections("app")
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(cols) != 1 || cols[0].Retain.Last != 2 {
		t.Fatalf("policy after writes = %+v, want Last=2", cols)
	}
	if got := len(ids(t, st, r)); got != 2 {
		t.Errorf("collection holds %d, want the bound still enforced", got)
	}
}

func TestNoPolicyMeansUnbounded(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/free")
	for i := 0; i < 20; i++ {
		if _, err := st.Put(r, fmt.Sprintf("x%d", i), map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(ids(t, st, r)); got != 20 {
		t.Errorf("kept %d, want all 20 — no policy must not trim", got)
	}
}

func TestRetentionValidation(t *testing.T) {
	// A partition with no bound reads like a policy and enforces nothing.
	if err := (store.Retention{Per: []string{"tag"}}).Validate(); !errors.Is(err, store.ErrBadRetention) {
		t.Errorf("--retain-per without --retain-last = %v, want ErrBadRetention", err)
	}
	if err := (store.Retention{Last: -1}).Validate(); !errors.Is(err, store.ErrBadRetention) {
		t.Errorf("negative bound = %v, want ErrBadRetention", err)
	}
	if err := (store.Retention{Last: 5, Per: []string{"a b"}}).Validate(); !errors.Is(err, store.ErrBadRetention) {
		t.Errorf("unusable field name = %v, want ErrBadRetention", err)
	}
	if err := (store.Retention{Last: 5, Per: []string{"tag", "repo_id"}}).Validate(); err != nil {
		t.Errorf("valid policy rejected: %v", err)
	}
	if _, err := store.ParseRetention("nope", nil); !errors.Is(err, store.ErrBadRetention) {
		t.Errorf("non-numeric --retain-last = %v, want ErrBadRetention", err)
	}
	r, err := store.ParseRetention("10", []string{"tag,repo_id", "user_id"})
	if err != nil {
		t.Fatalf("ParseRetention: %v", err)
	}
	if r.Last != 10 || len(r.Per) != 3 {
		t.Errorf("ParseRetention = %+v, want Last=10 and three partition fields", r)
	}
}
