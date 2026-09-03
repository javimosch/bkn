package script

import (
	"errors"

	"github.com/javimosch/bkn/internal/auth"
)

// newAuthAPI builds the `bkn.auth` namespace.
//
// Note the trust model this assumes: scripts are written by the operator, not
// by tenants. A script already reads decrypted secrets through bkn.kv, so
// withholding identity operations from it would buy nothing. `issue` can mint
// a session for any user, which is what makes SSO callbacks and invite
// acceptance scriptable - and is exactly why script code deserves the same
// review as core code.
func (r *Runner) newAuthAPI(throw func(error)) map[string]any {
	if r.auth == nil {
		return nil
	}
	a := r.auth

	nilOnMissing := func(u auth.User, err error) any {
		if errors.Is(err, auth.ErrUserNotFound) {
			return nil
		}
		if err != nil {
			throw(err)
		}
		return u
	}
	opt := func(m map[string]any, key string) *string {
		if m == nil {
			return nil
		}
		if v, ok := m[key].(string); ok {
			return &v
		}
		return nil
	}

	return map[string]any{
		"createUser": func(email, password string, opts map[string]any) any {
			name, role := "", ""
			if opts != nil {
				if v, ok := opts["name"].(string); ok {
					name = v
				}
				if v, ok := opts["role"].(string); ok {
					role = v
				}
			}
			u, err := a.CreateUser(email, password, name, role)
			if err != nil {
				throw(err)
			}
			return u
		},
		"findUser": func(idOrEmail string) any {
			return nilOnMissing(a.FindUser(idOrEmail))
		},
		"updateUser": func(idOrEmail string, patch map[string]any) any {
			var disabled *bool
			if patch != nil {
				if v, ok := patch["disabled"].(bool); ok {
					disabled = &v
				}
			}
			u, err := a.UpdateUser(idOrEmail, opt(patch, "name"), opt(patch, "role"),
				opt(patch, "password"), disabled)
			if err != nil {
				throw(err)
			}
			return u
		},
		"deleteUser": func(idOrEmail string) bool {
			err := a.DeleteUser(idOrEmail)
			if errors.Is(err, auth.ErrUserNotFound) {
				return false
			}
			if err != nil {
				throw(err)
			}
			return true
		},
		"createOrg": func(slug, name string) any {
			o, err := a.CreateOrg(slug, name)
			if err != nil {
				throw(err)
			}
			return o
		},
		"orgs": func() []any {
			orgs, err := a.ListOrgs()
			if err != nil {
				throw(err)
			}
			out := make([]any, len(orgs))
			for i, o := range orgs {
				out[i] = o
			}
			return out
		},
		"addMember": func(org, user, role string) any {
			m, err := a.AddMember(org, user, role)
			if err != nil {
				throw(err)
			}
			return m
		},
		"removeMember": func(org, user string) bool {
			err := a.RemoveMember(org, user)
			if errors.Is(err, auth.ErrNotAMember) {
				return false
			}
			if err != nil {
				throw(err)
			}
			return true
		},
		"members": func(org string) []any {
			ms, err := a.Members(org)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(ms))
			for i, m := range ms {
				out[i] = m
			}
			return out
		},
		"memberships": func(user string) []any {
			ms, err := a.Memberships(user)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(ms))
			for i, m := range ms {
				out[i] = m
			}
			return out
		},
		"can": func(user, org, minRole string) bool {
			ok, err := a.Can(user, org, minRole)
			if err != nil {
				throw(err)
			}
			return ok
		},
		// verify returns null rather than throwing for an invalid token: a
		// script checking a caller's token expects a boolean-ish answer, not
		// an exception.
		"verify": func(token string) any {
			claims, err := a.Verify(token)
			if err != nil {
				return nil
			}
			return claims
		},
		"issue": func(idOrEmail, org string) any {
			tokens, err := a.IssueFor(idOrEmail, org)
			if err != nil {
				throw(err)
			}
			return tokens
		},
	}
}
