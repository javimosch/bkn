// Package feedback implements cli-feedback-spec v1.0: one submission body,
// dual-written to the tool's own endpoint and to a central relay, idempotent
// on a client-generated id.
package feedback

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultRelay is the shared collector. FEEDBACK_RELAY=off disables it.
const DefaultRelay = "https://feedback.intrane.fr"

// MaxBytes caps a submission body.
const MaxBytes = 16384

var (
	ErrEmpty   = errors.New("message is required")
	ErrTooBig  = errors.New("message is too large")
	ErrBadKind = errors.New("kind must be one of: bug, idea, praise, note")
)

func Kinds() []string { return []string{"bug", "idea", "praise", "note"} }

// Submission is the shared body accepted by both the app endpoint and the relay.
type Submission struct {
	ID       string `json:"id"`
	App      string `json:"app"`
	Message  string `json:"message"`
	Kind     string `json:"kind,omitempty"`
	Version  string `json:"version,omitempty"`
	Context  string `json:"context,omitempty"`
	Reporter string `json:"reporter,omitempty"`
}

// NewID generates the idempotency key. The same id goes to both destinations,
// so a retry - or a later replay of a local store - is stored once, not twice.
func NewID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf)
}

// Validate checks a submission before either write.
func Validate(s Submission) error {
	if strings.TrimSpace(s.Message) == "" {
		return ErrEmpty
	}
	if len(s.Message)+len(s.Context) > MaxBytes {
		return ErrTooBig
	}
	if s.Kind != "" {
		for _, k := range Kinds() {
			if k == s.Kind {
				return nil
			}
		}
		return ErrBadKind
	}
	return nil
}

// Reporter defaults to the shell user, falling back to "agent".
func Reporter() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "agent"
}

// Relay resolves the relay base URL, or "" when it is switched off.
func Relay() string {
	relay := os.Getenv("FEEDBACK_RELAY")
	if relay == "off" {
		return ""
	}
	if relay == "" {
		return DefaultRelay
	}
	return strings.TrimSuffix(relay, "/")
}

// AppEndpoint resolves this tool's own submission endpoint.
func AppEndpoint() string {
	for _, name := range []string{"BKN_URL", "BKN_PUBLIC_URL"} {
		if v := os.Getenv(name); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	return ""
}

// Result reports which of the two writes landed.
type Result struct {
	ID      string   `json:"id"`
	Stored  int      `json:"stored"`
	Relayed int      `json:"relayed"`
	AppURL  string   `json:"app_endpoint,omitempty"`
	Relay   string   `json:"relay,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Send dual-writes the submission. Both writes are best-effort: reporting
// feedback must never be the thing that fails, or the report is lost along
// with whatever prompted it.
func Send(s Submission) Result {
	res := Result{ID: s.ID, AppURL: AppEndpoint(), Relay: Relay()}
	body, err := json.Marshal(s)
	if err != nil {
		res.Notes = append(res.Notes, "could not encode the submission")
		return res
	}

	if res.AppURL != "" {
		if err := post(res.AppURL+"/v1/feedback", body); err != nil {
			res.Notes = append(res.Notes, "app endpoint: "+err.Error())
		} else {
			res.Stored = 1
		}
	}
	if res.Relay != "" {
		if err := post(res.Relay+"/v1/feedback", body); err != nil {
			res.Notes = append(res.Notes, "relay: "+err.Error())
		} else {
			res.Relayed = 1
		}
	}
	return res
}

func post(url string, body []byte) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errors.New("HTTP " + resp.Status)
	}
	return nil
}
