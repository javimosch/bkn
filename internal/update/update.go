// Package update implements cli-update-spec v1.1: content-hash versioning, a
// verify-then-swap self-update, a throttled passive nudge, and self-install.
//
// The version is sha256[:12] of the artifact itself, so there is no version
// number to remember to bump: identical bytes are the same version, and a
// rebuild that changes nothing triggers no update.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNoArtifact = errors.New("the server is not publishing an artifact")
	ErrVerify     = errors.New("the download does not match the advertised version")
	ErrSmokeTest  = errors.New("the downloaded binary does not run")
	ErrPermission = errors.New("cannot write to that directory")
)

// DefaultServer is where a binary looks for its own updates.
const DefaultServer = "https://bkn.intrane.fr"

// Release is the /version response.
type Release struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Download string `json:"download"`
	SHA256   string `json:"sha256,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Home is the tool's config directory.
func Home() string {
	if dir := os.Getenv("BKN_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".bkn"
	}
	return filepath.Join(home, ".bkn")
}

// Server resolves the update server base URL.
func Server() string {
	if s := os.Getenv("BKN_SERVER"); s != "" {
		return strings.TrimSuffix(s, "/")
	}
	return DefaultServer
}

// HashFile returns sha256[:12] and the full digest of a file.
func HashFile(path string) (short, full string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	full = hex.EncodeToString(h.Sum(nil))
	return full[:12], full, nil
}

// LocalVersion is the content hash of the running binary.
func LocalVersion() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so an installed copy reached through a link hashes the
	// real file rather than failing to open it.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	short, _, err := HashFile(self)
	return short, err
}

// Fetch asks the server what it is publishing.
func Fetch(timeout time.Duration) (Release, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(Server() + "/version")
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()

	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&rel); err != nil {
		return Release{}, err
	}
	if resp.StatusCode == http.StatusNotFound || !rel.OK {
		if rel.Error != "" {
			return Release{}, fmt.Errorf("%w: %s", ErrNoArtifact, rel.Error)
		}
		return Release{}, ErrNoArtifact
	}
	return rel, nil
}

// Result reports what an update did.
type Result struct {
	Updated bool   `json:"updated"`
	From    string `json:"from"`
	To      string `json:"to"`
	Path    string `json:"path,omitempty"`
	Backup  string `json:"backup,omitempty"`
}

// Apply performs the full check → download → verify → smoke-test → swap.
func Apply(force bool, log func(string, ...any)) (Result, error) {
	local, err := LocalVersion()
	if err != nil {
		return Result{}, err
	}
	rel, err := Fetch(10 * time.Second)
	if err != nil {
		return Result{}, err
	}
	if rel.Version == local && !force {
		return Result{Updated: false, From: local, To: local}, nil
	}

	self, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	unlock, err := lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	// Stage beside the target, never in /tmp: a rename across filesystems
	// fails, and a copy would not be atomic.
	staged := self + ".new"
	log("[update] downloading %s", rel.Version)
	if err := download(rel, staged); err != nil {
		os.Remove(staged)
		return Result{}, err
	}
	defer os.Remove(staged)

	short, full, err := HashFile(staged)
	if err != nil {
		return Result{}, err
	}
	if short != rel.Version || (rel.SHA256 != "" && full != rel.SHA256) {
		return Result{}, fmt.Errorf("%w: got %s, expected %s", ErrVerify, short, rel.Version)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return Result{}, err
	}

	// A truncated publish can still be self-consistent: the hash matches
	// because it was computed over the truncated bytes. Running it is the only
	// check that catches that.
	log("[update] verifying %s runs", rel.Version)
	if err := smokeTest(staged); err != nil {
		return Result{}, err
	}

	backup := self + ".bak"
	if err := os.Rename(self, backup); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staged, self); err != nil {
		// Put the working binary back before reporting; a failed update must
		// never leave the machine without the tool.
		_ = os.Rename(backup, self)
		return Result{}, err
	}
	log("[update] %s -> %s (previous kept at %s)", local, rel.Version, backup)
	return Result{Updated: true, From: local, To: rel.Version, Path: self, Backup: backup}, nil
}

func download(rel Release, dest string) error {
	url := rel.Download
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = Server() + "/" + strings.TrimPrefix(url, "/")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func smokeTest(path string) error {
	cmd := exec.Command(path, "version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSmokeTest, err)
	}
	var probe struct {
		OK   bool   `json:"ok"`
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(out, &probe); err != nil || !probe.OK {
		return fmt.Errorf("%w: unexpected output %q", ErrSmokeTest, strings.TrimSpace(string(out)))
	}
	return nil
}

// lock stops two updates clobbering each other.
func lock() (func(), error) {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(Home(), "update.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// A stale lock from a killed update would otherwise block every
			// future one forever.
			if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 10*time.Minute {
				os.Remove(path)
				return lock()
			}
			return nil, errors.New("another update is in progress")
		}
		return nil, err
	}
	f.Close()
	return func() { os.Remove(path) }, nil
}
