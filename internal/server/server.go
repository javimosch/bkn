// Package server exposes the primitives over HTTP and implements the
// cli-daemon-spec v1.0 lifecycle endpoints.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/cron"
	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/guide"
	"github.com/javimosch/bkn/internal/hooks"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
	"github.com/javimosch/bkn/internal/update"
)

// Config configures a serve run.
type Config struct {
	Host    string
	Port    int
	Version string
}

// Server wires the primitives to HTTP routes.
type Server struct {
	cfg           Config
	st            *store.Store
	kv            *kv.KV
	reg           *script.Registry
	runner        *script.Runner
	auth          *auth.Auth
	files         *files.Store
	events        *events.Log
	cron          *cron.Registry
	scheduler     *cron.Scheduler
	hooks         *hooks.Registry
	dispatcher    *hooks.Dispatcher
	hookLimit     *hookLimiter
	feedbackLimit *hookLimiter
	throttle      *loginThrottle
	token         string // shutdown token, non-empty only when bound off-loopback
	admin         string // BKN_ADMIN_TOKEN, gates every non-public route
	srv           *http.Server
}

// IsLoopback reports whether host addresses only the local machine.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TokenPath is where the shutdown token is written so `daemon stop` can read
// it. It resolves through the tool's home directory rather than hardcoding
// ~/.bkn, so a deployment that relocates BKN_HOME does not scatter one file
// outside it.
func TokenPath() string {
	return filepath.Join(update.Home(), "shutdown.token")
}

// New builds a Server, enforcing the bind-safety rules before anything listens.
func New(cfg Config, st *store.Store, k *kv.KV, reg *script.Registry, runner *script.Runner, a *auth.Auth, f *files.Store, e *events.Log,
	cronReg *cron.Registry, scheduler *cron.Scheduler,
	hookReg *hooks.Registry, dispatcher *hooks.Dispatcher) (*Server, error) {
	s := &Server{
		cfg: cfg, st: st, kv: k, reg: reg, runner: runner, auth: a, files: f,
		events: e, cron: cronReg, scheduler: scheduler,
		hooks: hookReg, dispatcher: dispatcher,
		hookLimit: newHookLimiter(), feedbackLimit: newHookLimiter(),
		throttle: newLoginThrottle(), admin: os.Getenv("BKN_ADMIN_TOKEN"),
	}

	if !IsLoopback(cfg.Host) {
		// Off-loopback the box is reachable by others, so neither the data
		// routes nor the shutdown button may be left open.
		if s.admin == "" {
			return nil, errors.New("refusing to bind " + cfg.Host + " without BKN_ADMIN_TOKEN set")
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		s.token = hex.EncodeToString(buf)
		p := TokenPath()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte(s.token), 0o600); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// --- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSONNoType(w, status, v)
}

// writeJSONNoType encodes without touching Content-Type, for callers that
// have already chosen one.
func writeJSONNoType(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return after
	}
	return ""
}

// proxied reports that a request reached us through a reverse proxy rather
// than directly.
//
// This is the difference between a safe deployment and an open one. The
// loopback exemption below reasons that only a co-resident process can reach
// 127.0.0.1 — true of a laptop, false the moment nginx, Caddy or Traefik
// listens publicly and forwards to a loopback-bound backend, which is the
// standard way to deploy this. A forwarded request is not a local one, so it
// never gets the exemption.
func proxied(r *http.Request) bool {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host",
		"X-Real-Ip", "Forwarded", "X-Forwarded-Proto"} {
		if r.Header.Get(header) != "" {
			return true
		}
	}
	return false
}

// authed gates every route that is not explicitly public. With no admin token
// configured, a directly-reached loopback server is open (the single-user dev
// default); an off-loopback one never reaches here because New refuses to
// build it.
func (s *Server) authed(r *http.Request) bool {
	if s.admin == "" {
		return IsLoopback(s.cfg.Host) && !proxied(r)
	}
	return subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.admin)) == 1
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusForbidden, "not_authenticated", "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

