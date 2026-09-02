// Package daemon implements cli-daemon-spec v1.0 process lifecycle:
// start / stop / status built on GET /_health and POST /_shutdown.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/javimosch/bkn/internal/server"
)

// Status is what a health probe learned about a daemon.
type Status struct {
	Running bool   `json:"running"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	PID     int    `json:"pid,omitempty"`
	URL     string `json:"url"`
}

func baseURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// LogPath is where a detached daemon's stdio goes.
func LogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "bkn-daemon.log"
	}
	return filepath.Join(home, ".bkn", "daemon.log")
}

// Probe asks /_health whether a daemon is up. It never retries internally.
func Probe(host string, port int) Status {
	st := Status{Host: host, Port: port, URL: baseURL(host, port)}
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(st.URL + "/_health")
	if err != nil {
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st
	}
	var body struct {
		OK  bool `json:"ok"`
		PID int  `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return st
	}
	st.Running = body.OK
	st.PID = body.PID
	return st
}

// Start launches `bkn serve` detached and polls until it is healthy.
// It is idempotent: an already-running daemon is reported, not duplicated.
func Start(host string, port int) (Status, error) {
	if st := Probe(host, port); st.Running {
		return st, nil
	}

	self, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	logPath := LogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return Status{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Status{}, err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "serve", "--host", host, "--port", strconv.Itoa(port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	detach(cmd) // platform-specific: new session, survives the parent
	if err := cmd.Start(); err != nil {
		return Status{}, err
	}
	_ = cmd.Process.Release()

	// Poll for health rather than sleeping a fixed interval: a bad config
	// should surface as a timeout with a log path, not a false success.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := Probe(host, port); st.Running {
			return st, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return Status{Host: host, Port: port, URL: baseURL(host, port)},
		fmt.Errorf("daemon did not become healthy within 5s; see %s", logPath)
}

// Stop asks a running daemon to exit. Stopping a stopped daemon is a success.
func Stop(host string, port int) (bool, error) {
	st := Probe(host, port)
	if !st.Running {
		return false, nil
	}
	req, err := http.NewRequest(http.MethodPost, st.URL+"/_shutdown", nil)
	if err != nil {
		return false, err
	}
	if !server.IsLoopback(host) {
		tok, err := os.ReadFile(server.TokenPath())
		if err != nil {
			return false, fmt.Errorf("off-loopback shutdown needs the token at %s: %w", server.TokenPath(), err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("shutdown refused: HTTP %d", resp.StatusCode)
	}
	for i := 0; i < 30; i++ {
		if !Probe(host, port).Running {
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true, fmt.Errorf("daemon accepted shutdown but is still answering health")
}
