package store_test

import (
	"errors"
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

// A normalizer on a nested key must actually run. The filter side already
// addresses documents this way, so before this worked the two halves of the
// invariant disagreed and the document became unfindable by that field.
func TestNestedNormalizerIsApplied(t *testing.T) {
	st := newStore(t)
	r := ref(t, "veilleurs/signalements")
	if _, err := st.EnsureCollection(r, map[string]string{"declarant.email": "trim_lower"}); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}

	rec, err := st.Put(r, "", map[string]any{
		"declarant": map[string]any{"name": "Ada", "email": "  MiXeD@Example.IO  "},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got := rec["declarant"].(map[string]any)["email"]
	if got != "mixed@example.io" {
		t.Fatalf("declarant.email = %q, want it normalized", got)
	}
}

// The write side and the filter side must agree: a lookup by the field has to
// find the row a normalized write created, whichever form the caller types.
func TestNestedNormalizerMakesLookupsWork(t *testing.T) {
	st := newStore(t)
	r := ref(t, "veilleurs/signalements")
	if _, err := st.EnsureCollection(r, map[string]string{"declarant.email": "trim_lower"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(r, "s1", map[string]any{
		"declarant": map[string]any{"email": "  MiXeD@Example.IO  "},
	}); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{"mixed@example.io", "  MiXeD@Example.IO  ", "MIXED@EXAMPLE.IO"} {
		recs, err := st.List(r, store.ListOptions{
			Filters: []store.Filter{{Field: "declarant.email", Op: store.OpEq, Value: probe}},
		})
		if err != nil {
			t.Fatalf("List(%q): %v", probe, err)
		}
		if len(recs) != 1 {
			t.Errorf("lookup by %q found %d, want 1", probe, len(recs))
		}
	}
}

// Patch goes through the same normalizers as Put.
func TestNestedNormalizerAppliesOnPatch(t *testing.T) {
	st := newStore(t)
	r := ref(t, "veilleurs/signalements")
	if _, err := st.EnsureCollection(r, map[string]string{"declarant.email": "trim_lower"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(r, "s1", map[string]any{"declarant": map[string]any{"email": "a@b.io"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Patch(r, "s1", map[string]any{"declarant": map[string]any{"email": "  NEW@Example.IO  "}})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got := rec["declarant"].(map[string]any)["email"]; got != "new@example.io" {
		t.Errorf("after patch declarant.email = %q, want normalized", got)
	}
}

// Absent paths, non-object parents and non-string leaves must pass through
// rather than erroring or corrupting the document.
func TestNestedNormalizerToleratesShapesItCannotAddress(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/things")
	if _, err := st.EnsureCollection(r, map[string]string{"a.b.c": "trim_lower"}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]any{
		"path absent":       {"other": 1},
		"parent not object": {"a": "scalar"},
		"leaf absent":       {"a": map[string]any{"b": map[string]any{}}},
		"leaf not a string": {"a": map[string]any{"b": map[string]any{"c": float64(42)}}},
		"partial path":      {"a": map[string]any{"b": "scalar"}},
	}
	for name, doc := range cases {
		if _, err := st.Put(r, "", doc); err != nil {
			t.Errorf("%s: Put returned %v, want it to pass through", name, err)
		}
	}
}

// A document that genuinely holds a key containing a dot keeps its old
// behaviour: the literal key wins over the path interpretation.
func TestLiteralDottedKeyStillNormalizes(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/legacy")
	if _, err := st.EnsureCollection(r, map[string]string{"a.b": "trim_lower"}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Put(r, "", map[string]any{"a.b": "  MiXeD  "})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rec["a.b"] != "mixed" {
		t.Errorf("literal dotted key = %v, want normalized", rec["a.b"])
	}
}

// A path the store cannot address must be refused when declared, not accepted
// and silently ignored — rule 5, fail loudly, never downgrade.
func TestUnaddressableNormalizerFieldsAreRejected(t *testing.T) {
	st := newStore(t)
	for _, bad := range []string{"", "a..b", ".a", "a.", "items[0].email", "$.a", "a b", `a"b`} {
		if _, err := st.EnsureCollection(ref(t, "app/bad"), map[string]string{bad: "trim_lower"}); !errors.Is(err, store.ErrBadNormalizerField) {
			t.Errorf("EnsureCollection(normalize %q) = %v, want ErrBadNormalizerField", bad, err)
		}
	}
	if _, err := st.EnsureCollection(ref(t, "app/good"), map[string]string{"declarant.email": "trim_lower"}); err != nil {
		t.Errorf("a valid nested path was rejected: %v", err)
	}
}
