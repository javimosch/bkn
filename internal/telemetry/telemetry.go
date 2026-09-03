// Package telemetry implements cli-telemetry-spec: one POST, an allow-listed
// payload, disclosed before anything is sent, and off unless someone turns it
// on.
//
// bkn is opt-IN rather than opt-out. The spec permits either and asks for
// opt-in from tools that handle credentials or regulated data; bkn holds
// encrypted secrets, password hashes and token signing keys, so it takes the
// stricter option. Nothing is sent until `bkn telemetry --on`.
package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultEndpoint is overridable so an organisation can point this at its own
// collector - and so a reviewer can aim it somewhere harmless to see what it
// actually sends.
const DefaultEndpoint = "https://telemetry.intrane.fr/e"

// Events. Exactly three, per the spec; a tool must not invent more.
const (
	EventInstall = "install"
	EventRun     = "run"
	EventError   = "error"
)

// Payload is the complete allow-list. A field not here is never sent.
//
// install_id is deliberately omitted. The spec calls omitting it the safer
// default, and the install event already answers "did anyone get it running"
// without giving the collector a way to correlate one machine's runs.
type Payload struct {
	Tool      string `json:"tool"`
	Version   string `json:"version"`
	Event     string `json:"event"`
	Verb      string `json:"verb"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	ExitClass int    `json:"exit_class"`
	TS        string `json:"ts"`
}

type state struct {
	Enabled     bool   `json:"enabled"`
	NoticeShown bool   `json:"notice_shown"`
	InstallSent bool   `json:"install_sent"`
	LastSent    string `json:"last_sent,omitempty"`
	DecidedAt   string `json:"decided_at,omitempty"`
}

// Reporter decides whether to send, and sends.
type Reporter struct {
	home    string
	version string
	state   state
}

func New(home, version string) *Reporter {
	r := &Reporter{home: home, version: version}
	r.state = r.read()
	return r
}

func (r *Reporter) path() string { return filepath.Join(r.home, "telemetry.json") }

func (r *Reporter) read() state {
	var s state
	raw, err := os.ReadFile(r.path())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

func (r *Reporter) write() error {
	if err := os.MkdirAll(r.home, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path(), raw, 0o600)
}

// Endpoint resolves the collector URL.
func Endpoint() string {
	if v := os.Getenv("BKN_TELEMETRY_URL"); v != "" {
		return v
	}
	return DefaultEndpoint
}

func isOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// inCI reports an automation environment. A CI run is not a user, and counting
// one inflates every number the author is trying to read.
func inCI() bool {
	for _, name := range []string{
		"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI",
		"BUILDKITE", "CIRCLECI", "JENKINS_URL", "TEAMCITY_VERSION",
	} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// Decision explains whether telemetry will send, and why.
func (r *Reporter) Decision() (bool, string) {
	// Every switch is checked before any network code runs.
	if os.Getenv("DO_NOT_TRACK") == "1" {
		return false, "DO_NOT_TRACK=1"
	}
	if v := os.Getenv("BKN_TELEMETRY"); v != "" && isOff(v) {
		return false, "BKN_TELEMETRY is off"
	}
	if inCI() {
		return false, "automation environment detected"
	}
	if !r.state.Enabled {
		return false, "not enabled; bkn is opt-in (bkn telemetry --on)"
	}
	return true, "enabled"
}

// ExitClass maps an exit code to its semantic class.
func ExitClass(code int) int {
	switch {
	case code == 0:
		return 0
	case code >= 110:
		return 110
	case code >= 100:
		return 100
	case code >= 90:
		return 90
	case code >= 80:
		return 80
	default:
		return 110
	}
}

// NextPayload is what would be sent for a given verb and exit code.
func (r *Reporter) NextPayload(verb string, exitCode int) Payload {
	event := EventRun
	if exitCode != 0 {
		event = EventError
	} else if !r.state.InstallSent {
		event = EventInstall
	}
	return Payload{
		Tool: "bkn", Version: r.version, Event: event, Verb: verb,
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		ExitClass: ExitClass(exitCode),
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
}

const notice = `[telemetry] bkn sends anonymous usage counts: tool, version, os/arch, which
[telemetry] verb ran, and whether it failed. No identity, arguments, paths or data.
[telemetry] See ` + "`bkn telemetry`" + ` for the exact payload and endpoint.
[telemetry] Disable with BKN_TELEMETRY=0 (or DO_NOT_TRACK=1).`

// Report sends at most one event, after the caller's result is already out.
// A failure is silent: a tool that cannot reach the collector is working.
func (r *Reporter) Report(verb string, exitCode int) {
	if ok, _ := r.Decision(); !ok {
		return
	}
	// Nothing may leave the machine before the notice has been printed.
	if !r.state.NoticeShown {
		os.Stderr.WriteString(notice + "\n")
		r.state.NoticeShown = true
		_ = r.write()
	}

	payload := r.NextPayload(verb, exitCode)
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(Endpoint(), "application/json", bytes.NewReader(raw))
	if err != nil {
		return // silent, and no retry: the agent decides, not the tool
	}
	resp.Body.Close()

	if payload.Event == EventInstall {
		r.state.InstallSent = true
	}
	r.state.LastSent = payload.TS
	_ = r.write()
}

// SetEnabled persists the choice so it survives future runs.
func (r *Reporter) SetEnabled(on bool) error {
	r.state.Enabled = on
	r.state.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	return r.write()
}

// Status is what `bkn telemetry` prints.
func (r *Reporter) Status(verb string) map[string]any {
	enabled, reason := r.Decision()
	return map[string]any{
		"enabled":    enabled,
		"reason":     reason,
		"opt_in":     true,
		"endpoint":   Endpoint(),
		"install_id": nil,
		"install_id_note": "omitted by design: the spec calls this the safer default, " +
			"and nothing here needs to tell repeat runs from new installs",
		"next_payload": r.NextPayload(verb, 0),
		"enable":       "bkn telemetry --on",
		"disable":      "BKN_TELEMETRY=0, DO_NOT_TRACK=1, or bkn telemetry --off",
		"notice_shown": r.state.NoticeShown,
		"last_sent":    r.state.LastSent,
		"notice":       strings.Split(notice, "\n"),
	}
}
