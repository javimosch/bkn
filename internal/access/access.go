// Package access decides whether a caller may perform a verb on a collection,
// and what scope that permission carries.
//
// It exists as its own package for one reason: authorization that is spread
// across handlers cannot be read in one sitting, and a rule you cannot read
// is a rule you cannot audit. Every decision bkn makes about a store request
// is made by Decide, and the whole matrix is enumerated in one test.
//
// The package is pure. It touches no database and no request, so the decision
// can be tested without either.
package access

import "github.com/javimosch/bkn/internal/store"

// Kind is what the credential presented turned out to be.
const (
	KindAnon  = "anon"  // no credential, or one that did not verify
	KindUser  = "user"  // a valid access token
	KindAdmin = "admin" // the admin token, or the loopback exemption
)

// Caller is the authenticated identity behind a request, reduced to the three
// facts a policy can consider. Anything else it might want - a permission
// table, a per-document ACL - is application logic, and belongs in a script.
type Caller struct {
	Kind string
	Sub  string // user id, when Kind is user
	Org  string // org the token is scoped to, when there is one
}

func (c Caller) isAdmin() bool { return c.Kind == KindAdmin }
func (c Caller) isUser() bool  { return c.Kind == KindUser && c.Sub != "" }

// Decision is the answer. When Allow is true and Field is non-empty, the
// caller may act only on documents whose Field equals Value - and the server
// must apply that as part of the read or write, never as a check beside it.
type Decision struct {
	Allow    bool
	Audience string
	Field    string
	Value    string
	// NeedsAuth distinguishes "you did not say who you are" from "you may not
	// do this", so a client knows whether refreshing a token would help.
	NeedsAuth bool
	Reason    string
}

// Scoped reports whether the decision restricts which documents are visible.
func (d Decision) Scoped() bool { return d.Field != "" }

// Filter renders the scope as a store filter. It returns nil when unscoped,
// so callers can append it unconditionally.
func (d Decision) Filter() []store.Filter {
	if !d.Scoped() {
		return nil
	}
	return []store.Filter{{Field: d.Field, Op: store.OpEq, Value: d.Value}}
}

// Decide answers whether caller may perform verb against a collection with
// policy a.
//
// The admin token always wins: it is the operator credential, it is what the
// CLI uses, and a policy that could lock the operator out of their own
// datastore would be a footgun rather than a feature.
func Decide(a store.Access, verb string, c Caller) Decision {
	audience := a.RuleFor(verb)
	if c.isAdmin() {
		return Decision{Allow: true, Audience: audience}
	}

	switch audience {
	case "public":
		return Decision{Allow: true, Audience: audience}

	case "user":
		if !c.isUser() {
			return denyAuth(audience, "this collection allows "+verb+" to any signed-in user")
		}
		return Decision{Allow: true, Audience: audience}

	case "owner":
		if !c.isUser() {
			return denyAuth(audience, "this collection allows "+verb+" to a document's owner")
		}
		return Decision{Allow: true, Audience: audience, Field: a.OwnerField, Value: c.Sub}

	case "org":
		if !c.isUser() {
			return denyAuth(audience, "this collection allows "+verb+" within an organization")
		}
		// A token with no org cannot be scoped to one. Falling through to an
		// unscoped read here would hand every tenant's documents to a caller
		// who merely forgot to pick an organization, which is the exact
		// failure this whole mechanism exists to make impossible.
		if c.Org == "" {
			return Decision{Audience: audience,
				Reason: "your token is not scoped to an organization; sign in with --org or call /v1/auth/switch-org"}
		}
		return Decision{Allow: true, Audience: audience, Field: a.OrgField, Value: c.Org}
	}

	// admin, and any audience a future binary wrote that this one does not
	// know. Refusing the unknown is the only safe reading of a policy written
	// by a newer version.
	//
	// NeedsAuth stays false: no user token will ever satisfy an admin route,
	// so telling a client to go and refresh one would send it in a circle.
	// This is also what keeps an undeclared collection answering exactly as it
	// did before policies existed.
	return Decision{Audience: audience, Reason: "missing or invalid bearer token"}
}

func denyAuth(audience, reason string) Decision {
	return Decision{Audience: audience, NeedsAuth: true, Reason: reason}
}

// StampFields returns the document fields a scoped create must carry, taken
// from the token rather than from the body. This is the half of the mechanism
// that removes bugs rather than adding checks: a document cannot be created
// into the wrong tenant if the tenant is not something the client can say.
func StampFields(a store.Access, verb string, c Caller) map[string]any {
	d := Decide(a, verb, c)
	if !d.Allow || !d.Scoped() {
		return nil
	}
	return map[string]any{d.Field: d.Value}
}
