package server

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

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
