package files

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Local stores blobs under a root directory, one file per content hash.
type Local struct{ root string }

// DefaultLocalRoot is where blobs live when BKN_FILES_DIR is unset.
func DefaultLocalRoot() string {
	if p := os.Getenv("BKN_FILES_DIR"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "bkn-files"
	}
	return filepath.Join(home, ".bkn", "files")
}

func NewLocal(root string) *Local {
	if root == "" {
		root = DefaultLocalRoot()
	}
	return &Local{root: root}
}

func (l *Local) Name() string { return BackendLocal }
func (l *Local) Root() string { return l.root }

// path maps a content key to a file path.
//
// Keys are built from a namespace and a hex digest, both of which the caller
// controls fully, so no user-supplied filename ever reaches the filesystem.
// The join is still checked, because "no user input reaches here" is the kind
// of invariant that quietly stops being true.
func (l *Local) path(key string) (string, error) {
	full := filepath.Join(l.root, filepath.FromSlash(key))
	clean := filepath.Clean(full)
	if !strings.HasPrefix(clean, filepath.Clean(l.root)+string(os.PathSeparator)) {
		return "", ErrBadName
	}
	return clean, nil
}

// Put writes the blob unless an identical one is already there.
func (l *Local) Put(key string, r io.Reader, _ string) (string, error) {
	dest, err := l.path(key)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dest); err == nil {
		// Same content hash, same bytes: rewriting would only risk corrupting
		// a blob another name is reading.
		return key, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}

	// Write to a temporary file and rename, so a reader never sees a partial
	// blob under a name that promises a specific hash.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return key, nil
}

func (l *Local) Get(location string) (io.ReadCloser, error) {
	p, err := l.path(location)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *Local) Delete(location string) error {
	p, err := l.path(location)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
