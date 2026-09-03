package update

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPrefix is where a binary installs itself: no sudo, and already on
// $PATH for most setups.
func DefaultPrefix() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/usr/local/bin"
	}
	return filepath.Join(home, ".local", "bin")
}

// Install copies the running binary to prefix/bkn.
//
// A copy, not a move or a symlink: the original path may be a build artifact
// or a file in Downloads that the caller deletes straight afterwards, and the
// installed copy has to survive that.
func Install(prefix string) (string, error) {
	if prefix == "" {
		prefix = DefaultPrefix()
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	dest := filepath.Join(prefix, "bkn")
	if dest == self {
		return dest, nil // already installed here; idempotent
	}

	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return "", fmt.Errorf("%w: %s", ErrPermission, prefix)
	}
	src, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer src.Close()

	// Write beside the target and rename: replacing a binary that is currently
	// executing fails with ETXTBSY, and a rename swaps the directory entry
	// without touching the running inode.
	staged := dest + ".installing"
	out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		if os.IsPermission(err) {
			return "", fmt.Errorf("%w: %s", ErrPermission, prefix)
		}
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(staged)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(staged)
		return "", err
	}
	if err := os.Rename(staged, dest); err != nil {
		os.Remove(staged)
		return "", err
	}
	return dest, nil
}

// Uninstall removes prefix/bkn, reporting whether anything was there.
// Removing something already absent is a success, not a failure.
func Uninstall(prefix string) (string, bool, error) {
	if prefix == "" {
		prefix = DefaultPrefix()
	}
	dest := filepath.Join(prefix, "bkn")
	err := os.Remove(dest)
	if os.IsNotExist(err) {
		return dest, false, nil
	}
	if os.IsPermission(err) {
		return dest, false, fmt.Errorf("%w: %s", ErrPermission, dest)
	}
	if err != nil {
		return dest, false, err
	}
	return dest, true, nil
}

// OnPath reports whether a directory is on the caller's PATH, so install can
// say so without editing anyone's shell configuration.
func OnPath(dir string) bool {
	for _, entry := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if entry == dir {
			return true
		}
	}
	return false
}
