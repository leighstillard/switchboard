package claude

import (
	"sort"
	"sync"
	"time"
)

// clock abstracts time so idle-eviction is deterministically testable.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) timer
}

// timer is the subset of *time.Timer the backend uses.
type timer interface {
	Stop() bool
}

// realClock delegates to the time package.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) timer {
	return time.AfterFunc(d, f)
}

// ---------------------------------------------------------------------------
// fakeClock — manual time control for tests
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{deadline: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock forward and fires any timers whose deadline passed.
// Callbacks run without the clock lock held so they may reschedule timers.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*fakeTimer
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.stopped && !t.deadline.After(now) {
			t.fired = true
			due = append(due, t)
		} else if !t.stopped {
			kept = append(kept, t)
		}
	}
	c.timers = kept
	c.mu.Unlock()

	// Deterministic order: earliest deadline first.
	sort.SliceStable(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })
	for _, t := range due {
		t.f()
	}
}

type fakeTimer struct {
	deadline time.Time
	f        func()
	stopped  bool
	fired    bool
}

func (t *fakeTimer) Stop() bool {
	wasActive := !t.stopped && !t.fired
	t.stopped = true
	return wasActive
}
