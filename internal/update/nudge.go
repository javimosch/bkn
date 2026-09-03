package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// nudgeInterval throttles the passive check. Once an hour is often enough to
// notice a release and rare enough not to be a background job.
const nudgeInterval = time.Hour

type nudgeState struct {
	CheckedAt string `json:"checked_at"`
	Latest    string `json:"latest"`
}

func nudgePath() string { return filepath.Join(Home(), "update-check.json") }

// Nudge prints a one-line notice on stderr when the server has something
// newer. It never updates anything and never fails a command: an unreachable
// server means the caller carries on.
func Nudge() {
	if os.Getenv("BKN_NO_NUDGE") != "" {
		return
	}
	state := readNudgeState()
	if state.CheckedAt != "" {
		if at, err := time.Parse(time.RFC3339, state.CheckedAt); err == nil &&
			time.Since(at) < nudgeInterval {
			return
		}
	}

	rel, err := Fetch(3 * time.Second)
	if err != nil {
		// Record the attempt anyway, so an offline machine does not retry on
		// every single command.
		writeNudgeState(nudgeState{CheckedAt: time.Now().UTC().Format(time.RFC3339)})
		return
	}
	writeNudgeState(nudgeState{
		CheckedAt: time.Now().UTC().Format(time.RFC3339), Latest: rel.Version,
	})

	local, err := LocalVersion()
	if err != nil || local == rel.Version {
		return
	}
	fmt.Fprintf(os.Stderr,
		"[update] a newer bkn is available (%s -> %s). Run: bkn update\n", local, rel.Version)
}

func readNudgeState() nudgeState {
	var state nudgeState
	raw, err := os.ReadFile(nudgePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(raw, &state)
	return state
}

func writeNudgeState(state nudgeState) {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.WriteFile(nudgePath(), raw, 0o600)
}
