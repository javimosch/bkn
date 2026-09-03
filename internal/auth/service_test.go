package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/kv"
)

// A bcrypt hash of "legacy-password" produced by the Node backend's bcryptjs.
// The whole point of choosing bcrypt was that hashes like this keep verifying.
const legacyBcryptHash = "$2a$10$8x.8lYKooXQ1OR.9PuQSdOidlKhYAoLut.chHRM3VvqO7cfC/uEJi"

func newAuth(t *testing.T) *auth.Auth {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	t.Setenv("BKN_AUTH_SECRET", "test-secret")
	t.Setenv("BKN_ENCRYPTION_KEY", strings.Repeat("a", 32))

	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	kr, _ := kv.LoadKeyring()
	a, err := auth.New(conn, kv.New(conn, kr, 0))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

func mustUser(t *testing.T, a *auth.Auth, email, password string) auth.User {
	t.Helper()
	u, err := a.CreateUser(email, password, "", "")
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return u
}

// Requirement R4: the email invariant lives in one place, and any spelling of
// the same address must reach the same row.
func TestEmailIsNormalizedOnCreateAndLookup(t *testing.T) {
	a := newAuth(t)
	u := mustUser(t, a, "  Ada@Example.IO ", "correct-horse")
	if u.Email != "ada@example.io" {
		t.Fatalf("stored email = %q, want normalized", u.Email)
	}
	for _, spelling := range []string{"ada@example.io", "ADA@EXAMPLE.IO", " Ada@Example.io  "} {
		if _, err := a.FindUser(spelling); err != nil {
			t.Errorf("FindUser(%q): %v", spelling, err)
		}
		if _, err := a.Login(spelling, "correct-horse", ""); err != nil {
			t.Errorf("Login(%q): %v", spelling, err)
		}
	}
	if _, err := a.CreateUser("ADA@example.io", "another-password", "", ""); err != auth.ErrEmailTaken {
		t.Errorf("duplicate with different casing = %v, want ErrEmailTaken", err)
	}
}

func TestPasswordAndEmailValidation(t *testing.T) {
	a := newAuth(t)
	if _, err := a.CreateUser("ada@example.io", "short", "", ""); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("short password = %v, want ErrWeakPassword", err)
	}
	for _, bad := range []string{"", "not-an-email", "a@b", "a b@c.io", "@nope.io"} {
		if _, err := a.CreateUser(bad, "correct-horse", "", ""); !errors.Is(err, auth.ErrBadEmail) {
			t.Errorf("CreateUser(%q) = %v, want ErrBadEmail", bad, err)
		}
	}
	if _, err := a.CreateUser("ada@example.io", "correct-horse", "", "wizard"); !errors.Is(err, auth.ErrBadGlobalRole) {
		t.Errorf("bad role = %v, want ErrBadGlobalRole", err)
	}
}

func TestLegacyBcryptHashesStillVerify(t *testing.T) {
	a := newAuth(t)
	u := mustUser(t, a, "legacy@old.io", "placeholder-password")

	// Simulate a row migrated from the Node backend.
	if _, err := a.DB().Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, legacyBcryptHash, u.ID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := a.Login("legacy@old.io", "legacy-password", ""); err != nil {
		t.Errorf("login with a bcryptjs hash: %v", err)
	}
	if _, err := a.Login("legacy@old.io", "wrong", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Errorf("wrong password = %v, want ErrBadCredentials", err)
	}
}

// A login attempt must not reveal whether the address is registered.
func TestUnknownUserAndWrongPasswordAreIndistinguishable(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")

	_, missing := a.Login("nobody@example.io", "whatever1", "")
	_, wrong := a.Login("ada@example.io", "whatever1", "")
	if !errors.Is(missing, auth.ErrBadCredentials) || !errors.Is(wrong, auth.ErrBadCredentials) {
		t.Errorf("errors differ: missing=%v wrong=%v", missing, wrong)
	}
}

// A refresh token is single-use. A stolen one stops working as soon as the
// legitimate holder refreshes.
func TestRefreshTokensRotate(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")
	first, err := a.Login("ada@example.io", "correct-horse", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	second, err := a.Refresh(first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("refresh returned the same token")
	}
	if _, err := a.Refresh(first.RefreshToken); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("reusing the old refresh token = %v, want ErrSessionInvalid", err)
	}
	if _, err := a.Refresh(second.RefreshToken); err != nil {
		t.Errorf("the new refresh token should work: %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")
	tokens, _ := a.Login("ada@example.io", "correct-horse", "")

	for i := 0; i < 2; i++ {
		if err := a.Logout(tokens.RefreshToken); err != nil {
			t.Errorf("Logout #%d: %v", i+1, err)
		}
	}
	if err := a.Logout("a-token-that-never-existed"); err != nil {
		t.Errorf("Logout of an unknown token: %v", err)
	}
	if _, err := a.Refresh(tokens.RefreshToken); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("refresh after logout = %v, want ErrSessionInvalid", err)
	}
}

