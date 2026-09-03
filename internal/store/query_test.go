package store_test

import (
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

func seedProducts(t *testing.T, s *store.Store) store.Ref {
	t.Helper()
	r := ref(t, "shop/products")
	rows := []map[string]any{
		{"name": "widget", "price": 30, "stock": 5, "status": "live"},
		{"name": "gizmo", "price": 10, "stock": 0, "status": "draft"},
		{"name": "doohickey", "price": 50, "stock": 2, "status": "live"},
		{"name": "thing", "price": 20, "status": "archived"}, // no stock field
	}
	for _, row := range rows {
		if _, err := s.Put(r, row["name"].(string), row); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	return r
}

func names(recs []store.Record) []string {
	out := make([]string, len(recs))
	for i, rec := range recs {
		out[i], _ = rec["name"].(string)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseFilterSyntax(t *testing.T) {
	cases := []struct {
		spec  string
		field string
		op    store.Op
		value string
	}{
		{"price=30", "price", store.OpEq, "30"},
		{"price!=30", "price", store.OpNe, "30"},
		{"price>20", "price", store.OpGt, "20"},
		{"price>=20", "price", store.OpGte, "20"},
		{"price<20", "price", store.OpLt, "20"},
		{"price<=20", "price", store.OpLte, "20"},
		{"email=a@b.io", "email", store.OpEq, "a@b.io"},
		// A value containing an operator character must not be re-split.
		{"note=a>b", "note", store.OpEq, "a>b"},
	}
	for _, tc := range cases {
		got, err := store.ParseFilter(tc.spec)
		if err != nil {
			t.Errorf("ParseFilter(%q): %v", tc.spec, err)
			continue
		}
		if got.Field != tc.field || got.Op != tc.op || got.Value != tc.value {
			t.Errorf("ParseFilter(%q) = %+v, want %s %s %q", tc.spec, got, tc.field, tc.op, tc.value)
		}
	}

	in, err := store.ParseFilter("status:in=draft, live ,archived")
	if err != nil {
		t.Fatalf("ParseFilter(in): %v", err)
	}
	if in.Op != store.OpIn || len(in.Values) != 3 || in.Values[1] != "live" {
		t.Errorf("in filter = %+v", in)
	}

	for _, bad := range []string{"", "price", "price!!20", "=30", "status:in="} {
		if _, err := store.ParseFilter(bad); err == nil {
			t.Errorf("ParseFilter(%q) accepted invalid syntax", bad)
		}
	}
}

func TestOrderByDocumentField(t *testing.T) {
	s := newStore(t)
	r := seedProducts(t, s)

	asc, err := s.List(r, store.ListOptions{OrderBy: "price", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"gizmo", "thing", "widget", "doohickey"}; !equal(names(asc), want) {
		t.Errorf("price asc = %v, want %v", names(asc), want)
	}

	desc, err := s.List(r, store.ListOptions{OrderBy: "price", Desc: true, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"doohickey", "widget", "thing", "gizmo"}; !equal(names(desc), want) {
		t.Errorf("price desc = %v, want %v", names(desc), want)
	}
}

// SQLite sorts NULL first ascending, which would bury every populated row
// beneath the ones that lack the field entirely.
func TestRecordsMissingTheSortFieldGoLast(t *testing.T) {
	s := newStore(t)
	r := seedProducts(t, s)

	for _, desc := range []bool{false, true} {
		got, err := s.List(r, store.ListOptions{OrderBy: "stock", Desc: desc, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		last := names(got)[len(got)-1]
		if last != "thing" {
			t.Errorf("desc=%v: last record is %q, want the one with no stock field", desc, last)
		}
	}
}

func TestComparisonOperators(t *testing.T) {
	s := newStore(t)
	r := seedProducts(t, s)

	cases := []struct {
		spec string
		want int
	}{
		{"price>20", 2},
		{"price>=20", 3},
		{"price<20", 1},
		{"price<=20", 2},
		{"price=30", 1},
		{"status!=draft", 3},
		{"status:in=live,archived", 3},
		{"status:in=nothing", 0},
		{"price>999", 0},
	}
	for _, tc := range cases {
		f, err := store.ParseFilter(tc.spec)
		if err != nil {
			t.Fatalf("ParseFilter(%q): %v", tc.spec, err)
		}
		got, err := s.List(r, store.ListOptions{Filters: []store.Filter{f}, Limit: 50})
		if err != nil {
			t.Fatalf("List(%q): %v", tc.spec, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s matched %d, want %d (%v)", tc.spec, len(got), tc.want, names(got))
		}
		total, err := s.Count(r, []store.Filter{f})
		if err != nil {
			t.Fatalf("Count(%q): %v", tc.spec, err)
		}
		if total != tc.want {
			t.Errorf("Count(%s) = %d, want %d", tc.spec, total, tc.want)
		}
	}
}

func TestFiltersCombineWithAnd(t *testing.T) {
	s := newStore(t)
	r := seedProducts(t, s)

	price, _ := store.ParseFilter("price>15")
	status, _ := store.ParseFilter("status=live")
	got, err := s.List(r, store.ListOptions{
		Filters: []store.Filter{price, status}, OrderBy: "price", Limit: 50,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"widget", "doohickey"}; !equal(names(got), want) {
		t.Errorf("combined = %v, want %v", names(got), want)
	}
}

// A paginated list needs the match count, not the page size, to say
// "page 1 of 12".
func TestCountIsIndependentOfThePage(t *testing.T) {
	s := newStore(t)
	r := seedProducts(t, s)

	page, err := s.List(r, store.ListOptions{OrderBy: "price", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	total, err := s.Count(r, nil)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if len(page) != 2 || total != 4 {
		t.Errorf("page %d of total %d, want 2 of 4", len(page), total)
	}
	if empty, err := s.Count(ref(t, "shop/absent"), nil); err != nil || empty != 0 {
		t.Errorf("Count on an unknown collection = %d, %v", empty, err)
	}
}

// Normalizers apply to every operator's operands, not only to equality.
func TestNormalizersApplyToAllOperators(t *testing.T) {
	s := newStore(t)
	r := ref(t, "app/users")
	if _, err := s.EnsureCollection(r, map[string]string{"email": "trim_lower"}); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	for _, email := range []string{"  Ada@Example.IO ", "bob@example.io"} {
		if _, err := s.Put(r, "", map[string]any{"email": email}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	in, _ := store.ParseFilter("email:in=ADA@EXAMPLE.IO,nobody@example.io")
	got, err := s.List(r, store.ListOptions{Filters: []store.Filter{in}, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("in filter with unnormalized values matched %d, want 1", len(got))
	}

	ne, _ := store.ParseFilter("email!=ADA@EXAMPLE.IO")
	got, err = s.List(r, store.ListOptions{Filters: []store.Filter{ne}, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0]["email"] != "bob@example.io" {
		t.Errorf("!= filter = %v, want only bob", got)
	}
}

// Ordering is opt-in: an omitted OrderBy must behave exactly as it did before
// ordering existed.
//
// Seeded with generated ids rather than the names used elsewhere in this file,
// because updated_at has second resolution: writes landing in the same second
// tie, and the tie is broken by id. With ULIDs that is still creation order;
// with caller-supplied ids it is alphabetical, which is documented behaviour
// rather than recency.
func TestDefaultOrderIsStillRecency(t *testing.T) {
	s := newStore(t)
	r := ref(t, "shop/events")
	for _, name := range []string{"first", "second", "third"} {
		if _, err := s.Put(r, "", map[string]any{"name": name}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := s.List(r, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"third", "second", "first"}; !equal(names(got), want) {
		t.Errorf("default order = %v, want %v", names(got), want)
	}
}
