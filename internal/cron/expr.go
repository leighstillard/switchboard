// Package cron implements a minimal cron scheduler that dispatches prompts
// to jcode agent sessions on configurable schedules.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Expr is a parsed 5-field cron expression (minute hour dom month dow).
type Expr struct {
	minute [60]bool
	hour   [24]bool
	dom    [32]bool // 1-31
	month  [13]bool // 1-12
	dow    [7]bool  // 0-6 (0 = Sunday)
}

// Parse parses a standard 5-field cron expression.
//
// Supported syntax per field:
//   - Single value: "5"
//   - Wildcard: "*"
//   - Range: "1-5"
//   - Step: "*/15", "1-5/2"
//   - List: "1,3,5"
//
// Day-of-week accepts 0-7 where both 0 and 7 map to Sunday.
func Parse(spec string) (*Expr, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}

	e := &Expr{}

	if err := parseField(fields[0], e.minute[:], 0, 59); err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	if err := parseField(fields[1], e.hour[:], 0, 23); err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	if err := parseField(fields[2], e.dom[:], 1, 31); err != nil {
		return nil, fmt.Errorf("cron: day-of-month: %w", err)
	}
	if err := parseField(fields[3], e.month[:], 1, 12); err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	if err := parseDOW(fields[4], e.dow[:]); err != nil {
		return nil, fmt.Errorf("cron: day-of-week: %w", err)
	}

	return e, nil
}

// Matches reports whether t falls within the cron expression's schedule.
// The time is evaluated at minute granularity (seconds are ignored).
func (e *Expr) Matches(t time.Time) bool {
	return e.minute[t.Minute()] &&
		e.hour[t.Hour()] &&
		e.dom[t.Day()] &&
		e.month[int(t.Month())] &&
		e.dow[int(t.Weekday())]
}

// ---------------------------------------------------------------------------
// Field parsing
// ---------------------------------------------------------------------------

// parseField parses a single cron field (e.g. "*/15", "1-5/2", "1,3,5")
// and sets the corresponding bits in the boolean slice.
func parseField(field string, bits []bool, min, max int) error {
	for _, part := range strings.Split(field, ",") {
		if err := parsePart(part, bits, min, max); err != nil {
			return err
		}
	}
	return nil
}

// parsePart handles a single element of a comma-separated list.
func parsePart(part string, bits []bool, min, max int) error {
	// Check for step: "*/2" or "1-5/2".
	step := 1
	if idx := strings.IndexByte(part, '/'); idx >= 0 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step %q", part[idx+1:])
		}
		step = s
		part = part[:idx]
	}

	// Wildcard.
	if part == "*" {
		for i := min; i <= max; i += step {
			bits[i] = true
		}
		return nil
	}

	// Range: "1-5".
	if idx := strings.IndexByte(part, '-'); idx >= 0 {
		lo, err := strconv.Atoi(part[:idx])
		if err != nil {
			return fmt.Errorf("invalid range start %q", part[:idx])
		}
		hi, err := strconv.Atoi(part[idx+1:])
		if err != nil {
			return fmt.Errorf("invalid range end %q", part[idx+1:])
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("range %d-%d out of bounds [%d,%d]", lo, hi, min, max)
		}
		for i := lo; i <= hi; i += step {
			bits[i] = true
		}
		return nil
	}

	// Single value.
	v, err := strconv.Atoi(part)
	if err != nil {
		return fmt.Errorf("invalid value %q", part)
	}
	if v < min || v > max {
		return fmt.Errorf("value %d out of bounds [%d,%d]", v, min, max)
	}
	bits[v] = true
	return nil
}

// parseDOW parses a day-of-week field, normalising 7 to 0 (both mean Sunday).
func parseDOW(field string, bits []bool) error {
	for _, part := range strings.Split(field, ",") {
		if err := parseDOWPart(part, bits); err != nil {
			return err
		}
	}
	return nil
}

func parseDOWPart(part string, bits []bool) error {
	step := 1
	if idx := strings.IndexByte(part, '/'); idx >= 0 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step %q", part[idx+1:])
		}
		step = s
		part = part[:idx]
	}

	if part == "*" {
		for i := 0; i <= 6; i += step {
			bits[i] = true
		}
		return nil
	}

	if idx := strings.IndexByte(part, '-'); idx >= 0 {
		lo, err := strconv.Atoi(part[:idx])
		if err != nil {
			return fmt.Errorf("invalid range start %q", part[:idx])
		}
		hi, err := strconv.Atoi(part[idx+1:])
		if err != nil {
			return fmt.Errorf("invalid range end %q", part[idx+1:])
		}
		lo = normDOW(lo)
		hi = normDOW(hi)
		if lo > hi {
			return fmt.Errorf("range %d-%d out of bounds [0,6]", lo, hi)
		}
		for i := lo; i <= hi; i += step {
			bits[i] = true
		}
		return nil
	}

	v, err := strconv.Atoi(part)
	if err != nil {
		return fmt.Errorf("invalid value %q", part)
	}
	if v < 0 || v > 7 {
		return fmt.Errorf("value %d out of bounds [0,7]", v)
	}
	bits[normDOW(v)] = true
	return nil
}

// normDOW normalises day-of-week so 7 maps to 0 (Sunday).
func normDOW(d int) int {
	if d == 7 {
		return 0
	}
	return d
}
