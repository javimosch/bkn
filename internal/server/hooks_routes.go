package server

import (
	"errors"
	"net/http"

	"github.com/javimosch/bkn/internal/hooks"
)

func (s *Server) hooksRoutes(mux *http.ServeMux) {
	// Deliberately unauthenticated: the callers are third parties that
	// authenticate with a signature header, and the bound script is
	// responsible for checking it.
	mux.HandleFunc("POST /v1/hooks/{name}", s.hookDeliver)

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
	writeJSON(w, res.Status, res.Body)
}
