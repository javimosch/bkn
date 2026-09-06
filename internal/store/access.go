package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrBadAccess = errors.New("invalid access policy")

// Access is a collection's declared authorization policy.
//
// It is deliberately not a rule language. A rule is one word from a closed
// set, and scoping is a field name rather than an expression, because the
// query surface refuses OR and expressions for the same reason: a predicate
// language growing inside bkn is application code that moved rather than
// application code that went away.
//
// The two scoped audiences need a field to scope by, and that field is
// declared once here rather than repeated at every call site. That is the
// whole point: the caller cannot forget the tenant filter, because the caller
// never writes it.
type Access struct {
	OwnerField string            `json:"owner_field,omitempty"`
	OrgField   string            `json:"org_field,omitempty"`
	Rules      map[string]string `json:"rules,omitempty"`
}

// Verbs are the four things a caller can ask of a collection. read covers get,
// list and count: they answer the same question at different resolutions, and
// a policy that let you count what you cannot read would leak the count.
func Verbs() []string { return []string{"read", "create", "update", "delete"} }

// Audiences, from most restrictive to least.
//
//	admin  - the admin token only. The default, so an undeclared collection
//	         behaves exactly as it did before policies existed.
//	user   - any caller holding a valid access token, unscoped.
//	owner  - a token holder, restricted to documents whose owner field is them.
//	org    - a token holder, restricted to documents whose org field matches
//	         the org their token is scoped to.
//	public - anybody, with or without a token.
func Audiences() []string { return []string{"admin", "user", "owner", "org", "public"} }

func validVerb(v string) bool     { return contains(Verbs(), v) }
func validAudience(a string) bool { return contains(Audiences(), a) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RuleFor returns the audience allowed to perform verb. An unset verb is
// admin: a policy grants, it never revokes, so declaring read=public on a
// collection does not quietly open writes.
func (a Access) RuleFor(verb string) string {
	if r, ok := a.Rules[verb]; ok && r != "" {
		return r
	}
	return "admin"
}

// IsZero reports whether nothing was declared, so an untouched collection
// serializes without an access key at all.
func (a Access) IsZero() bool {
	return a.OwnerField == "" && a.OrgField == "" && len(a.Rules) == 0
}

// ScopeField returns the document field an audience scopes by, or "" if the
// audience is unscoped.
func (a Access) ScopeField(audience string) string {
	switch audience {
	case "owner":
		return a.OwnerField
	case "org":
		return a.OrgField
	}
	return ""
}

// Validate rejects a policy that reads like a policy and enforces nothing.
// Declaring read=owner without saying which field holds the owner would
// otherwise be accepted and then scope by the empty string.
func (a Access) Validate() error {
	for verb, audience := range a.Rules {
		if !validVerb(verb) {
			return fmt.Errorf("%w: unknown verb %q, want one of %s",
				ErrBadAccess, verb, strings.Join(Verbs(), "|"))
		}
		if !validAudience(audience) {
			return fmt.Errorf("%w: unknown audience %q, want one of %s",
				ErrBadAccess, audience, strings.Join(Audiences(), "|"))
		}
		if audience == "owner" && a.OwnerField == "" {
			return fmt.Errorf("%w: %s=owner needs --owner-field", ErrBadAccess, verb)
		}
		if audience == "org" && a.OrgField == "" {
			return fmt.Errorf("%w: %s=org needs --org-field", ErrBadAccess, verb)
		}
	}
	for _, f := range []string{a.OwnerField, a.OrgField} {
		if f != "" && !identRe.MatchString(f) {
			return fmt.Errorf("%w: scope field %q must match %s", ErrBadAccess, f, identRe)
		}
	}
	return nil
}

// ParseAccess reads repeated or comma-separated "verb=audience" pairs.
func ParseAccess(pairs []string, ownerField, orgField string) (Access, error) {
	a := Access{OwnerField: ownerField, OrgField: orgField}
	for _, raw := range pairs {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			verb, audience, ok := strings.Cut(part, "=")
			if !ok {
				return Access{}, fmt.Errorf("%w: --access takes verb=audience, got %q", ErrBadAccess, part)
			}
			if a.Rules == nil {
				a.Rules = map[string]string{}
			}
			a.Rules[strings.TrimSpace(verb)] = strings.TrimSpace(audience)
		}
	}
	return a, a.Validate()
}

// String renders the policy the way it is declared, so `store collections`
// output can be pasted back into `store create`.
func (a Access) String() string {
	if len(a.Rules) == 0 {
		return ""
	}
	verbs := make([]string, 0, len(a.Rules))
	for v := range a.Rules {
		verbs = append(verbs, v)
	}
	sort.Slice(verbs, func(i, j int) bool {
		return indexOf(Verbs(), verbs[i]) < indexOf(Verbs(), verbs[j])
	})
	parts := make([]string, 0, len(verbs))
	for _, v := range verbs {
		parts = append(parts, v+"="+a.Rules[v])
	}
	return strings.Join(parts, ",")
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return len(list)
}