func decodeBody(r *http.Request) (map[string]any, error) {
	var m map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// --- routes ---------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// cli-daemon-spec: health is open so a broken auth config is still probeable.
	mux.HandleFunc("GET /_health", s.handleHealth)
	mux.HandleFunc("POST /_shutdown", s.handleShutdown)

	// cli-guide-spec: the guide is documentation, so it is public.
	mux.HandleFunc("GET /guide", func(w http.ResponseWriter, r *http.Request) {
		body, err := guide.Body(s.cfg.Version)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "1.0", "guide": body})
	})
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, guide.LLMsTxt(s.cfg.Version))
	})

	mux.HandleFunc("GET /v1/store/{ns}/{coll}", s.guard(s.storeList))
	mux.HandleFunc("POST /v1/store/{ns}/{coll}", s.guard(s.storePut))
	mux.HandleFunc("GET /v1/store/{ns}/{coll}/{id}", s.guard(s.storeGet))
	mux.HandleFunc("PATCH /v1/store/{ns}/{coll}/{id}", s.guard(s.storePatch))
	mux.HandleFunc("DELETE /v1/store/{ns}/{coll}/{id}", s.guard(s.storeDelete))

	s.authRoutes(mux)
	s.filesRoutes(mux)
	s.eventsRoutes(mux)
	s.hooksRoutes(mux)
	s.lifecycleRoutes(mux)

	mux.HandleFunc("GET /v1/script", s.guard(s.scriptList))
	mux.HandleFunc("POST /v1/script/{name}/run", s.guard(s.scriptRun))
	mux.HandleFunc("GET /v1/script/{name}/runs", s.guard(s.scriptRuns))

	mux.HandleFunc("GET /v1/kv", s.kvList)
	mux.HandleFunc("GET /v1/kv/{key}", s.kvGet)
	mux.HandleFunc("POST /v1/kv/{key}", s.guard(s.kvSet))
	mux.HandleFunc("DELETE /v1/kv/{key}", s.guard(s.kvDelete))

	return mux
}

// handleHealth reports liveness, and means it.
//
// Answering 200 because the listener is up says only that Go is running. This
// process is useless without its datastore, so the check touches it - with a
// trivial query and a hard deadline, because a liveness probe that can hang is
// worse than one that lies.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()

	if err := s.st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "service": "bkn", "pid": os.Getpid(), "reason": "datastore unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "bkn", "pid": os.Getpid(),
	})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(s.token)) != 1 {
		writeErr(w, http.StatusForbidden, "not_authenticated", "shutdown requires a bearer token off loopback")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopping": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush() // the response must land before the process goes away
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}()
}

func isOp(candidate string) bool {
	for _, op := range store.Ops() {
		if op == candidate {
			return true
		}
	}
	return false
}

func parseRef(w http.ResponseWriter, r *http.Request) (store.Ref, bool) {
	ref, err := store.ParseRef(r.PathValue("ns") + "/" + r.PathValue("coll"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_arguments", err.Error())
		return store.Ref{}, false
	}
	return ref, true
}

func (s *Server) storeList(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var filters []store.Filter
	for field, vals := range q {
		switch field {
		case "limit", "offset", "order_by", "order", "by", "by_limit":
			continue
		}
		// A query parameter may carry an operator: ?price=gt:20
		f := store.Filter{Field: field, Op: store.OpEq, Value: vals[0]}
		if op, value, ok := strings.Cut(vals[0], ":"); ok && isOp(op) {
			f.Op = store.Op(op)
			f.Value = value
			if f.Op == store.OpIn {
				f.Values = strings.Split(value, ",")
			}
		}
		filters = append(filters, f)
	}
	// ?by=<field> asks a different question of the same collection: how many
	// documents in each group, rather than which documents. The reserved
	// parameter names above already work this way for limit and order_by.
	if by := q.Get("by"); by != "" {
		byLimit, _ := strconv.Atoi(q.Get("by_limit"))
		rollup, err := s.st.CountBy(ref, filters, by, byLimit)
		if errors.Is(err, store.ErrBadGroupBy) {
			writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "storage_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "collection": ref.String(), "by": rollup.By,
			"total": rollup.Total, "groups": rollup.Groups,
			"truncated": rollup.Truncated(), "buckets": rollup.Buckets,
		})
		return
	}

	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	recs, err := s.st.List(ref, store.ListOptions{
		Filters: filters, OrderBy: q.Get("order_by"),
		Desc:  q.Get("order") != "asc",
		Limit: limit, Offset: offset,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	total, err := s.st.Count(ref, filters)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "collection": ref.String(), "count": len(recs),
		"total": total, "records": recs,
	})
}

