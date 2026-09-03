package files_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/files"
)

func newStore(t *testing.T) (*files.Store, string) {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	root := t.TempDir()
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return files.New(conn, files.NewLocal(root)), root
}

func put(t *testing.T, s *files.Store, ns, name, content string, opts files.PutOptions) files.File {
	t.Helper()
	f, err := s.Put(ns, name, strings.NewReader(content), opts)
	if err != nil {
		t.Fatalf("Put(%s/%s): %v", ns, name, err)
	}
	return f
}

func read(t *testing.T, s *files.Store, ns, name string) string {
	t.Helper()
	_, rc, err := s.Get(ns, name)
	if err != nil {
		t.Fatalf("Get(%s/%s): %v", ns, name, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func mustNS(t *testing.T, s *files.Store, ns files.Namespace) {
	t.Helper()
	if _, err := s.EnsureNamespace(ns); err != nil {
		t.Fatalf("EnsureNamespace(%s): %v", ns.Name, err)
	}
}

// The on-disk path is derived from the content hash, never from the caller's
// filename, so traversal is impossible by construction. The name check exists
// so names stay addressable; both are asserted here.
func TestNamesCannotEscapeTheStore(t *testing.T) {
	s, root := newStore(t)
	mustNS(t, s, files.Namespace{Name: "docs"})

	for _, bad := range []string{
		"../escape", "../../etc/passwd", "a/b", `a\b`, "", ".", "..", strings.Repeat("x", 256),
	} {
		if _, err := s.Put("docs", bad, strings.NewReader("x"), files.PutOptions{}); !errors.Is(err, files.ErrBadName) {
			t.Errorf("Put(%q) = %v, want ErrBadName", bad, err)
		}
	}

	put(t, s, "docs", "ok.txt", "content", files.PutOptions{})
	var found []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 1 {
		t.Fatalf("expected one blob on disk, got %v", found)
	}
	// The stored path names the digest, not "ok.txt".
	if strings.Contains(found[0], "ok.txt") {
		t.Errorf("blob path %q embeds the caller's filename", found[0])
	}
}

// Identical bytes under two names share one blob, so deleting one name must
// not empty the other.
func TestDeduplicationAndReferenceCounting(t *testing.T) {
	s, root := newStore(t)
	mustNS(t, s, files.Namespace{Name: "docs"})

	a := put(t, s, "docs", "a.txt", "same bytes", files.PutOptions{})
	b := put(t, s, "docs", "b.txt", "same bytes", files.PutOptions{})
	if a.SHA256 != b.SHA256 {
		t.Fatalf("hashes differ: %s vs %s", a.SHA256, b.SHA256)
	}
	if n := countBlobs(root); n != 1 {
		t.Errorf("blobs on disk = %d, want 1 (deduplicated)", n)
	}

	if err := s.Delete("docs", "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := read(t, s, "docs", "b.txt"); got != "same bytes" {
		t.Errorf("surviving name reads %q", got)
	}
	if n := countBlobs(root); n != 1 {
		t.Errorf("blobs after one delete = %d, want the shared blob kept", n)
	}

	if err := s.Delete("docs", "b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := countBlobs(root); n != 0 {
		t.Errorf("blobs after the last reference = %d, want 0", n)
	}
}

func countBlobs(root string) int {
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestNamespaceLimitsAreEnforced(t *testing.T) {
	s, _ := newStore(t)
	mustNS(t, s, files.Namespace{Name: "small", MaxBytes: 16})
	mustNS(t, s, files.Namespace{Name: "images", AllowTypes: []string{"image/*"}})

	if _, err := s.Put("small", "ok.txt", strings.NewReader("under the limit"), files.PutOptions{}); err != nil {
		t.Errorf("a file under the limit was refused: %v", err)
	}
	_, err := s.Put("small", "big.txt", strings.NewReader(strings.Repeat("x", 64)), files.PutOptions{})
	if !errors.Is(err, files.ErrTooLarge) {
		t.Errorf("oversized file = %v, want ErrTooLarge", err)
	}

	if _, err := s.Put("images", "a.txt", strings.NewReader("x"), files.PutOptions{ContentType: "text/plain"}); !errors.Is(err, files.ErrTypeRefused) {
		t.Errorf("disallowed type = %v, want ErrTypeRefused", err)
	}
	if _, err := s.Put("images", "a.png", strings.NewReader("x"), files.PutOptions{ContentType: "image/png"}); err != nil {
		t.Errorf("allowed type was refused: %v", err)
	}
}

// An oversized upload must be rejected, never silently truncated to the cap.
func TestOversizedUploadIsNotTruncated(t *testing.T) {
	s, root := newStore(t)
	mustNS(t, s, files.Namespace{Name: "small", MaxBytes: 10})

	_, err := s.Put("small", "big.bin", bytes.NewReader(bytes.Repeat([]byte("a"), 11)), files.PutOptions{})
	if !errors.Is(err, files.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge", err)
	}
	if n := countBlobs(root); n != 0 {
		t.Errorf("a refused upload left %d blobs behind", n)
	}
	if _, err := s.Show("small", "big.bin"); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a refused upload left metadata behind: %v", err)
	}
}

func TestAllowTypeMatching(t *testing.T) {
	cases := []struct {
		allow []string
		ct    string
		want  bool
	}{
		{nil, "anything/at-all", true},
		{[]string{"image/*"}, "image/png", true},
		{[]string{"image/*"}, "image/svg+xml", true},
		{[]string{"image/*"}, "text/plain", false},
		{[]string{"image/png"}, "image/png", true},
		{[]string{"image/png"}, "image/jpeg", false},
		{[]string{"text/plain"}, "text/plain; charset=utf-8", true},
		{[]string{"*/*"}, "application/zip", true},
		{[]string{"image/*"}, "IMAGE/PNG", true},
	}
	for _, tc := range cases {
		ns := files.Namespace{AllowTypes: tc.allow}
		if got := ns.Allows(tc.ct); got != tc.want {
			t.Errorf("Allows(%q) with %v = %v, want %v", tc.ct, tc.allow, got, tc.want)
		}
	}
}

func TestOverwriteIsOptOut(t *testing.T) {
	s, _ := newStore(t)
	mustNS(t, s, files.Namespace{Name: "docs"})
	put(t, s, "docs", "a.txt", "first", files.PutOptions{})

	if _, err := s.Put("docs", "a.txt", strings.NewReader("second"), files.PutOptions{}); !errors.Is(err, files.ErrExists) {
		t.Errorf("silent overwrite = %v, want ErrExists", err)
	}
	put(t, s, "docs", "a.txt", "second", files.PutOptions{Overwrite: true})
	if got := read(t, s, "docs", "a.txt"); got != "second" {
		t.Errorf("after overwrite = %q", got)
	}
}

// Extension wins over sniffing: a .css file of plain words sniffs as
// text/plain and then does not load in a browser.
func TestContentTypeDetection(t *testing.T) {
	s, _ := newStore(t)
	mustNS(t, s, files.Namespace{Name: "assets"})

	cases := map[string]string{
		"style.css": "text/css",
		"app.js":    "text/javascript",
		"page.html": "text/html",
		"data.json": "application/json",
	}
	for name, want := range cases {
		f := put(t, s, "assets", name, "body { color: red }", files.PutOptions{})
		if f.ContentType != want {
			t.Errorf("%s detected as %q, want %q", name, f.ContentType, want)
		}
	}
	// An explicit type always wins.
	f := put(t, s, "assets", "weird.css", "x", files.PutOptions{ContentType: "application/x-thing"})
	if f.ContentType != "application/x-thing" {
		t.Errorf("explicit content type was overridden: %q", f.ContentType)
	}
}

func TestUnknownNamespaceAndBackend(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put("nope", "a.txt", strings.NewReader("x"), files.PutOptions{}); !errors.Is(err, files.ErrNoNamespace) {
		t.Errorf("Put into an unknown namespace = %v, want ErrNoNamespace", err)
	}
	// s3 is not registered in this store, so declaring it must fail up front
	// rather than at the first upload.
	if _, err := s.EnsureNamespace(files.Namespace{Name: "remote", Backend: "s3"}); !errors.Is(err, files.ErrBadBackend) {
		t.Errorf("unconfigured backend = %v, want ErrBadBackend", err)
	}
	if got := s.Available(); len(got) != 1 || got[0] != files.BackendLocal {
		t.Errorf("Available() = %v, want [local]", got)
	}
}

func TestMetadataAndListing(t *testing.T) {
	s, _ := newStore(t)
	mustNS(t, s, files.Namespace{Name: "docs"})
	put(t, s, "docs", "a.txt", "a", files.PutOptions{Metadata: map[string]any{"owner": "ada"}})
	put(t, s, "docs", "b.txt", "b", files.PutOptions{})

	f, err := s.Show("docs", "a.txt")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if f.Metadata["owner"] != "ada" {
		t.Errorf("metadata = %v", f.Metadata)
	}
	list, err := s.List("docs", 50, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d files, %v", len(list), err)
	}
	nss, err := s.Namespaces()
	if err != nil || len(nss) != 1 || nss[0].Count != 2 || nss[0].Bytes != 2 {
		t.Errorf("Namespaces = %+v, %v", nss, err)
	}
}

func TestDeleteNamespaceRemovesEverything(t *testing.T) {
	s, root := newStore(t)
	mustNS(t, s, files.Namespace{Name: "temp"})
	put(t, s, "temp", "a.txt", "a", files.PutOptions{})
	put(t, s, "temp", "b.txt", "b", files.PutOptions{})

	if err := s.DeleteNamespace("temp"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if n := countBlobs(root); n != 0 {
		t.Errorf("blobs left after deleting the namespace: %d", n)
	}
	if _, err := s.Namespace("temp"); !errors.Is(err, files.ErrNoNamespace) {
		t.Errorf("namespace still present: %v", err)
	}
}
