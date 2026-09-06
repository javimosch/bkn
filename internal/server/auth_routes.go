package server

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/javimosch/bkn/internal/access"
	"github.com/javimosch/bkn/internal/auth"
)

// loginThrottle limits failed password attempts per email.
//
// A login endpoint without one is an offline password cracker with a network
// interface. It is per-email rather than per-IP because the credential being
// guessed is the email's, and an attacker picks their own IP.
type loginThrottle struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

const (
	throttleWindow = 15 * time.Minute
	throttleMax    = 10
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{failures: map[string][]time.Time{}}
}

// blocked reports whether the key has spent its budget, pruning old entries.
func (t *loginThrottle) blocked(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-throttleWindow)
	kept := t.failures[key][:0]
	for _, at := range t.failures[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(t.failures, key)
	} else {
		t.failures[key] = kept
	}
	return len(kept) >= throttleMax
}

func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	t.failures[key] = append(t.failures[key], time.Now())
	t.mu.Unlock()
}

func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	delete(t.failures, key)
	t.mu.Unlock()
}

// authStatus maps an auth error to an HTTP status and a stable error type.
func authStatus(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		return http.StatusUnauthorized, "bad_credentials"
	case errors.Is(err, auth.ErrUserDisabled):
		return http.StatusForbidden, "user_disabled"
	case errors.Is(err, auth.ErrSessionInvalid):
		return http.StatusUnauthorized, "session_invalid"
	case errors.Is(err, auth.ErrTokenExpired):
		return http.StatusUnauthorized, "token_expired"
	case errors.Is(err, auth.ErrBadToken):
		return http.StatusUnauthorized, "bad_token"
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrOrgNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, auth.ErrNotAMember):
		return http.StatusForbidden, "not_a_member"
	case errors.Is(err, auth.ErrEmailTaken), errors.Is(err, auth.ErrSlugTaken):
		return http.StatusConflict, "already_exists"
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadEmail),
		errors.Is(err, auth.ErrBadRole), errors.Is(err, auth.ErrBadGlobalRole),
		errors.Is(err, auth.ErrBadSlug):
		return http.StatusBadRequest, "validation_error"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	// Self-service. An application has no operator standing by when somebody
	// signs up or changes their own password.
	mux.HandleFunc("POST /v1/auth/register", s.authRegister)
	mux.HandleFunc("POST /v1/auth/password", s.authPassword)

	mux.HandleFunc("POST /v1/auth/orgs", s.authCreateOrg)
	mux.HandleFunc("GET /v1/auth/orgs/{org}/members", s.authMembers)
	mux.HandleFunc("POST /v1/auth/orgs/{org}/members", s.authAddMember)
	mux.HandleFunc("DELETE /v1/auth/orgs/{org}/members/{user}", s.authRemoveMember)

	mux.HandleFunc("POST /v1/auth/login", s.authLogin)
	mux.HandleFunc("POST /v1/auth/refresh", s.authRefresh)
	mux.HandleFunc("POST /v1/auth/logout", s.authLogout)
	mux.HandleFunc("POST /v1/auth/switch-org", s.authSwitchOrg)
	mux.HandleFunc("GET /v1/auth/me", s.authMe)

	// Administration of identities is operator work, not user work.
	mux.HandleFunc("GET /v1/auth/users", s.guard(s.authListUsers))
	mux.HandleFunc("POST /v1/auth/users", s.guard(s.authCreateUser))
	mux.HandleFunc("GET /v1/auth/orgs", s.guard(s.authListOrgs))
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	email := auth.NormalizeEmail(str(body, "email"))
	if s.throttle.blocked(email) {
		w.Header().Set("Retry-After", strconv.Itoa(int(throttleWindow.Seconds())))
		writeErr(w, http.StatusTooManyRequests, "rate_limited",
			"too many failed attempts for this account; try again later")
		return
	}

	tokens, err := s.auth.Login(email, str(body, "password"), str(body, "org"))
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			s.throttle.fail(email)
		}
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	s.throttle.reset(email)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": tokens})
}

func (s *Server) authRefresh(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	tokens, err := s.auth.Refresh(str(body, "refresh_token"))
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": tokens})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	if err := s.auth.Logout(str(body, "refresh_token")); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "logged_out": true})
}

func (s *Server) authSwitchOrg(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	tokens, err := s.auth.SwitchOrg(str(body, "refresh_token"), str(body, "org"))
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": tokens})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "not_authenticated", "missing bearer access token")
		return
	}
	user, claims, err := s.auth.Me(token)
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	memberships, err := s.auth.Memberships(user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "user": user, "claims": claims, "memberships": memberships,
	})
}

func (s *Server) authListUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	users, err := s.auth.ListUsers(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(users), "users": users})
}

func (s *Server) authCreateUser(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	u, err := s.auth.CreateUser(str(body, "email"), str(body, "password"), str(body, "name"), str(body, "role"))
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
}

func (s *Server) authListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.auth.ListOrgs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(orgs), "orgs": orgs})
}

// --- self-service ----------------------------------------------------------
//
// Creating users and changing passwords were admin operations, because the
// only client was an operator. An application has neither: nobody is standing
// by to run `bkn auth user create` when somebody signs up, and a user must be
// able to change their own password without handing it to an operator first.

