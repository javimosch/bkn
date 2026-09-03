// Package cron schedules scripts.
//
// It is deliberately thin: the script primitive already runs code safely with
// a timeout and a run history, so a scheduler only has to answer "what is due"
// and call it. That is the payoff for having built the escape hatch first.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrBadSchedule = errors.New("schedule must be 5 cron fields, @every <duration>, or a @shortcut")
	ErrBadField    = errors.New("cron field is out of range")
)

// Schedule is a parsed cron expression.
type Schedule struct {
	raw     string
	every   time.Duration // set for "@every 5m"; the bitsets are unused then
	minutes uint64        // 0-59
	hours   uint64        // 0-23
	doms    uint64        // 1-31
	months  uint64        // 1-12
	dows    uint64        // 0-6, Sunday = 0
	domStar bool
	dowStar bool
}

func (s Schedule) String() string { return s.raw }

var shortcuts = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Shortcuts lists the recognised @names.
func Shortcuts() []string {
	return []string{"@hourly", "@daily", "@weekly", "@monthly", "@yearly", "@every <duration>"}
}

// Parse reads a schedule expression.
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Schedule{}, ErrBadSchedule
	}
	original := spec

	if rest, ok := strings.CutPrefix(spec, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return Schedule{}, fmt.Errorf("%w: %v", ErrBadSchedule, err)
		}
		if d < time.Second {
			return Schedule{}, fmt.Errorf("%w: interval must be at least 1s", ErrBadSchedule)
		}
		return Schedule{raw: original, every: d}, nil
	}
	if expanded, ok := shortcuts[strings.ToLower(spec)]; ok {
		spec = expanded
	}

	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("%w: got %d fields in %q", ErrBadSchedule, len(fields), original)
	}
	s := Schedule{raw: original}
	var err error
	if s.minutes, err = parseField(fields[0], 0, 59); err != nil {
		return Schedule{}, fmt.Errorf("minute: %w", err)
	}
	if s.hours, err = parseField(fields[1], 0, 23); err != nil {
		return Schedule{}, fmt.Errorf("hour: %w", err)
	}
	if s.doms, err = parseField(fields[2], 1, 31); err != nil {
		return Schedule{}, fmt.Errorf("day of month: %w", err)
	}
	if s.months, err = parseField(fields[3], 1, 12); err != nil {
		return Schedule{}, fmt.Errorf("month: %w", err)
	}
	if s.dows, err = parseDOW(fields[4]); err != nil {
		return Schedule{}, fmt.Errorf("day of week: %w", err)
	}
	s.domStar = fields[2] == "*"
	s.dowStar = fields[4] == "*"
	return s, nil
}

// parseField reads one field: "*", "*/n", "a", "a-b", "a-b/n", or a comma list.
func parseField(field string, min, max int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		step := 1
		if base, stepStr, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%w: bad step in %q", ErrBadField, part)
			}
			step, part = n, base
		}

		lo, hi := min, max
		if part != "*" {
			from, to, isRange := strings.Cut(part, "-")
			n, err := strconv.Atoi(strings.TrimSpace(from))
			if err != nil {
				return 0, fmt.Errorf("%w: %q is not a number", ErrBadField, from)
			}
			lo = n
			hi = n
			if isRange {
				m, err := strconv.Atoi(strings.TrimSpace(to))
				if err != nil {
					return 0, fmt.Errorf("%w: %q is not a number", ErrBadField, to)
				}
				hi = m
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, fmt.Errorf("%w: %q is outside %d-%d", ErrBadField, part, min, max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	if bits == 0 {
		return 0, fmt.Errorf("%w: %q matches nothing", ErrBadField, field)
	}
	return bits, nil
}

// parseDOW accepts 0-7 where both 0 and 7 mean Sunday, as every other cron does.
func parseDOW(field string) (uint64, error) {
	bits, err := parseField(field, 0, 7)
	if err != nil {
		return 0, err
	}
	if bits&(1<<7) != 0 {
		bits = (bits &^ (1 << 7)) | 1
	}
	return bits, nil
}

func (s Schedule) matches(t time.Time) bool {
	if s.minutes&(1<<uint(t.Minute())) == 0 ||
		s.hours&(1<<uint(t.Hour())) == 0 ||
		s.months&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domHit := s.doms&(1<<uint(t.Day())) != 0
	dowHit := s.dows&(1<<uint(int(t.Weekday()))) != 0

	// Vixie cron semantics: when BOTH day fields are restricted, either one
	// matching is enough. "0 0 1 * 1" is the 1st of the month OR any Monday,
	// not the 1st when it happens to be a Monday. It surprises everyone once.
	if !s.domStar && !s.dowStar {
		return domHit || dowHit
	}
	return domHit && dowHit
}

// Next returns the first firing time strictly after t, or the zero time if the
// schedule cannot fire within four years (e.g. "0 0 30 2 *").
func (s Schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Add(s.every).Truncate(time.Second)
	}
	// Start at the next whole minute; cron has minute resolution.
	next := t.Truncate(time.Minute).Add(time.Minute)
	limit := next.AddDate(4, 0, 0)
	for next.Before(limit) {
		if s.matches(next) {
			return next
		}
		next = next.Add(time.Minute)
	}
	return time.Time{}
}
