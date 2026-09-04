package cron

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextFiringTimes(t *testing.T) {
	cases := []struct {
		spec, from, want string
	}{
		{"* * * * *", "2026-03-05T10:00:30Z", "2026-03-05T10:01:00Z"},
		{"0 * * * *", "2026-03-05T10:15:00Z", "2026-03-05T11:00:00Z"},
		{"@hourly", "2026-03-05T10:15:00Z", "2026-03-05T11:00:00Z"},
		{"0 3 * * *", "2026-03-05T10:00:00Z", "2026-03-06T03:00:00Z"},
		{"@daily", "2026-03-05T10:00:00Z", "2026-03-06T00:00:00Z"},
		{"*/15 * * * *", "2026-03-05T10:01:00Z", "2026-03-05T10:15:00Z"},
		{"30 2 1 * *", "2026-03-05T10:00:00Z", "2026-04-01T02:30:00Z"},
		{"0 0 * * 1", "2026-03-05T10:00:00Z", "2026-03-09T00:00:00Z"},  // next Monday
		{"@weekly", "2026-03-05T10:00:00Z", "2026-03-08T00:00:00Z"},    // next Sunday
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"}, // leap day
		{"5,10 * * * *", "2026-03-05T10:06:00Z", "2026-03-05T10:10:00Z"},
		{"0 9-17 * * *", "2026-03-05T18:30:00Z", "2026-03-06T09:00:00Z"},
	}
	for _, tc := range cases {
		s, err := Parse(tc.spec)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.spec, err)
			continue
		}
		got := s.Next(at(tc.from))
		if !got.Equal(at(tc.want)) {
			t.Errorf("Parse(%q).Next(%s) = %s, want %s", tc.spec, tc.from, got.Format(time.RFC3339), tc.want)
		}
	}
}

// Vixie cron semantics: when BOTH day fields are restricted, either matching
// is enough. It surprises everyone exactly once, so it gets a test.
func TestBothDayFieldsRestrictedMeansOr(t *testing.T) {
	s, err := Parse("0 0 1 * 1") // the 1st, OR any Monday
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 2026-03-02 is a Monday; the 1st is a Sunday.
	if got := s.Next(at("2026-02-28T12:00:00Z")); !got.Equal(at("2026-03-01T00:00:00Z")) {
		t.Errorf("next = %s, want the 1st (day-of-month match)", got.Format(time.RFC3339))
	}
	if got := s.Next(at("2026-03-01T12:00:00Z")); !got.Equal(at("2026-03-02T00:00:00Z")) {
		t.Errorf("next = %s, want the Monday (day-of-week match)", got.Format(time.RFC3339))
	}

	// With only one of them restricted, it is a plain AND.
	only, _ := Parse("0 0 1 * *")
	if got := only.Next(at("2026-03-02T12:00:00Z")); !got.Equal(at("2026-04-01T00:00:00Z")) {
		t.Errorf("next = %s, want the 1st of April", got.Format(time.RFC3339))
	}
}

func TestEveryDuration(t *testing.T) {
	s, err := Parse("@every 90s")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	from := at("2026-03-05T10:00:00Z")
	if got := s.Next(from); !got.Equal(from.Add(90 * time.Second)) {
		t.Errorf("next = %s", got.Format(time.RFC3339))
	}
	if _, err := Parse("@every 100ms"); err == nil {
		t.Error("a sub-second interval was accepted")
	}
	if _, err := Parse("@every nonsense"); err == nil {
		t.Error("an unparseable duration was accepted")
	}
}

// Sunday is 0 in most crontabs and 7 in some; both must work.
func TestSundayIsZeroAndSeven(t *testing.T) {
	zero, err := Parse("0 0 * * 0")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	seven, err := Parse("0 0 * * 7")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	from := at("2026-03-05T10:00:00Z")
	if !zero.Next(from).Equal(seven.Next(from)) {
		t.Errorf("0 and 7 disagree: %s vs %s", zero.Next(from), seven.Next(from))
	}
}

func TestParseRejectsNonsense(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "@nope", "@every",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted an invalid schedule", bad)
		}
	}
}

// A schedule that can never fire must return the zero time rather than looping.
func TestImpossibleScheduleTerminates(t *testing.T) {
	s, err := Parse("0 0 30 2 *") // February 30th
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	done := make(chan time.Time, 1)
	go func() { done <- s.Next(at("2026-03-05T10:00:00Z")) }()
	select {
	case got := <-done:
		if !got.IsZero() {
			t.Errorf("next = %s, want the zero time", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not terminate")
	}
}

func TestScheduleRoundTripsItsText(t *testing.T) {
	for _, spec := range []string{"0 3 * * *", "@daily", "@every 5m"} {
		s, err := Parse(spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", spec, err)
		}
		if s.String() != spec {
			t.Errorf("String() = %q, want the original %q", s.String(), spec)
		}
	}
}

// An `@every` job must never be scheduled sooner than its interval. Stored
// times have second resolution, so rounding the next run down put it inside
// the current second: at :01.999 an `@every 1s` job was scheduled 1ms later
// and then re-fired for the rest of that second.
func TestEveryNeverSchedulesEarly(t *testing.T) {
	for _, spec := range []string{"@every 1s", "@every 5s", "@every 1m"} {
		s, err := Parse(spec)
		if err != nil {
			t.Fatalf("Parse(%q): %v", spec, err)
		}
		for _, ms := range []int{0, 1, 100, 500, 900, 999} {
			when := time.Date(2026, 1, 1, 10, 0, 1, ms*1e6, time.UTC)
			next := s.Next(when)
			if gap := next.Sub(when); gap < s.every {
				t.Errorf("%s at .%03d: next is %v away, want at least %v", spec, ms, gap, s.every)
			}
			if !next.Equal(next.Truncate(time.Second)) {
				t.Errorf("%s at .%03d: next %v is not second-aligned, so stamp() rounds it into the past", spec, ms, next)
			}
		}
	}
}
