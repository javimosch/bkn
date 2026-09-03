package update_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/javimosch/bkn/internal/update"
)

func TestHashFileIsTheVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact")
	content := []byte("some bytes")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	short, full, err := update.HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	if full != want || short != want[:12] {
		t.Errorf("hash = %s / %s, want %s", short, full, want)
	}

	// Identical bytes are the same version: no bump discipline needed, and a
	// rebuild that changes nothing triggers no update.
	same := filepath.Join(dir, "copy")
	if err := os.WriteFile(same, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	other, _, err := update.HashFile(same)
	if err != nil || other != short {
		t.Errorf("identical content hashed differently: %s vs %s", other, short)
	}
}

func TestInstallCopiesAndIsIdempotent(t *testing.T) {
	prefix := t.TempDir()

	dest, err := update.Install(prefix)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if dest != filepath.Join(prefix, "bkn") {
		t.Errorf("installed at %q", dest)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("the installed file is not executable")
	}

	// Installing twice is not an error: an agent re-running a setup step must
	// never see a false failure.
	before := info.Size()
	if _, err := update.Install(prefix); err != nil {
		t.Errorf("second Install: %v", err)
	}
	after, _ := os.Stat(dest)
	if after.Size() != before {
		t.Errorf("size changed on reinstall: %d -> %d", before, after.Size())
	}
}

func TestInstallRefusesAnUnwritablePrefixWithoutEscalating(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })

	if _, err := update.Install(filepath.Join(parent, "nested")); err == nil {
		t.Fatal("Install succeeded into an unwritable prefix")
	} else if !isPermission(err) {
		t.Errorf("error = %v, want a permission error", err)
	}
}

func isPermission(err error) bool {
	return err != nil && (os.IsPermission(err) ||
		containsStr(err.Error(), "cannot write to that directory"))
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// Removing something that is not there is a success, not a failure.
func TestUninstallIsANoOpWhenAbsent(t *testing.T) {
	prefix := t.TempDir()

	path, removed, err := update.Uninstall(prefix)
	if err != nil {
		t.Fatalf("Uninstall on an empty prefix: %v", err)
	}
	if removed {
		t.Error("reported removing a file that was never there")
	}
	if path != filepath.Join(prefix, "bkn") {
		t.Errorf("path = %q", path)
	}

	if _, err := update.Install(prefix); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, removed, err = update.Uninstall(prefix); err != nil || !removed {
		t.Errorf("Uninstall = %v, %v, want removed", removed, err)
	}
	if _, removed, err = update.Uninstall(prefix); err != nil || removed {
		t.Errorf("second Uninstall = %v, %v, want a successful no-op", removed, err)
	}
}

func TestServerResolution(t *testing.T) {
	t.Setenv("BKN_SERVER", "")
	if got := update.Server(); got != update.DefaultServer {
		t.Errorf("Server() = %q, want the compiled-in default", got)
	}
	t.Setenv("BKN_SERVER", "http://localhost:9000/")
	if got := update.Server(); got != "http://localhost:9000" {
		t.Errorf("Server() = %q, want the trailing slash trimmed", got)
	}
}

func TestOnPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+"/opt/tools")
	if !update.OnPath("/opt/tools") {
		t.Error("a directory on PATH was not detected")
	}
	if update.OnPath("/somewhere/else") {
		t.Error("a directory not on PATH was reported as present")
	}
}
