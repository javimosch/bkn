package files

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"

	"github.com/javimosch/bkn/internal/store"
)

// PutOptions describes an upload.
type PutOptions struct {
	ContentType string
	Metadata    map[string]any
	Overwrite   bool
}

// Put stores bytes under ns/name.
//
// The reader is bounded by the namespace's limit and hashed as it is read, so
// an oversized upload is refused without ever being fully buffered or written.
func (s *Store) Put(nsName, name string, r io.Reader, opts PutOptions) (File, error) {
	if err := ValidateName(name); err != nil {
		return File{}, err
	}
	ns, err := s.Namespace(nsName)
	if err != nil {
		return File{}, err
	}
	backend, err := s.backend(ns.Backend)
	if err != nil {
		return File{}, err
	}

	if !opts.Overwrite {
		if _, err := s.Show(nsName, name); err == nil {
			return File{}, ErrExists
		} else if err != ErrNotFound {
			return File{}, err
		}
	}

	limit := ns.Limit()
	// Read one byte past the limit so exceeding it is detectable rather than
	// silently truncating the file to exactly the cap.
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return File{}, err
	}
	if int64(len(buf)) > limit {
		return File{}, fmt.Errorf("%w: %d bytes exceeds the %d byte limit for %q",
			ErrTooLarge, len(buf), limit, nsName)
	}

	contentType := opts.ContentType
	if contentType == "" {
		contentType = detectContentType(name, buf)
	}
	if !ns.Allows(contentType) {
		return File{}, fmt.Errorf("%w: %q is not in %v for namespace %q",
			ErrTypeRefused, contentType, ns.AllowTypes, nsName)
	}

	sum := sha256.Sum256(buf)
	digest := hex.EncodeToString(sum[:])
	key := nsName + "/" + digest[:2] + "/" + digest

	location, err := backend.Put(key, bytes.NewReader(buf), contentType)
	if err != nil {
		return File{}, err
	}

	meta := opts.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return File{}, err
	}

	f := File{
		ID: store.NewID(), Namespace: nsName, Name: name, SHA256: digest,
		Size: int64(len(buf)), ContentType: contentType, Backend: ns.Backend,
		Metadata: meta, CreatedAt: now(), UpdatedAt: now(), location: location,
	}
	if _, err := s.db.Exec(`
		INSERT INTO files (id, ns, name, sha256, size, content_type, backend, location, metadata, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ns, name) DO UPDATE SET
		  sha256=excluded.sha256, size=excluded.size, content_type=excluded.content_type,
		  backend=excluded.backend, location=excluded.location,
		  metadata=excluded.metadata, updated_at=excluded.updated_at`,
		f.ID, f.Namespace, f.Name, f.SHA256, f.Size, f.ContentType, f.Backend,
		f.location, string(metaJSON), f.CreatedAt, f.UpdatedAt); err != nil {
		return File{}, err
	}
	return f, nil
}

// detectContentType prefers the extension and falls back to sniffing, because
// a .css file full of plain words sniffs as text/plain and then does not load.
func detectContentType(name string, sample []byte) string {
	if ext := filepath.Ext(name); ext != "" {
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			return strings.Split(byExt, ";")[0]
		}
	}
	if len(sample) > 512 {
		sample = sample[:512]
	}
	return strings.Split(httpDetect(sample), ";")[0]
}

// Show returns a file's metadata without touching its bytes.
func (s *Store) Show(nsName, name string) (File, error) {
	return s.scanFile(s.db.QueryRow(`
		SELECT id, ns, name, sha256, size, content_type, backend, location, metadata, created_at, updated_at
		FROM files WHERE ns = ? AND name = ?`, nsName, name))
}

func (s *Store) scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	var meta string
	err := row.Scan(&f.ID, &f.Namespace, &f.Name, &f.SHA256, &f.Size, &f.ContentType,
		&f.Backend, &f.location, &meta, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, err
	}
	f.Metadata = map[string]any{}
	_ = json.Unmarshal([]byte(meta), &f.Metadata)
	return f, nil
}

// Get returns a file's metadata and an open reader over its bytes. The caller
// closes the reader.
func (s *Store) Get(nsName, name string) (File, io.ReadCloser, error) {
	f, err := s.Show(nsName, name)
	if err != nil {
		return File{}, nil, err
	}
	backend, err := s.backend(f.Backend)
	if err != nil {
		return File{}, nil, err
	}
	rc, err := backend.Get(f.location)
	if err != nil {
		return File{}, nil, err
	}
	return f, rc, nil
}

// List returns a namespace's files, newest first.
func (s *Store) List(nsName string, limit, offset int) ([]File, error) {
	q := `SELECT id, ns, name, sha256, size, content_type, backend, location, metadata, created_at, updated_at
	      FROM files WHERE ns = ? ORDER BY created_at DESC, id DESC`
	args := []any{nsName}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []File{}
	for rows.Next() {
		f, err := s.scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Delete removes a file's metadata, and its bytes only if nothing else refers
// to them.
//
// Content addressing means two names can share one blob. Deleting the bytes
// whenever a name goes away would silently empty the other one, so the backend
// delete is conditional on the last reference disappearing.
func (s *Store) Delete(nsName, name string) error {
	f, err := s.Show(nsName, name)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM files WHERE ns = ? AND name = ?`, nsName, name); err != nil {
		return err
	}

	var refs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE location = ?`, f.location).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return nil
	}
	backend, err := s.backend(f.Backend)
	if err != nil {
		return err
	}
	return backend.Delete(f.location)
}
