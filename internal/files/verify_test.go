package files_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/javimosch/bkn/internal/files"
)

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("p", 64))
	zipBytes  = []byte("PK\x03\x04" + strings.Repeat("z", 64))
	htmlBytes = []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>")
	docxType  = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func putTyped(t *testing.T, s *files.Store, ns, name, ct string, body []byte) (files.File, error) {
	t.Helper()
	return s.Put(ns, name, bytes.NewReader(body), files.PutOptions{ContentType: ct})
}

// The point of the flag. Without it the allow-list is checked against a string
// the caller sent, so any bytes at all enter an image-only namespace simply by
// being declared as an image.
func TestVerifyTypeRefusesBytesThatContradictTheClaim(t *testing.T) {
	s, _ := newStore(t)
	for _, tc := range []struct {
		ns     string
		verify bool
	}{{"trusting", false}, {"verifying", true}} {
		if _, err := s.EnsureNamespace(files.Namespace{
			Name: tc.ns, AllowTypes: []string{"image/*"}, VerifyType: tc.verify,
		}); err != nil {
			t.Fatalf("namespace: %v", err)
		}
	}

	// The status quo, stated as a test so nobody has to rediscover it: a
	// namespace that does not verify believes the label.
	if _, err := putTyped(t, s, "trusting", "x.png", "image/png", htmlBytes); err != nil {
		t.Fatalf("a trusting namespace should still accept a declared type: %v", err)
	}

	_, err := putTyped(t, s, "verifying", "x.png", "image/png", htmlBytes)
	if !errors.Is(err, files.ErrTypeMismatch) {
		t.Fatalf("html declared as image/png returned %v, want ErrTypeMismatch", err)
	}
	if _, err := putTyped(t, s, "verifying", "real.png", "image/png", pngBytes); err != nil {
		t.Fatalf("a real png was refused: %v", err)
	}
}

// With no claim at all, the bytes still decide, so an image-only namespace
// cannot be filled with html by simply omitting the header.
func TestVerifyTypeSniffsWhenNothingIsDeclared(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.EnsureNamespace(files.Namespace{
		Name: "ns", AllowTypes: []string{"image/*"}, VerifyType: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Named .png, no declared type, html inside.
	if _, err := putTyped(t, s, "ns", "sneaky.png", "", htmlBytes); !errors.Is(err, files.ErrTypeRefused) {
		t.Fatalf("undeclared html in an image namespace returned %v, want ErrTypeRefused", err)
	}
}

// A sniffer sees bytes, not intentions. Every OOXML document is a zip, so
// refusing that pairing would make the flag unusable for the exact namespace
// that most wants it.
func TestVerifyTypeAllowsAContainerFormatItCannotDistinguish(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.EnsureNamespace(files.Namespace{
		Name: "docs", AllowTypes: []string{docxType}, VerifyType: true,
	}); err != nil {
		t.Fatal(err)
	}
	f, err := putTyped(t, s, "docs", "report.docx", docxType, zipBytes)
	if err != nil {
		t.Fatalf("a docx over zip bytes was refused: %v", err)
	}
	// The declared type is kept, because it is the more specific truth about
	// bytes the sniffer can only call a zip.
	if f.ContentType != docxType {
		t.Errorf("content type = %q, want the declared %q", f.ContentType, docxType)
	}
	// But a claim of a different kind of thing over the same bytes is refused.
	if _, err := putTyped(t, s, "docs", "fake.docx", "image/png", zipBytes); !errors.Is(err, files.ErrTypeMismatch) {
		t.Errorf("image/png over zip bytes returned %v, want ErrTypeMismatch", err)
	}
}

// Namespaces that predate the flag must behave exactly as they did.
func TestVerifyTypeIsOffByDefault(t *testing.T) {
	s, _ := newStore(t)
	ns, err := s.EnsureNamespace(files.Namespace{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if ns.VerifyType {
		t.Fatal("a namespace defaulted to verifying types")
	}
	if _, err := putTyped(t, s, "plain", "x.png", "image/png", htmlBytes); err != nil {
		t.Fatalf("default behaviour changed: %v", err)
	}
}

// Two uploads racing for the same free name: one wins, the other is told the
// name is taken. It used to be a Show followed by an unconditional upsert, so
// both saw the name free and the second silently replaced the first - the name
// still resolved, it just pointed at the other file.
func TestConcurrentPutsCannotSilentlyReplaceEachOther(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.EnsureNamespace(files.Namespace{Name: "race"}); err != nil {
		t.Fatal(err)
	}

	const writers = 12
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		exists int
		other  []error
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(strings.Repeat(string(rune('a'+i)), 128))
			<-start
			_, err := s.Put("race", "contested.bin", bytes.NewReader(body), files.PutOptions{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, files.ErrExists):
				exists++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if wins != 1 {
		t.Errorf("%d writers stored the name, want exactly 1 (the rest must get ErrExists)", wins)
	}
	if wins+exists != writers {
		t.Errorf("accounted for %d of %d writers", wins+exists, writers)
	}

	// And the surviving name resolves to the bytes of whoever actually won.
	f, err := s.Show("race", "contested.bin")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if f.Size != 128 {
		t.Errorf("size = %d, want 128", f.Size)
	}
}

// --overwrite is still how you deliberately replace a file.
func TestOverwriteStillReplaces(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.EnsureNamespace(files.Namespace{Name: "ns"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("ns", "f.bin", strings.NewReader("first"), files.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("ns", "f.bin", strings.NewReader("second"), files.PutOptions{}); !errors.Is(err, files.ErrExists) {
		t.Fatalf("a second put without --overwrite returned %v, want ErrExists", err)
	}
	f, err := s.Put("ns", "f.bin", strings.NewReader("second!"), files.PutOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if f.Size != int64(len("second!")) {
		t.Errorf("size = %d, want %d", f.Size, len("second!"))
	}
}
