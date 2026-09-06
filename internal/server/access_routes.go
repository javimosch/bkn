package server

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/javimosch/bkn/internal/access"
	"github.com/javimosch/bkn/internal/store"
)

// caller resolves the credential on a request to an identity.
//
// The order matters. The admin token is checked first and compared in
// constant time, because it is the operator credential and must not be
// affected by anything a tenant can send. Only then is the bearer tried as a
// user access token - so a leaked user token can never be mistaken for the
// admin one, whatever it contains.
func (s *Server) caller(r *http.Request) access.Caller {
	if s.authed(r) {
		return access.Caller{Kind: access.KindAdmin}
	}
	token := bearer(r)
	if token == "" || s.auth == nil {
		return access.Caller{Kind: access.KindAnon}
	}
	claims, err := s.auth.Verify(token)
	if err != nil {
		return access.Caller{Kind: access.KindAnon}
	}
	return access.Caller{Kind: access.KindUser, Sub: claims.Subject, Org: claims.Org}
}

// policyFor loads a collection's declared policy. A collection that does not
// exist yet has no policy, which means admin-only - so an unknown ref cannot
// be talked into existence by an anonymous caller, and a PUT that would create
// it still works for the operator.
func (s *Server) policyFor(ref store.Ref) store.Access {
	c, err := s.st.Describe(ref)
	if err != nil {
		return store.Access{}
	}
	return c.Access
}

// policed wraps a store handler with the authorization decision for verb.
//
// The decision is computed once, here, and handed to the handler. Handlers do
// not consult the policy themselves: the only way to serve a store request is
// to be given a Decision, so forgetting to check is not a shape the code can
// take.
func (s *Server) policed(verb string, next func(http.ResponseWriter, *http.Request, store.Ref, access.Decision)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.caller(r)

		// Authentication comes before validation. A ref that does not parse
		// names no collection, so there is no policy to consult and it is
		// admin-only like any collection nobody declared - which means an
		// anonymous caller is refused without being told the grammar of a ref
		// it was never allowed to address. Deciding after parsing would have
		// turned that 403 into a 400 carrying the rules.
		ref, err := store.ParseRef(r.PathValue("ns") + "/" + r.PathValue("coll"))
		if err != nil {
			if d := access.Decide(store.Access{}, verb, c); !d.Allow {
				s.deny(w, d)
				return
			}
			writeErr(w, http.StatusBadRequest, "invalid_arguments", err.Error())
			return
		}

		d := access.Decide(s.policyFor(ref), verb, c)
		if !d.Allow {
			s.deny(w, d)
			return
		}
		next(w, r, ref, d)
	}
}

// deny writes a refusal.
//
// 401 says the credential was missing or would not verify; 403 says it
// verified and still is not enough. A client can act on the first (refresh,
// sign in) and cannot act on the second. An admin-only collection keeps the
// status and the error type it had before policies existed, because every
// existing client and assertion was written against them.
func (s *Server) deny(w http.ResponseWriter, d access.Decision) {
	switch {
	case d.NeedsAuth:
		writeErr(w, http.StatusUnauthorized, "not_authenticated", d.Reason)
	case d.Audience == "admin":
		writeErr(w, http.StatusForbidden, "not_authenticated", d.Reason)
	default:
		writeErr(w, http.StatusForbidden, "forbidden", d.Reason)
	}
}

// --- CORS ------------------------------------------------------------------
//
// A browser application is the reason the scoped audiences exist, and a
// browser cannot use them without this. It is an explicit allow-list rather
// than a reflected origin: bkn authenticates with a bearer header, so a
// permissive default would let any page on the internet spend a token it
// happened to obtain.

func corsOrigins() []string {
	raw := os.Getenv("BKN_CORS_ORIGIN")
	if raw == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func allowedOrigin(origin string, allow []string) string {
	if origin == "" {
		return ""
	}
	for _, a := range allow {
		if a == "*" {
			// Echo the caller's origin rather than "*". They are equivalent
			// for bearer auth, and echoing keeps the response usable if a
			// deployment later adds credentials.
			return origin
		}
		if strings.EqualFold(a, origin) {
			return origin
		}
	}
	return ""
}

// withCORS answers preflights and stamps the response of a cross-origin call.
// Hooks are excluded: each hook already carries its own origin allow-list,
// declared per hook, and two layers disagreeing about one request is worse
// than either.
func withCORS(allow []string, next http.Handler) http.Handler {
	if len(allow) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/hooks/") {
			next.ServeHTTP(w, r)
			return
		}
		origin := allowedOrigin(r.Header.Get("Origin"), allow)
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Type")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if origin == "" {
				writeErr(w, http.StatusForbidden, "origin_not_allowed",
					"this deployment does not accept cross-origin requests from "+r.Header.Get("Origin"))
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- declaring a collection over HTTP --------------------------------------
//
// Normalizers, retention and now an access policy were all CLI-only, which was
// fine while the only administrator had a shell on the box. An access policy
// is what makes a deployment usable by an application, and the deployments
// that need one are exactly the ones behind a reverse proxy that nobody logs
// into. A policy you cannot declare remotely is a policy most people will
// never declare.
//
// These are admin-only and deliberately not part of the policy mechanism: the
// rules that decide who may read a collection are not themselves editable by
// the audiences they name.

func (s *Server) collectionDeclare(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}

	normalize := map[string]string{}
	for field, raw := range mapOf(body["normalize"]) {
		rule, ok := raw.(string)
		if !ok {
			writeErr(w, http.StatusBadRequest, "validation_error", "normalize values must be strings")
			return
		}
		normalize[field] = rule
	}

	retain, setRetain, err := parseRetainBody(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	c, err := s.st.EnsureCollectionWith(ref, normalize, retain, setRetain)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	if rawAccess, present := body["access"]; present {
		acc, err := parseAccessBody(rawAccess)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
		if c, err = s.st.SetAccess(ref, acc); err != nil {
			writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "collection": c})
}

func (s *Server) collectionList(w http.ResponseWriter, r *http.Request) {
	cols, err := s.st.Collections(r.URL.Query().Get("ns"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(cols), "collections": cols})
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func parseRetainBody(body map[string]any) (store.Retention, bool, error) {
	raw, present := body["retain"]
	if !present {
		return store.Retention{}, false, nil
	}
	m := mapOf(raw)
	last := ""
	if n, ok := m["last"].(float64); ok {
		last = strconv.Itoa(int(n))
	}
	var per []string
	for _, f := range listOf(m["per"]) {
		per = append(per, f)
	}
	ret, err := store.ParseRetention(last, per)
	return ret, true, err
}

func parseAccessBody(raw any) (store.Access, error) {
	m := mapOf(raw)
	a := store.Access{}
	a.OwnerField, _ = m["owner_field"].(string)
	a.OrgField, _ = m["org_field"].(string)
	for verb, v := range mapOf(m["rules"]) {
		audience, ok := v.(string)
		if !ok {
			return a, fmt.Errorf("access rule for %q must be a string", verb)
		}
		if a.Rules == nil {
			a.Rules = map[string]string{}
		}
		a.Rules[verb] = audience
	}
	return a, a.Validate()
}

func listOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
