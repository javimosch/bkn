package store_test

import (
	"testing"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return store.New(conn)
}

func ref(t *testing.T, s string) store.Ref {
	t.Helper()
	r, err := store.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

func TestParseRefRejectsBadShapes(t *testing.T) {
	for _, bad := range []string{"users", "MyApp/users", "myapp/Users", "myapp/", "/users", "9app/users", "myapp/users/extra"} {
		if _, err := store.ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted a bad ref", bad)
		}
	}
	if _, err := store.ParseRef("my-app_2/user_records"); err != nil {
		t.Errorf("ParseRef rejected a valid ref: %v", err)
	}
}

// A normalizer must apply to writes AND to filter values, otherwise a lookup
// cannot find the row its own write created. This is the invariant that used
// to be duplicated in every consumer.
func TestNormalizerAppliesToWritesAndFilters(t *testing.T) {
	s := newStore(t)
	r := ref(t, "myapp/users")
	if _, err := s.EnsureCollection(r, map[string]string{"email": "trim_lower"}); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if _, err := s.Put(r, "", map[string]any{"email": "  Ada@Example.IO "}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Find(r, []store.Filter{{Field: "email", Op: store.OpEq, Value: "ADA@EXAMPLE.IO"}})
	if err != nil {
		t.Fatalf("Find with differently-cased filter: %v", err)
	}
	if got["email"] != "ada@example.io" {
		t.Errorf("stored email = %v, want normalized", got["email"])
	}
}

func TestPutAcceptsCallerSuppliedID(t *testing.T) {
	s := newStore(t)
	r := ref(t, "myapp/users")

	rec, err := s.Put(r, "explicit-id", map[string]any{"n": 1})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rec["id"] != "explicit-id" {
		t.Errorf("id = %v, want explicit-id", rec["id"])
	}

	// An id inside the document body works too, and is not duplicated as a field.
	rec, err = s.Put(r, "", map[string]any{"id": "from-body", "n": 2})
	if err != nil {
		t.Fatalf("Put with body id: %v", err)
	}
	if rec["id"] != "from-body" {
		t.Errorf("id = %v, want from-body", rec["id"])
	}

	// With neither, one is minted.
	rec, err = s.Put(r, "", map[string]any{"n": 3})
	if err != nil {
		t.Fatalf("Put without id: %v", err)
	}
	if id, _ := rec["id"].(string); len(id) != 26 {
		t.Errorf("generated id = %q, want 26 chars", id)
	}
}

func TestFilterTypesMatchJSONTypes(t *testing.T) {
	s := newStore(t)
	r := ref(t, "myapp/items")
	if _, err := s.Put(r, "x", map[string]any{"age": 30, "active": true, "name": "ada"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, f := range []store.Filter{
		{Field: "age", Op: store.OpEq, Value: "30"},
		{Field: "active", Op: store.OpEq, Value: "true"},
		{Field: "name", Op: store.OpEq, Value: "ada"},
	} {
		if _, err := s.Find(r, []store.Filter{f}); err != nil {
			t.Errorf("Find %s=%s: %v", f.Field, f.Value, err)
		}
	}
	if _, err := s.Find(r, []store.Filter{{Field: "age", Op: store.OpEq, Value: "31"}}); err != store.ErrNotFound {
		t.Errorf("Find on a non-matching number = %v, want ErrNotFound", err)
	}
}

func TestPatchMergesAndRefusesIDChange(t *testing.T) {
	s := newStore(t)
	r := ref(t, "myapp/users")
	if _, err := s.Put(r, "u1", map[string]any{"a": 1, "b": 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Patch(r, "u1", map[string]any{"b": 3, "c": 4, "id": "hijack"})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got["a"] != float64(1) || got["b"] != float64(3) || got["c"] != float64(4) {
		t.Errorf("merge wrong: %v", got)
	}
	if got["id"] != "u1" {
		t.Errorf("id = %v, want u1 (ids are immutable)", got["id"])
	}
}

func TestListPagesAndDeleteReportsMissing(t *testing.T) {
	s := newStore(t)
	r := ref(t, "myapp/items")
	for i := 0; i < 5; i++ {
		if _, err := s.Put(r, string(rune('a'+i)), map[string]any{"i": i}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	page, err := s.List(r, store.ListOptions{Limit: 2})
	if err != nil || len(page) != 2 {
		t.Fatalf("List limit 2 = %d records, %v", len(page), err)
	}
	rest, err := s.List(r, store.ListOptions{Limit: 10, Offset: 2})
	if err != nil || len(rest) != 3 {
		t.Fatalf("List offset 2 = %d records, %v", len(rest), err)
	}
	if err := s.Delete(r, "nope"); err != store.ErrNotFound {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
}

// Reading an unknown collection is an empty result, not an error: a consumer
// polling for records should not have to special-case "not created yet".
func TestListUnknownCollectionIsEmpty(t *testing.T) {
	s := newStore(t)
	recs, err := s.List(ref(t, "myapp/nothing"), store.ListOptions{Limit: 10})
	if err != nil || len(recs) != 0 {
		t.Errorf("List on unknown collection = %v records, %v", len(recs), err)
	}
}

func TestIDsSortByCreationOrder(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 100; i++ {
		id := store.NewID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if len(id) != 26 {
			t.Fatalf("id %q is %d chars, want 26", id, len(id))
		}
		if prev != "" && id < prev {
			t.Fatalf("id %q sorts before its predecessor %q", id, prev)
		}
		prev = id
	}
}
