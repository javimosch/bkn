package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

func patchSetup(t *testing.T) (*store.Store, store.Ref, string) {
	t.Helper()
	st := newStore(t)
	r := ref(t, "app/runs")
	if _, err := st.EnsureCollection(r, nil); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	rec, err := st.Put(r, "", map[string]any{"status": "queued", "tries": float64(0), "log": ""})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return st, r, rec["id"].(string)
}

func TestIncrementAppendPushAndSetIfEmpty(t *testing.T) {
	st, ref, id := patchSetup(t)

	rec, err := st.Patch(ref, id, map[string]any{
		"tries":  map[string]any{"$inc": float64(1)},
		"log":    map[string]any{"$append": "started\n"},
		"stages": map[string]any{"$push": "build"},
		"worker": map[string]any{"$setIfEmpty": "w1"},
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if rec["tries"] != float64(1) {
		t.Errorf("tries = %v, want 1", rec["tries"])
	}
	if rec["log"] != "started\n" {
		t.Errorf("log = %q", rec["log"])
	}
	if got := rec["stages"].([]any); len(got) != 1 || got[0] != "build" {
		t.Errorf("stages = %v, want [build]", got)
	}
	if rec["worker"] != "w1" {
		t.Errorf("worker = %v, want w1", rec["worker"])
	}

	// Applied again they accumulate, except $setIfEmpty which now has a value.
	rec, err = st.Patch(ref, id, map[string]any{
		"tries":  map[string]any{"$inc": float64(2)},
		"log":    map[string]any{"$append": "done\n"},
		"stages": map[string]any{"$push": "test"},
		"worker": map[string]any{"$setIfEmpty": "w2"},
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if rec["tries"] != float64(3) {
		t.Errorf("tries = %v, want 3", rec["tries"])
	}
	if rec["log"] != "started\ndone\n" {
		t.Errorf("log = %q, want both lines", rec["log"])
	}
	if got := rec["stages"].([]any); len(got) != 2 {
		t.Errorf("stages = %v, want two", got)
	}
	if rec["worker"] != "w1" {
		t.Errorf("worker = %v, want the first claim to stand", rec["worker"])
	}
}

// The point of the operators: concurrent patches must all land. The old
// read-merge-write kept whichever write finished last.
func TestConcurrentIncrementsDoNotLoseUpdates(t *testing.T) {
	st, ref, id := patchSetup(t)

	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.Patch(ref, id, map[string]any{"tries": map[string]any{"$inc": float64(1)}}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Patch: %v", err)
	}

	rec, err := st.Get(ref, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec["tries"] != float64(writers) {
		t.Errorf("tries = %v after %d concurrent increments, want %d (updates were lost)",
			rec["tries"], writers, writers)
	}
}

// Two patches of different fields must both survive.
func TestConcurrentPatchesOfDifferentFieldsBothSurvive(t *testing.T) {
	st, ref, id := patchSetup(t)

	var wg sync.WaitGroup
	for i, f := range []map[string]any{
		{"status": "running"},
		{"log": map[string]any{"$append": "x"}},
	} {
		wg.Add(1)
		go func(i int, fields map[string]any) {
			defer wg.Done()
			if _, err := st.Patch(ref, id, fields); err != nil {
				t.Errorf("patch %d: %v", i, err)
			}
		}(i, f)
	}
	wg.Wait()

	rec, _ := st.Get(ref, id)
	if rec["status"] != "running" {
		t.Errorf("status = %v, want running (lost update)", rec["status"])
	}
	if rec["log"] != "x" {
		t.Errorf("log = %v, want x (lost update)", rec["log"])
	}
}

func TestPreconditions(t *testing.T) {
	st, ref, id := patchSetup(t)

	// A guard that holds lets the write through.
	if _, err := st.PatchWith(ref, id, map[string]any{"status": "running"},
		store.PatchOptions{If: map[string]string{"status": "queued"}}); err != nil {
		t.Fatalf("precondition that holds: %v", err)
	}

	// The same guard now fails, and nothing is written.
	_, err := st.PatchWith(ref, id, map[string]any{"status": "done"},
		store.PatchOptions{If: map[string]string{"status": "queued"}})
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale guard = %v, want ErrPrecondition", err)
	}
	rec, _ := st.Get(ref, id)
	if rec["status"] != "running" {
		t.Errorf("a failed precondition wrote anyway: status = %v", rec["status"])
	}

	// --if-absent claims a field exactly once, which is how a worker takes a job.
	if _, err := st.PatchWith(ref, id, map[string]any{"worker": "w1"},
		store.PatchOptions{IfAbsent: []string{"worker"}}); err != nil {
		t.Fatalf("claiming an absent field: %v", err)
	}
	_, err = st.PatchWith(ref, id, map[string]any{"worker": "w2"},
		store.PatchOptions{IfAbsent: []string{"worker"}})
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("second claim = %v, want ErrPrecondition", err)
	}
}

// Exactly one of many contenders may claim a job. This is the store-level
// equivalent of cron's claim(), which had to be hand-written in SQL.
func TestOnlyOneClaimantWins(t *testing.T) {
	st, ref, id := patchSetup(t)

	const contenders = 12
	var wg sync.WaitGroup
	won := make(chan string, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.PatchWith(ref, id, map[string]any{"worker": fmt.Sprintf("w%d", i)},
				store.PatchOptions{IfAbsent: []string{"worker"}})
			if err == nil {
				won <- fmt.Sprintf("w%d", i)
			} else if !errors.Is(err, store.ErrPrecondition) && !errors.Is(err, store.ErrConcurrent) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(won)
	if n := len(won); n != 1 {
		t.Errorf("%d contenders claimed the job, want exactly 1", n)
	}
}

func TestOperatorTypeErrors(t *testing.T) {
	st, ref, id := patchSetup(t)
	cases := map[string]map[string]any{
		"$inc on a string":    {"status": map[string]any{"$inc": float64(1)}},
		"$append on a number": {"tries": map[string]any{"$append": "x"}},
		"$inc with a string":  {"tries": map[string]any{"$inc": "one"}},
		"$push on a string":   {"log": map[string]any{"$push": "x"}},
	}
	for name, fields := range cases {
		if _, err := st.Patch(ref, id, fields); !errors.Is(err, store.ErrOperandType) {
			t.Errorf("%s = %v, want ErrOperandType", name, err)
		}
	}
	if _, err := st.Patch(ref, id, map[string]any{"tries": map[string]any{"$nope": 1}}); !errors.Is(err, store.ErrBadOperator) {
		t.Errorf("unknown operator = %v, want ErrBadOperator", err)
	}
}

// A plain object is still a plain value; only a single $-prefixed key is an
// operator. Documents that legitimately hold objects must not change meaning.
func TestPlainObjectsAreNotOperators(t *testing.T) {
	st, ref, id := patchSetup(t)
	meta := map[string]any{"region": "eu", "tier": "gold"}
	rec, err := st.Patch(ref, id, map[string]any{"meta": meta})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	got, ok := rec["meta"].(map[string]any)
	if !ok || got["region"] != "eu" || got["tier"] != "gold" {
		t.Errorf("meta = %v, want the object stored verbatim", rec["meta"])
	}
}
