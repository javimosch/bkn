package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/javimosch/bkn/internal/hooks"
)

// hookLimiter caps deliveries per client IP per minute.
//
// A public form endpoint with no limit is a spam funnel and a cheap way to
// fill somebody else's disk. Webhooks from a provider leave this off (0);
// anything a browser can reach should not.
type hookLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newHookLimiter() *hookLimiter { return &hookLimiter{hits: map[string][]time.Time{}} }

func (l *hookLimiter) allow(key string, perMinute int) bool {
	if perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-time.Minute)
	kept := l.hits[key][:0]
	for _, at := range l.hits[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= perMinute {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, time.Now())
	return true
}

// clientIP prefers the proxy's forwarded address, because behind one every
// request otherwise shares the proxy's IP and the limit becomes global.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) hooksRoutes(mux *http.ServeMux) {
	// Deliberately unauthenticated: the callers are third parties that
	// authenticate with a signature header, and the bound script is
	// responsible for checking it.
	// GET as well as POST: an export or a form-config endpoint is fetched,
	// not posted to. The script sees the method and decides.
	mux.HandleFunc("POST /v1/hooks/{name}", s.hookDeliver)
	mux.HandleFunc("GET /v1/hooks/{name}", s.hookDeliver)
	mux.HandleFunc("OPTIONS /v1/hooks/{name}", s.hookPreflight)

	mux.HandleFunc("GET /v1/hooks", s.guard(s.hookList))
}

func (s *Server) hookList(w http.ResponseWriter, r *http.Request) {
	list, err := s.hooks.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(list), "hooks": list})
}

// hookPreflight answers a browser's CORS preflight for hooks that opt in.
func (s *Server) hookPreflight(w http.ResponseWriter, r *http.Request) {
	h, err := s.hooks.Get(r.PathValue("name"))
	origin := r.Header.Get("Origin")
	if err != nil || !h.OriginAllowed(origin) {
		// Refusing the preflight is the whole enforcement: a browser will not
		// send the real request.
		w.WriteHeader(http.StatusForbidden)
		return
	}
	writeCORS(w, h, origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func writeCORS(w http.ResponseWriter, h hooks.Hook, origin string) {
	if origin == "" || !h.OriginAllowed(origin) {
		return
	}
	// Echo the caller's origin rather than "*" so the header stays correct
	// when the allow-list names specific sites.
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
}

func (s *Server) hookDeliver(w http.ResponseWriter, r *http.Request) {
	h, err := s.hooks.Get(r.PathValue("name"))
	if errors.Is(err, hooks.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "no such hook")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	origin := r.Header.Get("Origin")
	writeCORS(w, h, origin)
	if origin != "" && !h.OriginAllowed(origin) {
		writeErr(w, http.StatusForbidden, "origin_not_allowed",
			"this hook does not accept cross-origin requests from "+origin)
		return
	}
	if !s.hookLimit.allow(h.Name+"|"+clientIP(r), h.RateLimit) {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "rate_limited",
			fmt.Sprintf("this hook accepts %d requests per minute per client", h.RateLimit))
		return
	}

	delivery, err := hooks.ReadDelivery(h.Name, r, h.MaxBytes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable_body", err.Error())
		return
	}

	res, err := s.dispatcher.Deliver(h, delivery)
	if errors.Is(err, hooks.ErrDisabled) {
		writeErr(w, http.StatusServiceUnavailable, "hook_disabled", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	for k, v := range res.Headers {
		w.Header().Set(k, v)
	}
	writeHookBody(w, res)
}

// writeHookBody honours a Content-Type the script chose. A CSV export or an
// RSS feed is a string, and JSON-encoding it would wrap the whole document in
// quotes and escape every newline.
func writeHookBody(w http.ResponseWriter, res hooks.Response) {
	contentType := w.Header().Get("Content-Type")
	text, isText := res.Body.(string)
	if contentType == "" || strings.Contains(contentType, "json") || !isText {
		if contentType == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		writeJSONNoType(w, res.Status, res.Body)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(res.Status)
	_, _ = w.Write([]byte(text))
}
