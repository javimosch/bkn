package store_test

import (
	"errors"
	"testing"

	"github.com/javimosch/bkn/internal/store"
)

// mine is the scope filter a policy produces for a caller: one equality on the
// field that decides who owns a document.
func mine(field, who string) []store.Filter {
	return []store.Filter{{Field: field, Op: store.OpEq, Value: who}}
}

func seedTwoOwners(t *testing.T) (*store.Store, store.Ref) {
	t.Helper()
	st := newStore(t)
	r := ref(t, "app/notes")
	if _, err := st.Put(r, "a1", map[string]any{"title": "ada", "user_id": "ada"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Put(r, "b1", map[string]any{"title": "bob", "user_id": "bob"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st, r
}

func TestGetIfHidesAnotherOwnersDocument(t *testing.T) {
	st, r := seedTwoOwners(t)
	if _, err := st.GetIf(r, "a1", mine("user_id", "ada")); err != nil {
		t.Fatalf("owner could not read their own document: %v", err)
	}
	_, err := st.GetIf(r, "a1", mine("user_id", "bob"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading another owner's document returned %v, want ErrNotFound", err)
	}
	// Indistinguishable from an id that was never written: anything else tells
	// an unauthorized caller that the id is real.
	_, missing := st.GetIf(r, "never-existed", mine("user_id", "bob"))
	if missing == nil || missing.Error() != err.Error() {
		t.Errorf("a forbidden read (%v) is distinguishable from a missing one (%v)", err, missing)
	}
}

func TestDeleteIfLeavesAnotherOwnersDocument(t *testing.T) {
	st, r := seedTwoOwners(t)
	if err := st.DeleteIf(r, "a1", mine("user_id", "bob")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete across owners returned %v, want ErrNotFound", err)
	}
	if _, err := st.Get(r, "a1"); err != nil {
		t.Fatalf("the document was deleted anyway: %v", err)
	}
	if err := st.DeleteIf(r, "a1", mine("user_id", "ada")); err != nil {
		t.Fatalf("owner could not delete their own document: %v", err)
	}
}

// The upsert is the subtle one. A create policy stamps the caller's own id
// onto the document, so an unguarded upsert at somebody else's id would
// replace their document and relabel it as yours.
func TestPutIfCannotStealAnotherOwnersID(t *testing.T) {
	st, r := seedTwoOwners(t)
	_, err := st.PutIf(r, "a1", map[string]any{"title": "stolen", "user_id": "bob"}, mine("user_id", "bob"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("upsert onto another owner's id returned %v, want ErrNotFound", err)
	}
	rec, err := st.Get(r, "a1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec["title"] != "ada" || rec["user_id"] != "ada" {
		t.Fatalf("the document was overwritten: %v", rec)
	}
}

func TestPutIfInsertsWhenTheIDIsFree(t *testing.T) {
	st, r := seedTwoOwners(t)
	rec, err := st.PutIf(r, "c1", map[string]any{"title": "new", "user_id": "bob"}, mine("user_id", "bob"))
	if err != nil {
		t.Fatalf("insert at a free id: %v", err)
	}
	if rec["title"] != "new" {
		t.Fatalf("record = %v", rec)
	}
	// And overwrites its own.
	if _, err := st.PutIf(r, "c1", map[string]any{"title": "edited", "user_id": "bob"}, mine("user_id", "bob")); err != nil {
		t.Fatalf("owner could not overwrite their own document: %v", err)
	}
	got, _ := st.Get(r, "c1")
	if got["title"] != "edited" {
		t.Fatalf("record = %v", got)
	}
}

func TestAccessPolicySurvivesARoundTrip(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/notes")
	if _, err := st.EnsureCollection(r, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := store.Access{OwnerField: "user_id", Rules: map[string]string{"read": "owner", "create": "user"}}
	if _, err := st.SetAccess(r, want); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}
	got, err := st.Describe(r)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.Access.OwnerField != "user_id" || got.Access.RuleFor("read") != "owner" || got.Access.RuleFor("delete") != "admin" {
		t.Fatalf("access = %+v", got.Access)
	}

	// An ordinary write must not be able to touch the policy: EnsureCollection
	// runs on every Put, and a policy it could reset would be no policy.
	if _, err := st.Put(r, "x", map[string]any{"user_id": "ada"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	after, _ := st.Describe(r)
	if after.Access.RuleFor("read") != "owner" {
		t.Fatalf("a write cleared the policy: %+v", after.Access)
	}

	if _, err := st.SetAccess(r, store.Access{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, _ := st.Describe(r)
	if !cleared.Access.IsZero() {
		t.Fatalf("policy not cleared: %+v", cleared.Access)
	}
}

func TestSetAccessRejectsAnUnenforceablePolicy(t *testing.T) {
	st := newStore(t)
	r := ref(t, "app/notes")
	if _, err := st.EnsureCollection(r, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := st.SetAccess(r, store.Access{Rules: map[string]string{"read": "owner"}})
	if !errors.Is(err, store.ErrBadAccess) {
		t.Fatalf("read=owner with no owner field returned %v, want ErrBadAccess", err)
	}
}