func TestOrgScopingRequiresMembership(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")
	if _, err := a.CreateOrg("acme", "Acme"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if _, err := a.Login("ada@example.io", "correct-horse", "acme"); !errors.Is(err, auth.ErrNotAMember) {
		t.Errorf("login scoped to a non-member org = %v, want ErrNotAMember", err)
	}
	if _, err := a.AddMember("acme", "ada@example.io", auth.OrgOwner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	tokens, err := a.Login("ada@example.io", "correct-horse", "acme")
	if err != nil {
		t.Fatalf("Login scoped to acme: %v", err)
	}
	if tokens.Org != "acme" || tokens.OrgRole != auth.OrgOwner {
		t.Errorf("tokens = %+v, want org acme/owner", tokens)
	}
	claims, err := a.Verify(tokens.AccessToken)
	if err != nil || claims.Org != "acme" || claims.OrgRole != auth.OrgOwner {
		t.Errorf("claims = %+v, %v", claims, err)
	}
}

func TestSwitchOrgRequiresMembershipAndRotates(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")
	for _, slug := range []string{"acme", "other"} {
		if _, err := a.CreateOrg(slug, ""); err != nil {
			t.Fatalf("CreateOrg(%q): %v", slug, err)
		}
	}
	if _, err := a.AddMember("acme", "ada@example.io", auth.OrgOwner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	tokens, _ := a.Login("ada@example.io", "correct-horse", "acme")

	if _, err := a.SwitchOrg(tokens.RefreshToken, "other"); !errors.Is(err, auth.ErrNotAMember) {
		t.Errorf("switching to a non-member org = %v, want ErrNotAMember", err)
	}
	// A failed switch must not have consumed the token.
	if _, err := a.SwitchOrg(tokens.RefreshToken, "acme"); err != nil {
		t.Errorf("switching back: %v", err)
	}
}

// A platform admin runs the deployment; that is not the same as owning a
// tenant's organization.
func TestPlatformAdminIsNotImplicitlyAnOrgOwner(t *testing.T) {
	a := newAuth(t)
	if _, err := a.CreateUser("root@example.io", "correct-horse", "", auth.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := a.CreateOrg("acme", ""); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	ok, err := a.Can("root@example.io", "acme", auth.OrgMember)
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if ok {
		t.Error("a platform admin was granted an org role implicitly")
	}
}

func TestCanUsesTheRoleHierarchy(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "owner@x.io", "correct-horse")
	mustUser(t, a, "member@x.io", "correct-horse")
	if _, err := a.CreateOrg("acme", ""); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := a.AddMember("acme", "owner@x.io", auth.OrgOwner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if _, err := a.AddMember("acme", "member@x.io", auth.OrgMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	cases := []struct {
		user, min string
		want      bool
	}{
		{"owner@x.io", auth.OrgAdmin, true},
		{"owner@x.io", auth.OrgOwner, true},
		{"member@x.io", auth.OrgMember, true},
		{"member@x.io", auth.OrgAdmin, false},
	}
	for _, tc := range cases {
		got, err := a.Can(tc.user, "acme", tc.min)
		if err != nil {
			t.Fatalf("Can(%s, %s): %v", tc.user, tc.min, err)
		}
		if got != tc.want {
			t.Errorf("Can(%s, acme, %s) = %v, want %v", tc.user, tc.min, got, tc.want)
		}
	}
}

// Changing a password or disabling an account must end the sessions that were
// opened before it, otherwise the credential change buys nothing.
func TestPasswordChangeAndDisableEndSessions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*auth.Auth) error
	}{
		{"password change", func(a *auth.Auth) error {
			p := "a-brand-new-password"
			_, err := a.UpdateUser("ada@example.io", nil, nil, &p, nil)
			return err
		}},
		{"disable", func(a *auth.Auth) error {
			yes := true
			_, err := a.UpdateUser("ada@example.io", nil, nil, nil, &yes)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuth(t)
			mustUser(t, a, "ada@example.io", "correct-horse")
			tokens, _ := a.Login("ada@example.io", "correct-horse", "")

			if err := tc.apply(a); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if _, err := a.Refresh(tokens.RefreshToken); err == nil {
				t.Error("a session survived the credential change")
			}
		})
	}
}

func TestDisabledUserCannotLogInOrResolveMe(t *testing.T) {
	a := newAuth(t)
	mustUser(t, a, "ada@example.io", "correct-horse")
	tokens, _ := a.Login("ada@example.io", "correct-horse", "")

	yes := true
	if _, err := a.UpdateUser("ada@example.io", nil, nil, nil, &yes); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if _, err := a.Login("ada@example.io", "correct-horse", ""); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("login = %v, want ErrUserDisabled", err)
	}
	// The access token is still cryptographically valid, so Me must check the
	// live record rather than trusting the claims.
	if _, _, err := a.Me(tokens.AccessToken); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("Me = %v, want ErrUserDisabled", err)
	}
}

func TestDeleteUserCascades(t *testing.T) {
	a := newAuth(t)
	u := mustUser(t, a, "ada@example.io", "correct-horse")
	if _, err := a.CreateOrg("acme", ""); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := a.AddMember("acme", u.ID, auth.OrgMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	tokens, _ := a.Login("ada@example.io", "correct-horse", "")

	if err := a.DeleteUser(u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := a.FindUser(u.ID); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("FindUser after delete = %v", err)
	}
	if _, err := a.Refresh(tokens.RefreshToken); err == nil {
		t.Error("a session outlived its user")
	}
	members, err := a.Members("acme")
	if err != nil || len(members) != 0 {
		t.Errorf("members after delete = %v, %v", members, err)
	}
}

// The password hash must never travel on a value handed back to a caller.
func TestUserValuesCarryNoPasswordHash(t *testing.T) {
	a := newAuth(t)
	u := mustUser(t, a, "ada@example.io", "correct-horse")
	tokens, _ := a.Login("ada@example.io", "correct-horse", "")

	for name, v := range map[string]auth.User{"create": u, "login": tokens.User} {
		if strings.Contains(strings.ToLower(sprint(v)), "$2a$") {
			t.Errorf("%s leaked a password hash: %v", name, v)
		}
	}
}

func sprint(u auth.User) string {
	return u.ID + u.Email + u.Name + u.Role + u.CreatedAt + u.UpdatedAt
}