func (s *Server) storePut(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	doc, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	rec, err := s.st.Put(ref, r.URL.Query().Get("id"), doc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

func (s *Server) storeGet(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	rec, err := s.st.Get(ref, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

func (s *Server) storePatch(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	fields, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	// Preconditions mirror the CLI's --if / --if-absent. They live in the
	// query rather than the body so the body stays exactly the patch.
	opts := store.PatchOptions{IfAbsent: r.URL.Query()["if-absent"]}
	for _, cond := range r.URL.Query()["if"] {
		field, want, ok := strings.Cut(cond, "=")
		if !ok || field == "" {
			writeErr(w, http.StatusBadRequest, "validation_error", "if must be field=value, got "+cond)
			return
		}
		if opts.If == nil {
			opts.If = map[string]string{}
		}
		opts.If[field] = want
	}
	rec, err := s.st.PatchWith(ref, r.PathValue("id"), fields, opts)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoCollection) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if errors.Is(err, store.ErrPrecondition) || errors.Is(err, store.ErrConcurrent) {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

func (s *Server) storeDelete(w http.ResponseWriter, r *http.Request) {
	ref, ok := parseRef(w, r)
	if !ok {
		return
	}
	err := s.st.Delete(ref, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": r.PathValue("id")})
}

func (s *Server) scriptList(w http.ResponseWriter, r *http.Request) {
	scripts, err := s.reg.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(scripts), "scripts": scripts})
}

func (s *Server) scriptRun(w http.ResponseWriter, r *http.Request) {
	var input any = map[string]any{}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(&input); err != nil {
			writeErr(w, http.StatusBadRequest, "validation_error", "body must be JSON")
			return
		}
	}
	res, err := s.runner.Run(r.PathValue("name"), input)
	if errors.Is(err, script.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if errors.Is(err, script.ErrDisabled) {
		writeErr(w, http.StatusForbidden, "script_disabled", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// A script that failed is a completed request that reports a failure, not
	// a transport-level error: the caller still wants the logs and the run id.
	status := http.StatusOK
	if !res.OK {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"ok": res.OK, "value": res.Value, "run": res.Run})
}

func (s *Server) scriptRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.reg.Runs(r.PathValue("name"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(runs), "runs": runs})
}

// kvList is public when ?public=1; anything broader needs the admin token.
func (s *Server) kvList(w http.ResponseWriter, r *http.Request) {
	publicOnly := r.URL.Query().Get("public") == "1"
	if !publicOnly && !s.authed(r) {
		writeErr(w, http.StatusForbidden, "not_authenticated", "listing non-public entries requires a bearer token")
		return
	}
	entries, err := s.kv.List(r.URL.Query().Get("prefix"), publicOnly)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(entries), "entries": entries})
}

func (s *Server) kvGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	// Access is decided on metadata alone. Decrypting first would let a
	// decrypt-time error reveal that a private key exists.
	meta, err := s.kv.Meta(key)
	if errors.Is(err, kv.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if !meta.Public && !s.authed(r) {
		// Same response as a missing key: whether a private setting exists is
		// itself information.
		writeErr(w, http.StatusNotFound, "not_found", "key not found")
		return
	}

	e, err := s.kv.Get(key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entry": e})
}

func (s *Server) kvSet(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	str := func(k string) string {
		if v, ok := body[k].(string); ok {
			return v
		}
		return ""
	}
	typ := str("type")
	if typ == "" {
		typ = kv.TypeString
	}
	public, _ := body["public"].(bool)
	e, err := s.kv.Set(r.PathValue("key"), str("value"), typ, str("description"), public)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entry": e})
}

func (s *Server) kvDelete(w http.ResponseWriter, r *http.Request) {
	err := s.kv.Delete(r.PathValue("key"))
	if errors.Is(err, kv.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": r.PathValue("key")})
}

// ListenAndServe blocks until the server stops.
func (s *Server) ListenAndServe() error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// cli-daemon-spec §1: one confirmation line on stderr, flushed before the
	// accept loop, so a scripted "launch then poll" has something to wait on.
	fmt.Fprintf(os.Stderr, "[serve] listening on http://%s\n", addr)
	if s.admin == "" && IsLoopback(s.cfg.Host) {
		// Say it at startup, because the failure mode is silent: everything
		// works, and the data is public.
		fmt.Fprintf(os.Stderr, "[serve] no BKN_ADMIN_TOKEN: admin routes are open to "+
			"direct local callers. Requests arriving through a proxy are refused; "+
			"set BKN_ADMIN_TOKEN before putting this behind one.\n")
	}
	if s.token != "" {
		fmt.Fprintf(os.Stderr, "[serve] off-loopback bind: shutdown token at %s\n", TokenPath())
	}
	// The scheduler lives for as long as the listener does. Cron jobs fire
	// only while a daemon is running; `bkn cron tick` covers the case where
	// something else owns the timing.
	if s.scheduler != nil {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		go s.scheduler.Start(ctx)
		fmt.Fprintf(os.Stderr, "[serve] cron scheduler ticking every %s\n", cron.TickInterval)
	}

	err = s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
