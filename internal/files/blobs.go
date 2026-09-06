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

	// A namespace that verifies types decides from the bytes, and the
	// uploader's claim decides nothing.
	//
	// Without this the allow-list is checked against a string the caller sent:
	// declare Content-Type: image/png over any bytes at all and an image-only
	// namespace accepts them and records them as an image. That was harmless
	// while the only writer held the admin token. It stops being harmless the
	// moment a tenant can upload, which is the direction this is heading.
	//
	// The cost is honest and worth stating: sniffing sees bytes, not
	// intentions, so a namespace with VerifyType must allow-list what files
	// look like rather than what they are called. A .docx IS a zip and sniffs
	// as application/zip; a .css full of words sniffs as text/plain. A format
	// that is a container of another format cannot be told apart this way by
	// anyone, which is why a caller needing that distinction still validates
	// it itself.
	contentType := opts.ContentType
	if ns.VerifyType {
		sniffed := sniffContentType(buf)
		if contentType == "" {
			// Nothing was claimed, so the bytes are the only evidence. This is
			// what stops an image-only namespace being filled with html by
			// simply omitting the header and naming the file .png - the name
			// is not evidence either.
			contentType = sniffed
		} else if !typesAgree(contentType, sniffed) {
			return File{}, fmt.Errorf("%w: uploaded bytes look like %q, not the %q that was declared, "+
				"and namespace %q verifies types",
				ErrTypeMismatch, sniffed, contentType, nsName)
		}
		// A claim that survives typesAgree is kept: it is the more specific
		// truth about bytes a sniffer can only call a zip, and it is what the
		// allow-list and the serving header are written against.
	}
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
	// Claiming a name and writing it are one statement.
	//
	// This used to be a Show that returned ErrExists followed, some way down,
	// by an unconditional upsert - so two uploads of the same new name both
	// saw it free and both wrote, and the second silently replaced the first.
	// The window is small and the consequence is not: the name still resolves,
	// so nothing looks wrong, it just points at the other file. DO NOTHING
	// plus a zero rows-affected turns that into ErrExists for whoever lost.
	conflict := `DO UPDATE SET
		  sha256=excluded.sha256, size=excluded.size, content_type=excluded.content_type,
		  backend=excluded.backend, location=excluded.location,
		  metadata=excluded.metadata, updated_at=excluded.updated_at`
	if !opts.Overwrite {
		conflict = `DO NOTHING`
	}
	res, err := s.db.Exec(`
		INSERT INTO files (id, ns, name, sha256, size, content_type, backend, location, metadata, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ns, name) `+conflict,
		f.ID, f.Namespace, f.Name, f.SHA256, f.Size, f.ContentType, f.Backend,
		f.location, string(metaJSON), f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return File{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return File{}, ErrExists
	}
	return f, nil
}

// sniffContentType asks the bytes, ignoring the name. detectContentType
// prefers the extension, which is fine for deciding how to serve a file the
// operator put there and useless for verification - the extension is part of
// what an uploader controls.
func sniffContentType(buf []byte) string {
	if len(buf) > 512 {
		buf = buf[:512]
	}
	return strings.Split(httpDetect(buf), ";")[0]
}

// typesAgree reports whether a declared content type is consistent with what
// the bytes sniff as.
//
// Exact equality is too strict to be usable: every OOXML document (.docx,
// .xlsx, .pptx) is a zip, and a JPEG may be declared as the more specific
// image/jpeg while sniffing identically. The rule is that a claim may be more
// specific than the sniff within the same container, never a different kind of
// thing - so image/png over zip bytes is refused, and the docx-over-zip case
// that a byte sniffer genuinely cannot distinguish is allowed through.
func typesAgree(declared, sniffed string) bool {
	d := strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	s := strings.ToLower(strings.TrimSpace(strings.Split(sniffed, ";")[0]))
	if d == s {
		return true
	}
	// application/octet-stream is the sniffer saying "no idea", which is not
	// evidence of a lie - and is the honest limit of this check. Arbitrary
	// binary that resembles no known format can still be declared as anything
	// the allow-list permits. What the check does remove is the whole class
	// the sniffer CAN name: html, javascript, svg and every other text format
	// that a browser would act on.
	if s == "application/octet-stream" {
		return true
	}
	if zipContainers[d] && s == "application/zip" {
		return true
	}
	// text/plain covers every text format the sniffer cannot name: css, csv,
	// json, markdown.
	if s == "text/plain" && (strings.HasPrefix(d, "text/") || textLike[d]) {
		return true
	}
	return false
}

// zipContainers are formats that are a zip archive underneath, so a sniffer
// reports application/zip and cannot say more.
var zipContainers = map[string]bool{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	"application/vnd.oasis.opendocument.text":                                   true,
	"application/vnd.oasis.opendocument.spreadsheet":                            true,
	"application/epub+zip": true,
	"application/zip":      true,
}

// textLike are text formats whose media type does not start with "text/".
var textLike = map[string]bool{
	"application/json":       true,
	"application/xml":        true,
	"application/javascript": true,
	"image/svg+xml":          true,
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
