package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/javimosch/bkn/internal/files"
)

// inlineTypes may be rendered in the browser. Everything else is served as a
// download.
//
// A file store that serves arbitrary user uploads inline from the API's own
// origin is a stored-XSS delivery mechanism: an uploaded .html or .svg runs
// script with the origin's privileges. SVG is excluded for exactly that
// reason even though it is an image, and text/html never appears here.
var inlineTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"image/avif":      true,
	"text/plain":      true,
	"audio/mpeg":      true,
	"audio/ogg":       true,
	"video/mp4":       true,
	"video/webm":      true,
	"application/pdf": false, // a PDF can carry script; make it a download
}

func filesStatus(err error) (int, string) {
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrNoNamespace):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, files.ErrExists):
		return http.StatusConflict, "already_exists"
	case errors.Is(err, files.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, files.ErrTypeRefused):
		return http.StatusUnsupportedMediaType, "type_refused"
	case errors.Is(err, files.ErrTypeMismatch):
		return http.StatusUnsupportedMediaType, "type_mismatch"
	case errors.Is(err, files.ErrBadName), errors.Is(err, files.ErrBadNamespace),
		errors.Is(err, files.ErrBadBackend):
		return http.StatusBadRequest, "validation_error"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func (s *Server) filesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/files", s.guard(s.filesNamespaces))
	mux.HandleFunc("GET /v1/files/{ns}", s.guard(s.filesList))
	mux.HandleFunc("GET /v1/files/{ns}/{name}", s.filesServe)
	mux.HandleFunc("POST /v1/files/{ns}/{name}", s.guard(s.filesPut))
	mux.HandleFunc("DELETE /v1/files/{ns}/{name}", s.guard(s.filesDelete))
}

func (s *Server) filesNamespaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.files.Namespaces()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(list), "namespaces": list,
		"backends_available": s.files.Available(),
	})
}

func (s *Server) filesList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	list, err := s.files.List(r.PathValue("ns"), limit, offset)
	if err != nil {
		status, typ := filesStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "namespace": r.PathValue("ns"), "count": len(list), "files": list,
	})
}

// filesServe streams a file's bytes. A public namespace needs no auth; every
// other one requires either an authed session or a valid signed URL
// (?sig=...&exp=... when the namespace has a signing_key).
func (s *Server) filesServe(w http.ResponseWriter, r *http.Request) {
	nsName := r.PathValue("ns")
	ns, err := s.files.Namespace(nsName)
	if err != nil {
		// A private namespace and a missing one look identical to an
		// unauthorized caller.
		writeErr(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if !ns.Public && !s.authed(r) {
		// Check for a signed URL before rejecting.
		if ns.SigningKey != "" {
			sig := r.URL.Query().Get("sig")
			expStr := r.URL.Query().Get("exp")
			if sig != "" && expStr != "" {
				exp, err := strconv.ParseInt(expStr, 10, 64)
				if err == nil && files.VerifySignature(nsName, r.PathValue("name"), ns.SigningKey, sig, exp) {
					// Signature valid — serve the file.
					goto serve
				}
			}
		}
		writeErr(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
serve:
	f, rc, err := s.files.Get(nsName, r.PathValue("name"))
	if err != nil {
		status, typ := filesStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	defer rc.Close()

	// The content hash is a perfect ETag: identical bytes, identical tag.
	etag := `"` + f.SHA256 + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	disposition := "attachment"
	if inlineTypes[strings.ToLower(f.ContentType)] {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", disposition+"; filename="+strconv.Quote(f.Name))
	// nosniff stops a browser from second-guessing the type we declared, and
	// the CSP neuters anything that does end up being interpreted.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if _, err := io.Copy(w, rc); err != nil {
		// The status is already written; there is nothing useful to say to
		// the client, and the connection is the error signal.
		return
	}
}

func (s *Server) filesPut(w http.ResponseWriter, r *http.Request) {
	opts := files.PutOptions{
		ContentType: r.Header.Get("Content-Type"),
		Overwrite:   r.URL.Query().Get("overwrite") == "1",
	}
	if opts.ContentType == "application/octet-stream" {
		// The default a client sends when it has no idea; let detection win.
		opts.ContentType = ""
	}
	f, err := s.files.Put(r.PathValue("ns"), r.PathValue("name"), r.Body, opts)
	if err != nil {
		status, typ := filesStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file": f})
}

func (s *Server) filesDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.files.Delete(r.PathValue("ns"), r.PathValue("name")); err != nil {
		status, typ := filesStatus(err)
		writeErr(w, status, typ, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "deleted": r.PathValue("name"), "namespace": r.PathValue("ns"),
	})
}
