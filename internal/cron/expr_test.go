package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tm(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		spec string
		want string
	}{
		{"", "expected 5 fields"},
		{"* * *", "expected 5 fields"},
		{"60 * * * *", "out of bounds"},
		{"* 24 * * *", "out of bounds"},
		{"* * 0 * *", "out of bounds"},
		{"* * 32 * *", "out of bounds"},
		{"* * * 0 *", "out of bounds"},
		{"* * * 13 *", "out of bounds"},
		{"* * * * 8", "out of bounds"},
		{"* * * * -1", "invalid range start"},
		{"abc * * * *", "invalid value"},
		{"*/0 * * * *", "invalid step"},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			_, err := Parse(tt.spec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestWildcard(t *testing.T) {
	e, err := Parse("* * * * *")
	require.NoError(t, err)

	// Should match any time.
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 0)))
	assert.True(t, e.Matches(tm(2026, 6, 15, 12, 30)))
	assert.True(t, e.Matches(tm(2026, 12, 31, 23, 59)))
}

func TestSingleValues(t *testing.T) {
	e, err := Parse("30 14 1 6 3")
	require.NoError(t, err)

	// 2026-06-03 is a Wednesday (dow=3), but day-of-month is 1 so no match.
	assert.False(t, e.Matches(tm(2026, 6, 3, 14, 30)))

	// Wednesday June 1, 2033 at 14:30 (2033-06-01 is a Wednesday).
	assert.True(t, e.Matches(tm(2033, 6, 1, 14, 30)))

	// Wrong minute.
	assert.False(t, e.Matches(tm(2033, 6, 1, 14, 31)))
}

func TestRanges(t *testing.T) {
	e, err := Parse("0 9-17 * * *")
	require.NoError(t, err)

	assert.True(t, e.Matches(tm(2026, 1, 1, 9, 0)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 17, 0)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 8, 0)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 18, 0)))
}

func TestSteps(t *testing.T) {
	e, err := Parse("*/15 * * * *")
	require.NoError(t, err)

	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 0)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 15)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 30)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 45)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 0, 10)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 0, 1)))
}

func TestRangeWithStep(t *testing.T) {
	e, err := Parse("0 1-5/2 * * *")
	require.NoError(t, err)

	// Should match hours 1, 3, 5.
	assert.True(t, e.Matches(tm(2026, 1, 1, 1, 0)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 2, 0)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 3, 0)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 4, 0)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 5, 0)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 6, 0)))
}

func TestLists(t *testing.T) {
	e, err := Parse("0,15,30,45 * * * *")
	require.NoError(t, err)

	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 0)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 15)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 30)))
	assert.True(t, e.Matches(tm(2026, 1, 1, 0, 45)))
	assert.False(t, e.Matches(tm(2026, 1, 1, 0, 10)))
}

func TestDOWSundayNormalization(t *testing.T) {
	// Both 0 and 7 should mean Sunday.
	e0, err := Parse("0 0 * * 0")
	require.NoError(t, err)

	e7, err := Parse("0 0 * * 7")
	require.NoError(t, err)

	// 2026-05-24 is a Sunday.
	sunday := tm(2026, 5, 24, 0, 0)
	monday := tm(2026, 5, 25, 0, 0)

	assert.True(t, e0.Matches(sunday))
	assert.True(t, e7.Matches(sunday))
	assert.False(t, e0.Matches(monday))
	assert.False(t, e7.Matches(monday))
}

func TestDOWRange(t *testing.T) {
	// Monday through Friday.
	e, err := Parse("0 9 * * 1-5")
	require.NoError(t, err)

	// 2026-05-25 is Monday.
	assert.True(t, e.Matches(tm(2026, 5, 25, 9, 0)))
	// 2026-05-29 is Friday.
	assert.True(t, e.Matches(tm(2026, 5, 29, 9, 0)))
	// 2026-05-24 is Sunday.
	assert.False(t, e.Matches(tm(2026, 5, 24, 9, 0)))
	// 2026-05-30 is Saturday (Weekday() == 6).
	assert.False(t, e.Matches(tm(2026, 5, 30, 9, 0)))
}

func TestDailyAt21(t *testing.T) {
	e, err := Parse("0 21 * * *")
	require.NoError(t, err)

	assert.True(t, e.Matches(tm(2026, 5, 23, 21, 0)))
	assert.False(t, e.Matches(tm(2026, 5, 23, 21, 1)))
	assert.False(t, e.Matches(tm(2026, 5, 23, 20, 0)))
}

func TestComplexExpression(t *testing.T) {
	// Every 5 minutes during business hours on weekdays in Jan/Jul.
	e, err := Parse("*/5 9-17 * 1,7 1-5")
	require.NoError(t, err)

	// 2026-01-05 is Monday, 9:00 -> match.
	assert.True(t, e.Matches(tm(2026, 1, 5, 9, 0)))
	// 2026-01-05 is Monday, 9:03 -> no match (not */5).
	assert.False(t, e.Matches(tm(2026, 1, 5, 9, 3)))
	// 2026-02-02 is Monday, 9:00 -> no match (not Jan/Jul).
	assert.False(t, e.Matches(tm(2026, 2, 2, 9, 0)))
}
