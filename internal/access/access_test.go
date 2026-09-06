package access_test

import (
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/access"
	"github.com/javimosch/bkn/internal/store"
)

var (
	admin = access.Caller{Kind: access.KindAdmin}
	anon  = access.Caller{Kind: access.KindAnon}
	ada   = access.Caller{Kind: access.KindUser, Sub: "u_ada", Org: "o_acme"}
	bob   = access.Caller{Kind: access.KindUser, Sub: "u_bob"} // no org
)

// TestMatrix enumerates every audience against every kind of caller. The point
// of the table is that the whole authorization surface fits on one screen: a
// rule that is not here is a rule that does not exist.
func TestMatrix(t *testing.T) {
	policy := store.Access{
		OwnerField: "user_id",
		OrgField:   "org_id",
		Rules: map[string]string{
			"read": "owner", "create": "org", "update": "user", "delete": "public",
		},
	}

	cases := []struct {
		verb      string
		caller    access.Caller
		allow     bool
		field     string
		value     string
		needsAuth bool
	}{
		// admin passes everything, and is never scoped: it is the operator.
		{"read", admin, true, "", "", false},
		{"create", admin, true, "", "", false},
		{"delete", admin, true, "", "", false},

		// owner: a signed-in caller, scoped to their own documents.
		{"read", ada, true, "user_id", "u_ada", false},
		{"read", bob, true, "user_id", "u_bob", false},
		{"read", anon, false, "", "", true},

		// org: scoped to the org the token carries.
		{"create", ada, true, "org_id", "o_acme", false},
		{"create", anon, false, "", "", true},

		// user: signed in, unscoped.
		{"update", ada, true, "", "", false},
		{"update", anon, false, "", "", true},

		// public: everybody, including anonymous.
		{"delete", anon, true, "", "", false},
		{"delete", ada, true, "", "", false},
	}

	for _, c := range cases {
		d := access.Decide(policy, c.verb, c.caller)
		if d.Allow != c.allow {
			t.Errorf("%s by %s: allow=%v want %v (%s)", c.verb, c.caller.Kind, d.Allow, c.allow, d.Reason)
		}
		if d.Field != c.field || d.Value != c.value {
			t.Errorf("%s by %s: scope=%s=%s want %s=%s", c.verb, c.caller.Kind, d.Field, d.Value, c.field, c.value)
		}
		if !d.Allow && d.NeedsAuth != c.needsAuth {
			t.Errorf("%s by %s: needsAuth=%v want %v", c.verb, c.caller.Kind, d.NeedsAuth, c.needsAuth)
		}
	}
}

// A token with no org cannot be scoped to one, and the failure has to be a
// refusal. Treating it as unscoped would hand every tenant's documents to a
// caller who merely forgot to choose an organization.
func TestOrgAudienceRefusesAnOrglessToken(t *testing.T) {
	policy := store.Access{OrgField: "org_id", Rules: map[string]string{"read": "org"}}
	d := access.Decide(policy, "read", bob)
	if d.Allow {
		t.Fatal("an org-scoped read was allowed for a token with no org")
	}
	if d.NeedsAuth {
		t.Error("this is not an authentication problem; a refresh will not fix it")
	}
}

// An undeclared verb is admin-only. A policy grants, it never revokes: opening
// reads must not open writes.
func TestUndeclaredVerbsStayAdminOnly(t *testing.T) {
	policy := store.Access{Rules: map[string]string{"read": "public"}}
	if !access.Decide(policy, "read", anon).Allow {
		t.Fatal("declared read=public did not allow an anonymous read")
	}
	for _, verb := range []string{"create", "update", "delete"} {
		if access.Decide(policy, verb, ada).Allow {
			t.Errorf("%s was allowed by a policy that only declared read", verb)
		}
	}
}

// A collection nobody has declared a policy for behaves exactly as it did
// before policies existed. This is the backward-compatibility assertion.
func TestNoPolicyIsAdminOnly(t *testing.T) {
	var none store.Access
	for _, verb := range store.Verbs() {
		if !access.Decide(none, verb, admin).Allow {
			t.Errorf("admin was refused %s on an undeclared collection", verb)
		}
		for _, c := range []access.Caller{ada, anon} {
			if access.Decide(none, verb, c).Allow {
				t.Errorf("%s was allowed %s on an undeclared collection", c.Kind, verb)
			}
		}
	}
}

// An audience written by a newer binary must be refused, not ignored. Reading
// an unknown word as "no restriction" would turn a forward-compatibility
// problem into a data leak.
func TestUnknownAudienceIsRefused(t *testing.T) {
	policy := store.Access{Rules: map[string]string{"read": "everyone-in-the-building"}}
	if access.Decide(policy, "read", ada).Allow {
		t.Fatal("an unknown audience was treated as permissive")
	}
}

func TestStampFieldsComeFromTheTokenNotTheBody(t *testing.T) {
	policy := store.Access{OwnerField: "user_id", Rules: map[string]string{"create": "owner"}}
	got := access.StampFields(policy, "create", ada)
	if got["user_id"] != "u_ada" {
		t.Fatalf("stamp = %v, want user_id=u_ada", got)
	}
	if access.StampFields(policy, "create", admin) != nil {
		t.Error("admin creates are unscoped, so nothing should be stamped")
	}
}

func TestPolicyValidation(t *testing.T) {
	bad := []store.Access{
		{Rules: map[string]string{"read": "owner"}},                        // owner without a field
		{Rules: map[string]string{"read": "org"}},                          // org without a field
		{Rules: map[string]string{"peek": "public"}},                       // unknown verb
		{Rules: map[string]string{"read": "friends"}},                      // unknown audience
		{OwnerField: "User ID", Rules: map[string]string{"read": "owner"}}, // unusable field name
	}
	for i, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("case %d: %+v validated, want a rejection", i, a)
		}
	}
	ok := store.Access{OwnerField: "user_id", Rules: map[string]string{"read": "owner", "create": "user"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid policy was rejected: %v", err)
	}
}

// An admin-only collection must not tell a signed-in user that authentication
// would help, because it would not: no user token satisfies an admin route.
// The server turns this into the 403 those routes always returned.
func TestAdminOnlyIsNotAnAuthenticationProblem(t *testing.T) {
	var none store.Access
	for _, c := range []access.Caller{ada, anon} {
		d := access.Decide(none, "read", c)
		if d.NeedsAuth {
			t.Errorf("%s: admin-only read reported as fixable by authenticating", c.Kind)
		}
	}
}

// A normalizer may name a nested key with dots; a scope field may not, and the
// coupling is the reason. The read side would address "$.declarant.user_id"
// through json_extract while a scoped create stamps the value with a literal
// map write, producing a top-level key of that name — so the document would be
// invisible to its own owner. This test exists so that anyone adding nested
// scope fields has to teach the stamp to walk the path in the same change.
func TestNestedScopeFieldIsRefusedUntilTheStampCanWalkAPath(t *testing.T) {
	a := store.Access{OwnerField: "declarant.user_id", Rules: map[string]string{"read": "owner"}}
	err := a.Validate()
	if err == nil {
		t.Fatal("a nested scope field was accepted; the create side stamps a literal key and cannot honour it")
	}
	if !strings.Contains(err.Error(), "top-level") {
		t.Errorf("the refusal should say why, got: %v", err)
	}
}
