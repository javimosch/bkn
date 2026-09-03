package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/javimosch/bkn/internal/feedback"
	"github.com/javimosch/bkn/internal/store"
	"github.com/javimosch/bkn/internal/update"
)

// ReleaseDir is where a serving instance publishes the artifact it wants its
// clients to update to.
func ReleaseDir() string {
	if dir := os.Getenv("BKN_RELEASE_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(update.Home(), "release")
}

func artifactPath() string { return filepath.Join(ReleaseDir(), "bkn") }

func (s *Server) lifecycleRoutes(mux *http.ServeMux) {
	// Both are open: a liveness or update check that needs a credential
	// cannot tell you that credentials themselves are broken.
	mux.HandleFunc("GET /version", s.serveVersion)
	mux.HandleFunc("GET /dl/bkn", s.serveArtifact)

	// cli-feedback-spec: open, size-capped, rate-limited, idempotent on id.
	mux.HandleFunc("POST /v1/feedback", s.serveFeedback)
}

func (s *Server) serveVersion(w http.ResponseWriter, r *http.Request) {
	short, full, err := update.HashFile(artifactPath())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "no artifact published in " + ReleaseDir(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": short, "download": "/dl/bkn", "sha256": full,
	})
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(artifactPath())
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no artifact published")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="bkn"`)
	http.ServeContent(w, r, "bkn", info.ModTime(), f)
}

// feedbackCollection is where submissions land. Storing them is the store
// primitive doing its job; there is no feedback table.
const feedbackCollection = "feedback/messages"

func (s *Server) serveFeedback(w http.ResponseWriter, r *http.Request) {
	if !s.feedbackLimit.allow("feedback|"+clientIP(r), 30) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error": "too many submissions; try again shortly",
		})
		return
	}

	body := http.MaxBytesReader(w, r.Body, feedback.MaxBytes)
	var sub feedback.Submission
	if err := decodeInto(body, &sub); err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "error": "submission exceeds the size cap",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "body must be a JSON object",
		})
		return
	}
	if strings.TrimSpace(sub.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "message is required",
		})
		return
	}
	if sub.ID == "" {
		// The client may omit the id; the server generates one and echoes it,
		// so the caller can still deduplicate a retry.
		sub.ID = feedback.NewID()
	}

	ref, err := store.ParseRef(feedbackCollection)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Idempotent on id: a duplicate delivery is a success, not a second row.
	if _, _, err := s.st.PutIfAbsent(ref, sub.ID, map[string]any{
		"app": clamp(sub.App, 64), "message": sub.Message,
		"kind": clamp(sub.Kind, 24), "version": clamp(sub.Version, 32),
		"context": sub.Context, "reporter": clamp(sub.Reporter, 64),
		"received_at": nowRFC3339(),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "could not store the submission",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": sub.ID, "stored": true})
}

// clamp trims an oversized field rather than rejecting the whole submission.
func clamp(v string, max int) string {
	if len(v) > max {
		return v[:max]
	}
	return v
}
