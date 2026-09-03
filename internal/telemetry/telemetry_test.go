package telemetry_test

import (
	"encoding/json"
	"testing"

	"github.com/javimosch/bkn/internal/telemetry"
)

func newReporter(t *testing.T) *telemetry.Reporter {
	t.Helper()
	// Every switch the spec names, cleared, so a test does not inherit the
	// developer's own environment.
	for _, name := range []string{
		"DO_NOT_TRACK", "BKN_TELEMETRY", "BKN_TELEMETRY_URL",
		"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI",
		"BUILDKITE", "CIRCLECI", "JENKINS_URL", "TEAMCITY_VERSION",
	} {
		t.Setenv(name, "")
	}
	return telemetry.New(t.TempDir(), "1.2.3")
}

// bkn holds encrypted secrets, password hashes and token signing keys, so it
// takes the spec's stricter option: nothing is sent unless someone opts in.
func TestDefaultsToDisabled(t *testing.T) {
	r := newReporter(t)
	enabled, reason := r.Decision()
	if enabled {
		t.Error("telemetry was enabled without anyone asking for it")
	}
	if reason == "" {
		t.Error("a refusal must say why")
	}
}

func TestEveryOffSwitchIsHonoured(t *testing.T) {
	cases := map[string]struct{ name, value string }{
		"DO_NOT_TRACK":      {"DO_NOT_TRACK", "1"},
		"tool switch 0":     {"BKN_TELEMETRY", "0"},
		"tool switch off":   {"BKN_TELEMETRY", "off"},
		"tool switch false": {"BKN_TELEMETRY", "false"},
		"tool switch no":    {"BKN_TELEMETRY", "no"},
		"CI":                {"CI", "true"},
		"GitHub Actions":    {"GITHUB_ACTIONS", "true"},
		"GitLab CI":         {"GITLAB_CI", "true"},
		"Buildkite":         {"BUILDKITE", "true"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newReporter(t)
			// Enabled first, so the switch is what turns it off.
			if err := r.SetEnabled(true); err != nil {
				t.Fatalf("SetEnabled: %v", err)
			}
			if enabled, _ := r.Decision(); !enabled {
				t.Fatal("could not enable telemetry to begin with")
			}
			t.Setenv(tc.name, tc.value)
			if enabled, reason := r.Decision(); enabled {
				t.Errorf("%s=%s did not disable telemetry (%s)", tc.name, tc.value, reason)
			}
		})
	}
}

func TestOptOutPersists(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"DO_NOT_TRACK", "BKN_TELEMETRY", "CI"} {
		t.Setenv(name, "")
	}
	first := telemetry.New(dir, "1.2.3")
	if err := first.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if enabled, _ := telemetry.New(dir, "1.2.3").Decision(); !enabled {
		t.Error("the choice did not survive a new process")
	}
	if err := first.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if enabled, _ := telemetry.New(dir, "1.2.3").Decision(); enabled {
		t.Error("opting out did not survive a new process")
	}
}

// The payload is an allow-list. A field not in the spec must not appear, and a
// deny-list would leak by default.
func TestPayloadIsTheAllowList(t *testing.T) {
	r := newReporter(t)
	raw, err := json.Marshal(r.NextPayload("store list", 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	allowed := map[string]bool{
		"tool": true, "version": true, "event": true, "verb": true,
		"os": true, "arch": true, "exit_class": true, "ts": true,
		"install_id": true,
	}
	for field := range fields {
		if !allowed[field] {
			t.Errorf("payload carries %q, which is not in the allow-list", field)
		}
	}
	for _, required := range []string{"tool", "version", "event", "verb", "os", "arch", "exit_class", "ts"} {
		if _, ok := fields[required]; !ok {
			t.Errorf("payload is missing %q", required)
		}
	}
	// install_id is deliberately omitted; the spec calls that the safer default.
	if _, present := fields["install_id"]; present {
		t.Error("install_id should be omitted entirely, not sent")
	}
}

func TestEventsAndExitClasses(t *testing.T) {
	r := newReporter(t)

	// The first event on a machine is install; it answers "did anyone get it
	// running", which is the question most tools cannot answer at all.
	if got := r.NextPayload("version", 0).Event; got != telemetry.EventInstall {
		t.Errorf("first event = %q, want install", got)
	}
	if got := r.NextPayload("store put", 92).Event; got != telemetry.EventError {
		t.Errorf("failing verb = %q, want error", got)
	}

	for code, want := range map[int]int{
		0: 0, 80: 80, 82: 80, 85: 80, 90: 90, 92: 90, 95: 90,
		100: 100, 105: 100, 110: 110, 119: 110, 1: 110,
	} {
		if got := telemetry.ExitClass(code); got != want {
			t.Errorf("ExitClass(%d) = %d, want %d", code, got, want)
		}
	}
}

// A verb is the subcommand name, never free text and never arguments.
func TestVerbCarriesNoArguments(t *testing.T) {
	r := newReporter(t)
	p := r.NextPayload("kv set", 0)
	if p.Verb != "kv set" {
		t.Errorf("verb = %q", p.Verb)
	}
	raw, _ := json.Marshal(p)
	for _, leak := range []string{"secret", "/home/", "--password", "@"} {
		if contains(string(raw), leak) {
			t.Errorf("payload %s appears to leak %q", raw, leak)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func TestStatusExplainsItself(t *testing.T) {
	r := newReporter(t)
	status := r.Status("telemetry")
	for _, key := range []string{
		"enabled", "reason", "endpoint", "next_payload", "disable", "notice_shown", "notice",
	} {
		if _, ok := status[key]; !ok {
			t.Errorf("status is missing %q", key)
		}
	}
	// The endpoint must be visible and overridable, or a reviewer has no way
	// to see what the tool sends without letting it send.
	t.Setenv("BKN_TELEMETRY_URL", "http://localhost:9999/collect")
	if got := telemetry.Endpoint(); got != "http://localhost:9999/collect" {
		t.Errorf("endpoint override ignored: %q", got)
	}
}
