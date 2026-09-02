// Package out implements the cli-output-spec v1.0 output contract:
// stdout = data only, stderr = context only, semantic exit codes, typed errors.
package out

import (
	"encoding/json"
	"fmt"
	"os"
)

// SchemaVersion is the stdout contract version, not the tool version.
// Bump only on a breaking change to emitted JSON shapes.
const SchemaVersion = "1.0"

// Exit codes per cli-output-spec §2.
const (
	OK               = 0
	InvalidArguments = 80
	ValidationError  = 82
	InvalidValue     = 85
	NotAuthenticated = 90
	NotFound         = 92
	Conflict         = 95
	ConnectionError  = 100
	ExternalError    = 105
	InternalError    = 110
)

// Error is the typed error body per cli-output-spec §3.
type Error struct {
	Code        int      `json:"code"`
	Type        string   `json:"type"`
	Message     string   `json:"message"`
	Recoverable bool     `json:"recoverable"`
	Suggestions []string `json:"suggestions,omitempty"`
}

// Log writes context to stderr. Never data.
func Log(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Data writes a success envelope to stdout: {"ok":true,"version":"1.0",...fields}.
// Callers pass result fields; ok and version are merged in here so every
// command emits the same envelope shape.
func Data(fields map[string]any) {
	env := map[string]any{"ok": true, "version": SchemaVersion}
	for k, v := range fields {
		env[k] = v
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(env)
}

// Raw writes an already-shaped object to stdout (help-json, guide) which carry
// their own top-level shape defined by their own specs.
func Raw(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Fail writes a typed error to stderr and exits with the matching code.
// Recoverable is derived from the code range: only 100-109 are transient.
// The tool never retries internally (cli-output-spec §3) - the agent decides.
func Fail(code int, typ, msg string, suggestions ...string) {
	e := Error{
		Code:        code,
		Type:        typ,
		Message:     msg,
		Recoverable: code >= 100 && code < 110,
		Suggestions: suggestions,
	}
	b, _ := json.Marshal(map[string]any{"ok": false, "error": e})
	fmt.Fprintf(os.Stderr, "%s\n", b)
	os.Exit(code)
}