// signupOpen reports whether anonymous registration is enabled.
//
// It is off by default and turned on by an environment variable rather than a
// stored setting, because an open registration endpoint is a decision about
// the deployment, and a deployment is configured where it is started - not by
// whoever last held a token.
func signupOpen() bool { return os.Getenv("BKN_OPEN_SIGNUP") == "1" }

func (s *Server) authRegister(w http.ResponseWriter, r *http.Request) {
	if !signupOpen() {
		writeErr(w, http.StatusForbidden, "signup_closed",
			"this deployment does not accept self-service registration; set BKN_OPEN_SIGNUP=1 to allow it")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	email := auth.NormalizeEmail(str(body, "email"))
	// The throttle is shared with login. Registration is the other way to
	// learn whether an address has an account, so it must cost the same.
	if s.throttle.blocked(email) {
		w.Header().Set("Retry-After", strconv.Itoa(int(throttleWindow.Seconds())))
		writeErr(w, http.StatusTooManyRequests, "rate_limited",
			"too many attempts for this account; try again later")
		return
	}
	// The role is not read from the body. A registration endpoint that let the
	// caller name their own role would be an admin-account vending machine.
	u, err := s.auth.CreateUser(email, str(body, "password"), str(body, "name"), "user")
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			s.throttle.fail(email)
		}
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	tokens, err := s.auth.IssueFor(u.ID, "")
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u, "tokens": tokens})
}

// authPassword changes the caller's own password.
//
// It verifies the current password by logging in with it rather than by
// trusting the access token alone: a token in the wrong hands is exactly the
// situation where an unverified password change is fatal. Changing a password
// revokes every session, which is what makes this the recovery move after a
// token leak.
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) {
	c := s.caller(r)
	if c.Kind != access.KindUser {
		writeErr(w, http.StatusUnauthorized, "not_authenticated",
			"this endpoint changes your own password, so it needs your access token")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	u, err := s.auth.FindUser(c.Sub)
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	if s.throttle.blocked(u.Email) {
		w.Header().Set("Retry-After", strconv.Itoa(int(throttleWindow.Seconds())))
		writeErr(w, http.StatusTooManyRequests, "rate_limited",
			"too many failed attempts for this account; try again later")
		return
	}
	if _, err := s.auth.Login(u.Email, str(body, "current_password"), ""); err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			s.throttle.fail(u.Email)
		}
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	s.throttle.reset(u.Email)
	next := str(body, "new_password")
	if _, err := s.auth.UpdateUser(u.ID, nil, nil, &next, nil); err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": true})
}

// --- organizations an application can create -------------------------------
//
// Creating an organization and inviting somebody into it were operator verbs,
// which is fine for a deployment with one tenant and impossible for a product
// where signing up means creating a workspace. These are the same operations
// the CLI performs, gated by org role instead of by the admin token.

// orgActor resolves the caller and their authority over one organization. A
// platform admin is deliberately not an owner of anybody's organization - the
// admin token is, because it is the operator's own credential and the
// alternative is an operator locked out of the deployment they run.
func (s *Server) orgActor(w http.ResponseWriter, r *http.Request, org, minRole string) (access.Caller, bool) {
	c := s.caller(r)
	if c.Kind == access.KindAdmin {
		return c, true
	}
	if c.Kind != access.KindUser {
		writeErr(w, http.StatusUnauthorized, "not_authenticated", "this endpoint needs your access token")
		return c, false
	}
	ok, err := s.auth.Can(c.Sub, org, minRole)
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return c, false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "forbidden",
			"you need to be "+minRole+" of this organization")
		return c, false
	}
	return c, true
}

// authCreateOrg creates an organization and makes its creator the owner.
//
// Creating one and then belonging to it are a single act from the caller's
// point of view, and splitting them would leave an ownerless organization
// behind whenever the second call failed.
func (s *Server) authCreateOrg(w http.ResponseWriter, r *http.Request) {
	c := s.caller(r)
	if c.Kind == access.KindAnon {
		writeErr(w, http.StatusUnauthorized, "not_authenticated",
			"creating an organization needs your access token")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	org, err := s.auth.CreateOrg(str(body, "slug"), str(body, "name"))
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	// An admin creating an organization on somebody else's behalf may name the
	// owner; a user creating their own workspace is the owner.
	owner := c.Sub
	if c.Kind == access.KindAdmin {
		owner = str(body, "owner")
	}
	if owner == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "org": org})
		return
	}
	m, err := s.auth.AddMember(org.ID, owner, "owner")
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "org": org, "membership": m})
}

func (s *Server) authMembers(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if _, ok := s.orgActor(w, r, org, "member"); !ok {
		return
	}
	members, err := s.auth.Members(org)
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(members), "members": members})
}

func (s *Server) authAddMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if _, ok := s.orgActor(w, r, org, "admin"); !ok {
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	role := str(body, "role")
	if role == "" {
		role = "member"
	}
	m, err := s.auth.AddMember(org, str(body, "user"), role)
	if err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "membership": m})
}

func (s *Server) authRemoveMember(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if _, ok := s.orgActor(w, r, org, "admin"); !ok {
		return
	}
	if err := s.auth.RemoveMember(org, r.PathValue("user")); err != nil {
		status, typ := authStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": r.PathValue("user")})
}
