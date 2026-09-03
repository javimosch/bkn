package feedback_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/javimosch/bkn/internal/feedback"
)

func collector(t *testing.T, status int) (*httptest.Server, *[]feedback.Submission) {
	t.Helper()
	var mu sync.Mutex
	got := []feedback.Submission{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sub feedback.Submission
		_ = json.Unmarshal(raw, &sub)
		mu.Lock()
		got = append(got, sub)
		mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestDualWriteCarriesTheSameID(t *testing.T) {
	app, appGot := collector(t, http.StatusOK)
	relay, relayGot := collector(t, http.StatusOK)
	t.Setenv("BKN_URL", app.URL)
	t.Setenv("FEEDBACK_RELAY", relay.URL)

	sub := feedback.Submission{
		ID: feedback.NewID(), App: "bkn", Message: "it works",
		Kind: "praise", Version: "1.0.0", Reporter: "agent",
	}
	res := feedback.Send(sub)

	if res.Stored != 1 || res.Relayed != 1 {
		t.Fatalf("stored=%d relayed=%d, want both 1 (%v)", res.Stored, res.Relayed, res.Notes)
	}
	if len(*appGot) != 1 || len(*relayGot) != 1 {
		t.Fatalf("app got %d, relay got %d", len(*appGot), len(*relayGot))
	}
	// The same id on both writes is what makes delivery idempotent: a retry,
	// or a later replay of a local store, never double-counts.
	if (*appGot)[0].ID != sub.ID || (*relayGot)[0].ID != sub.ID {
		t.Errorf("ids differ: app %q relay %q want %q",
			(*appGot)[0].ID, (*relayGot)[0].ID, sub.ID)
	}
	if (*relayGot)[0].App != "bkn" {
		t.Error("the relay must be told which app this came from")
	}
}

// Reporting feedback must never be the thing that fails, or the report is lost
// along with whatever prompted it.
func TestFailedWritesAreReportedNotRaised(t *testing.T) {
	t.Setenv("BKN_URL", "http://127.0.0.1:9")
	t.Setenv("FEEDBACK_RELAY", "http://127.0.0.1:9")

	res := feedback.Send(feedback.Submission{
		ID: feedback.NewID(), App: "bkn", Message: "into the void",
	})
	if res.Stored != 0 || res.Relayed != 0 {
		t.Errorf("unreachable collectors reported success: %+v", res)
	}
	if len(res.Notes) != 2 {
		t.Errorf("notes = %v, want one per failed destination", res.Notes)
	}
}

func TestRelayCanBeSwitchedOff(t *testing.T) {
	app, appGot := collector(t, http.StatusOK)
	t.Setenv("BKN_URL", app.URL)
	t.Setenv("FEEDBACK_RELAY", "off")

	if got := feedback.Relay(); got != "" {
		t.Errorf("Relay() = %q, want empty when off", got)
	}
	res := feedback.Send(feedback.Submission{ID: "x", App: "bkn", Message: "local only"})
	if res.Stored != 1 || res.Relayed != 0 {
		t.Errorf("stored=%d relayed=%d, want 1 and 0", res.Stored, res.Relayed)
	}
	if len(*appGot) != 1 {
		t.Errorf("the app endpoint should still have received it")
	}
}

// A tool with no local endpoint is relay-only and still conforms.
func TestRelayOnlyIsValid(t *testing.T) {
	relay, relayGot := collector(t, http.StatusOK)
	t.Setenv("BKN_URL", "")
	t.Setenv("BKN_PUBLIC_URL", "")
	t.Setenv("FEEDBACK_RELAY", relay.URL)

	res := feedback.Send(feedback.Submission{ID: "y", App: "bkn", Message: "relay only"})
	if res.Stored != 0 || res.Relayed != 1 {
		t.Errorf("stored=%d relayed=%d, want 0 and 1", res.Stored, res.Relayed)
	}
	if len(*relayGot) != 1 {
		t.Error("the relay did not receive the submission")
	}
}

func TestValidation(t *testing.T) {
	if err := feedback.Validate(feedback.Submission{Message: "  \n "}); err != feedback.ErrEmpty {
		t.Errorf("whitespace-only message = %v, want ErrEmpty", err)
	}
	if err := feedback.Validate(feedback.Submission{
		Message: strings.Repeat("x", feedback.MaxBytes+1),
	}); err != feedback.ErrTooBig {
		t.Errorf("oversized message = %v, want ErrTooBig", err)
	}
	if err := feedback.Validate(feedback.Submission{Message: "hi", Kind: "shouting"}); err != feedback.ErrBadKind {
		t.Errorf("unknown kind = %v, want ErrBadKind", err)
	}
	for _, kind := range append(feedback.Kinds(), "") {
		if err := feedback.Validate(feedback.Submission{Message: "hi", Kind: kind}); err != nil {
			t.Errorf("kind %q rejected: %v", kind, err)
		}
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := feedback.NewID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		if len(id) != 32 {
			t.Fatalf("id %q is %d chars, want 32 hex", id, len(id))
		}
		seen[id] = true
	}
}
