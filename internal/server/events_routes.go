package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/javimosch/bkn/internal/cron"
	"github.com/javimosch/bkn/internal/events"
)

func (s *Server) eventsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/events", s.guard(s.eventStreams))
	mux.HandleFunc("GET /v1/events/{stream}", s.guard(s.eventList))
	mux.HandleFunc("POST /v1/events/{stream}", s.guard(s.eventEmit))
	mux.HandleFunc("GET /v1/events/{stream}/stats", s.guard(s.eventStats))

	mux.HandleFunc("GET /v1/cron", s.guard(s.cronList))
	mux.HandleFunc("POST /v1/cron/{name}/run", s.guard(s.cronRun))
}

func eventsStatus(err error) (int, string) {
	switch {
	case errors.Is(err, events.ErrBadStream), errors.Is(err, events.ErrBadLevel),
		errors.Is(err, events.ErrBadGroupBy), errors.Is(err, events.ErrBadDuration):
		return http.StatusBadRequest, "validation_error"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// queryFrom reads the shared event filters off the query string.
func queryFrom(r *http.Request) events.Query {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	return events.Query{
		Type: q.Get("type"), Level: q.Get("level"), Source: q.Get("source"),
		Subject: q.Get("subject"), Since: q.Get("since"), Until: q.Get("until"),
		Limit: limit, Offset: offset,
	}
}

func (s *Server) eventStreams(w http.ResponseWriter, r *http.Request) {
	list, err := s.events.Streams()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(list), "streams": list})
}

func (s *Server) eventList(w http.ResponseWriter, r *http.Request) {
	list, err := s.events.List(r.PathValue("stream"), queryFrom(r))
	if err != nil {
		status, typ := eventsStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stream": r.PathValue("stream"), "count": len(list), "events": list,
	})
}

func (s *Server) eventEmit(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "body must be a JSON object")
		return
	}
	e := events.Event{
		Stream: r.PathValue("stream"),
		Type:   str(body, "type"), Level: str(body, "level"),
		Source: str(body, "source"), Subject: str(body, "subject"),
	}
	if data, ok := body["data"].(map[string]any); ok {
		e.Data = data
	}
	stored, err := s.events.Emit(e)
	if err != nil {
		status, typ := eventsStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "event": stored})
}

func (s *Server) eventStats(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "type"
	}
	buckets, err := s.events.Stats(r.PathValue("stream"), by, queryFrom(r))
	if err != nil {
		status, typ := eventsStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stream": r.PathValue("stream"), "by": by, "total": total, "buckets": buckets,
	})
}

func (s *Server) cronList(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.cron.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(jobs), "jobs": jobs, "scheduler_running": s.scheduler != nil,
	})
}

func (s *Server) cronRun(w http.ResponseWriter, r *http.Request) {
	res, err := s.scheduler.RunNow(r.PathValue("name"))
	if errors.Is(err, cron.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	status := http.StatusOK
	if res.Status != "ok" {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"ok": res.Status == "ok", "result": res})
}
