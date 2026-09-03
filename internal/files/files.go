// Package files is the blob primitive: namespaced, content-addressed file
// storage with a pluggable backend.
//
// Content addressing is not a flourish. Storing bytes under their SHA-256 and
// keeping the caller's filename only in metadata means the on-disk path is
// never derived from user input, so directory traversal is impossible by
// construction rather than by validation. Identical uploads also deduplicate,
// which matters for the "same logo in forty namespaces" case the previous
// system handled by storing forty copies.
package files

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("file not found")
	ErrNoNamespace  = errors.New("namespace does not exist")
	ErrBadNamespace = errors.New("namespace must match [a-z][a-z0-9_-]{0,62}")
	ErrBadName      = errors.New("file name must be 1-255 characters and contain no path separators")
	ErrTooLarge     = errors.New("file exceeds the namespace size limit")
	ErrTypeRefused  = errors.New("content type is not allowed in this namespace")
	ErrBadBackend   = errors.New("backend must be one of: local, s3")
	ErrExists       = errors.New("a file with that name already exists in this namespace")
)

const (
	BackendLocal = "local"
	BackendS3    = "s3"

	// DefaultMaxBytes bounds an upload when a namespace sets no limit of its
	// own. A cap that can be raised is safer than no cap at all.
	DefaultMaxBytes = 32 << 20
)

func Backends() []string { return []string{BackendLocal, BackendS3} }

var nsRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// ValidateName rejects anything that could be read as a path.
//
// The storage layout does not use this name, so a bad one cannot escape
// anywhere; the check exists so that names stay addressable and predictable.
func ValidateName(name string) error {
	if name == "" || len(name) > 255 {
		return ErrBadName
	}
	if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return ErrBadName
	}
	if path.Clean(name) != name {
		return ErrBadName
	}
	return nil
}

// Namespace is a storage bucket with its own limits and backend.
type Namespace struct {
	Name       string   `json:"name"`
	Backend    string   `json:"backend"`
	MaxBytes   int64    `json:"max_bytes"`
	AllowTypes []string `json:"allow_types"`
	Public     bool     `json:"public"`
	CreatedAt  string   `json:"created_at"`
	Count      int      `json:"count,omitempty"`
	Bytes      int64    `json:"bytes,omitempty"`
}

// Limit returns the effective size cap.
func (n Namespace) Limit() int64 {
	if n.MaxBytes > 0 {
		return n.MaxBytes
	}
	return DefaultMaxBytes
}

// Allows reports whether a content type may be stored here. An empty
// allow-list means any type; entries may be exact ("image/png") or a prefix
// wildcard ("image/*").
func (n Namespace) Allows(contentType string) bool {
	if len(n.AllowTypes) == 0 {
		return true
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	for _, allowed := range n.AllowTypes {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == ct || allowed == "*/*" {
			return true
		}
		if prefix, ok := strings.CutSuffix(allowed, "/*"); ok {
			if strings.HasPrefix(ct, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// File is a stored blob's metadata. The bytes live in a backend.
type File struct {
	ID          string         `json:"id"`
	Namespace   string         `json:"namespace"`
	Name        string         `json:"name"`
	SHA256      string         `json:"sha256"`
	Size        int64          `json:"size"`
	ContentType string         `json:"content_type"`
	Backend     string         `json:"backend"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	location    string
}

// Backend stores and retrieves bytes by content hash.
type Backend interface {
	// Put writes r under key and returns the location it can be read from.
	Put(key string, r io.Reader, contentType string) (string, error)
	Get(location string) (io.ReadCloser, error)
	Delete(location string) error
	Name() string
}

// Store owns file metadata and dispatches bytes to backends.
type Store struct {
	db       *sql.DB
	backends map[string]Backend
}

func New(db *sql.DB, backends ...Backend) *Store {
	s := &Store{db: db, backends: map[string]Backend{}}
	for _, b := range backends {
		if b != nil {
			s.backends[b.Name()] = b
		}
	}
	return s
}

// Available reports which backends this build has configured.
func (s *Store) Available() []string {
	out := make([]string, 0, len(s.backends))
	for _, name := range Backends() {
		if _, ok := s.backends[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

func (s *Store) backend(name string) (Backend, error) {
	b, ok := s.backends[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q is not configured in this deployment (available: %s)",
			ErrBadBackend, name, strings.Join(s.Available(), ", "))
	}
	return b, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// --- namespaces -----------------------------------------------------------

// EnsureNamespace creates a namespace, or returns the existing one unchanged.
func (s *Store) EnsureNamespace(ns Namespace) (Namespace, error) {
	if !nsRe.MatchString(ns.Name) {
		return Namespace{}, fmt.Errorf("%w: got %q", ErrBadNamespace, ns.Name)
	}
	if ns.Backend == "" {
		ns.Backend = BackendLocal
	}
	if _, err := s.backend(ns.Backend); err != nil {
		return Namespace{}, err
	}
	if ns.AllowTypes == nil {
		ns.AllowTypes = []string{}
	}
	ns.CreatedAt = now()

	types, _ := json.Marshal(ns.AllowTypes)
	pub := 0
	if ns.Public {
		pub = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO file_namespaces (name, backend, max_bytes, allow_types, public, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
		  backend=excluded.backend, max_bytes=excluded.max_bytes,
		  allow_types=excluded.allow_types, public=excluded.public`,
		ns.Name, ns.Backend, ns.MaxBytes, string(types), pub, ns.CreatedAt)
	if err != nil {
		return Namespace{}, err
	}
	return s.Namespace(ns.Name)
}

// Namespace returns one namespace.
func (s *Store) Namespace(name string) (Namespace, error) {
	var ns Namespace
	var types string
	var pub int
	err := s.db.QueryRow(`
		SELECT name, backend, max_bytes, allow_types, public, created_at
		FROM file_namespaces WHERE name = ?`, name).
		Scan(&ns.Name, &ns.Backend, &ns.MaxBytes, &types, &pub, &ns.CreatedAt)
	if err == sql.ErrNoRows {
		return Namespace{}, ErrNoNamespace
	}
	if err != nil {
		return Namespace{}, err
	}
	ns.Public = pub == 1
	ns.AllowTypes = []string{}
	_ = json.Unmarshal([]byte(types), &ns.AllowTypes)
	return ns, nil
}

// Namespaces lists every namespace with its usage.
func (s *Store) Namespaces() ([]Namespace, error) {
	rows, err := s.db.Query(`
		SELECT n.name, n.backend, n.max_bytes, n.allow_types, n.public, n.created_at,
		       (SELECT COUNT(*) FROM files f WHERE f.ns = n.name),
		       (SELECT COALESCE(SUM(f.size), 0) FROM files f WHERE f.ns = n.name)
		FROM file_namespaces n ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Namespace{}
	for rows.Next() {
		var ns Namespace
		var types string
		var pub int
		if err := rows.Scan(&ns.Name, &ns.Backend, &ns.MaxBytes, &types, &pub,
			&ns.CreatedAt, &ns.Count, &ns.Bytes); err != nil {
			return nil, err
		}
		ns.Public = pub == 1
		ns.AllowTypes = []string{}
		_ = json.Unmarshal([]byte(types), &ns.AllowTypes)
		out = append(out, ns)
	}
	return out, rows.Err()
}

// DeleteNamespace removes a namespace and every file in it.
func (s *Store) DeleteNamespace(name string) error {
	list, err := s.List(name, 0, 0)
	if err != nil {
		return err
	}
	for _, f := range list {
		if err := s.Delete(name, f.Name); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(`DELETE FROM file_namespaces WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoNamespace
	}
	return nil
}
