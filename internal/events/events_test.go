package events_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/events"
)

func newLog(t *testing.T) *events.Log {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return events.New(conn)
}

func emit(t *testing.T, l *events.Log, e events.Event) events.Event {
	t.Helper()
	out, err := l.Emit(e)
	if err != nil {
		t.Fatalf("Emit(%+v): %v", e, err)
	}
	return out
}

func TestEmitValidatesAndDefaults(t *testing.T) {
	l := newLog(t)

	e := emit(t, l, events.Event{Stream: "errors", Type: "http.500"})
	if e.Level != events.LevelInfo {
		t.Errorf("default level = %q, want info", e.Level)
	}
	if e.ID == "" || e.CreatedAt == "" {
		t.Errorf("id/created_at not populated: %+v", e)
	}

	if _, err := l.Emit(events.Event{Stream: "errors"}); err == nil {
		t.Error("an event with no type was accepted")
	}
	for _, bad := range []string{"", "Errors", "9x", "has space", strings.Repeat("x", 64)} {
		if _, err := l.Emit(events.Event{Stream: bad, Type: "x"}); !errors.Is(err, events.ErrBadStream) {
			t.Errorf("Emit into stream %q = %v, want ErrBadStream", bad, err)
		}
	}
	if _, err := l.Emit(events.Event{Stream: "errors", Type: "x", Level: "shouting"}); !errors.Is(err, events.ErrBadLevel) {
		t.Error("an invalid level was accepted")
	}
}

func TestFiltering(t *testing.T) {
	l := newLog(t)
	emit(t, l, events.Event{Stream: "api", Type: "http.500", Level: events.LevelError, Source: "web", Subject: "/orders"})
	emit(t, l, events.Event{Stream: "api", Type: "http.500", Level: events.LevelError, Source: "worker", Subject: "/orders"})
	emit(t, l, events.Event{Stream: "api", Type: "http.404", Level: events.LevelWarn, Source: "web", Subject: "/favicon"})
	emit(t, l, events.Event{Stream: "audit", Type: "setting.changed", Source: "cli"})

	cases := []struct {
		name  string
		query events.Query
		want  int
	}{
		{"all in stream", events.Query{}, 3},
		{"by type", events.Query{Type: "http.500"}, 2},
		{"by level", events.Query{Level: events.LevelError}, 2},
		{"by source", events.Query{Source: "web"}, 2},
		{"by subject", events.Query{Subject: "/orders"}, 2},
		{"combined", events.Query{Type: "http.500", Source: "web"}, 1},
		{"no match", events.Query{Type: "nope"}, 0},
		{"limit", events.Query{Limit: 1}, 1},
		{"offset past the end", events.Query{Offset: 99}, 0},
	}
	for _, tc := range cases {
		got, err := l.List("api", tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d events, want %d", tc.name, len(got), tc.want)
		}
	}

	if _, err := l.List("api", events.Query{Level: "shouting"}); !errors.Is(err, events.ErrBadLevel) {
		t.Error("an invalid level filter was accepted")
	}
}

// Newest first, and stable: ULID ids break created_at ties in creation order.
func TestListIsNewestFirst(t *testing.T) {
	l := newLog(t)
	for _, typ := range []string{"first", "second", "third"} {
		emit(t, l, events.Event{Stream: "api", Type: typ})
	}
	got, err := l.List("api", events.Query{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"third", "second", "first"}
	for i, e := range got {
		if e.Type != want[i] {
			t.Errorf("position %d = %q, want %q", i, e.Type, want[i])
		}
	}
}

func TestParseWhenAcceptsRelativeAndAbsolute(t *testing.T) {
	for _, spec := range []string{"30s", "15m", "24h", "7d", "2026-03-05T10:00:00Z"} {
		if _, err := events.ParseWhen(spec); err != nil {
			t.Errorf("ParseWhen(%q): %v", spec, err)
		}
	}
	for _, bad := range []string{"bananas", "7", "d7", "-1h", "7 days"} {
		if _, err := events.ParseWhen(bad); !errors.Is(err, events.ErrBadDuration) {
			t.Errorf("ParseWhen(%q) = %v, want ErrBadDuration", bad, err)
		}
	}
	got, err := events.ParseWhen("2h")
	if err != nil {
		t.Fatalf("ParseWhen: %v", err)
	}
	if delta := time.Since(got); delta < 119*time.Minute || delta > 121*time.Minute {
		t.Errorf("2h resolved to %v ago", delta)
	}
}

func TestStatsGroupsAndRejectsUnknownColumns(t *testing.T) {
	l := newLog(t)
	emit(t, l, events.Event{Stream: "api", Type: "a", Level: events.LevelError})
	emit(t, l, events.Event{Stream: "api", Type: "a", Level: events.LevelError})
	emit(t, l, events.Event{Stream: "api", Type: "b", Level: events.LevelInfo})

	buckets, err := l.Stats("api", "type", events.Query{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(buckets) != 2 || buckets[0].Key != "a" || buckets[0].Count != 2 {
		t.Errorf("buckets = %+v, want a=2 first", buckets)
	}

	// The group column cannot be a bound parameter, so it must come from a
	// fixed set rather than from the caller.
	for _, bad := range []string{"id", "data", "ns", "1; DROP TABLE events", ""} {
		if _, err := l.Stats("api", bad, events.Query{}); !errors.Is(err, events.ErrBadGroupBy) {
			t.Errorf("Stats(by=%q) = %v, want ErrBadGroupBy", bad, err)
		}
	}
}

func TestPruneRemovesOldEventsOnly(t *testing.T) {
	l := newLog(t)
	emit(t, l, events.Event{Stream: "api", Type: "recent"})
	emit(t, l, events.Event{Stream: "audit", Type: "recent"})

	n, err := l.Prune("", "30d")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d recent events", n)
	}

	// Timestamps have second resolution, so the sleep must exceed the age by
	// a whole second: sleeping only 1.1s for a 1s cutoff leaves a margin that
	// rounds to zero whenever the events land late in their second, which
	// made this flaky.
	time.Sleep(2100 * time.Millisecond)
	n, err = l.Prune("api", "1s")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1 from the api stream only", n)
	}
	rest, _ := l.List("audit", events.Query{})
	if len(rest) != 1 {
		t.Errorf("pruning one stream touched another: %d left", len(rest))
	}

	if _, err := l.Prune("", ""); !errors.Is(err, events.ErrBadDuration) {
		t.Error("Prune with no age was accepted")
	}
}

func TestStreamsSummary(t *testing.T) {
	l := newLog(t)
	emit(t, l, events.Event{Stream: "api", Type: "a", Level: events.LevelError})
	emit(t, l, events.Event{Stream: "api", Type: "b", Level: events.LevelInfo})
	emit(t, l, events.Event{Stream: "audit", Type: "c"})

	got, err := l.Streams()
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("streams = %d, want 2", len(got))
	}
	for _, s := range got {
		if s["stream"] == "api" && (s["count"] != 2 || s["errors"] != 1) {
			t.Errorf("api summary = %+v, want count 2 / errors 1", s)
		}
	}
}
