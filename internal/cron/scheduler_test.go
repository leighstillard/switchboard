package cron

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock dispatcher
// ---------------------------------------------------------------------------

type mockDispatcher struct {
	mu      sync.Mutex
	calls   []DispatchRequest
	results []*DispatchResult
	err     error
}

func (m *mockDispatcher) Dispatch(_ context.Context, req DispatchRequest) (*DispatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if m.err != nil {
		return nil, m.err
	}
	result := &DispatchResult{ThreadTS: "1234.5678", SessionID: "session_test_123"}
	if len(m.results) > 0 {
		result = m.results[len(m.calls)-1]
	}
	return result, nil
}

func (m *mockDispatcher) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "cron-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	st, err := store.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewRejectsInvalidSchedule(t *testing.T) {
	st := testStore(t)
	_, err := New([]Job{{ID: "bad", Schedule: "not-valid", Enabled: true}}, &mockDispatcher{}, st)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "job \"bad\"")
}

func TestJobMatchingAndDispatch(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "every-min", Schedule: "* * * * *", ChannelID: "C123", Prompt: "hello", UserID: "U456", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	// Manually trigger a tick -- the "* * * * *" schedule always matches.
	ctx := context.Background()
	sched.tick(ctx)

	// Wait briefly for async dispatch goroutine.
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, disp.callCount())
	disp.mu.Lock()
	assert.Equal(t, "C123", disp.calls[0].ChannelID)
	assert.Equal(t, "hello", disp.calls[0].Prompt)
	assert.Equal(t, "U456", disp.calls[0].UserID)
	disp.mu.Unlock()
}

func TestDedup(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "dedup-test", Schedule: "* * * * *", ChannelID: "C123", Prompt: "dup", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	ctx := context.Background()

	// Two ticks within the same minute should only fire once.
	sched.tick(ctx)
	sched.tick(ctx)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, disp.callCount())
}

func TestDisabledJobsSkipped(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "disabled", Schedule: "* * * * *", ChannelID: "C123", Prompt: "skip", Enabled: false},
	}, disp, st)
	require.NoError(t, err)

	sched.tick(context.Background())
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 0, disp.callCount())
}

func TestReloadUpdatesJobs(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "old", Schedule: "0 0 1 1 *", ChannelID: "C123", Prompt: "old", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	// Old job won't match now (midnight Jan 1), so no dispatch.
	sched.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, disp.callCount())

	// Reload with always-matching job.
	err = sched.Reload([]Job{
		{ID: "new", Schedule: "* * * * *", ChannelID: "CNEW", Prompt: "new", Enabled: true},
	})
	require.NoError(t, err)

	sched.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, disp.callCount())

	disp.mu.Lock()
	assert.Equal(t, "CNEW", disp.calls[0].ChannelID)
	disp.mu.Unlock()
}

func TestReloadRejectsInvalidSchedule(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "good", Schedule: "* * * * *", ChannelID: "C123", Prompt: "ok", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	err = sched.Reload([]Job{
		{ID: "bad", Schedule: "invalid", Enabled: true},
	})
	require.Error(t, err)

	// Original jobs should still be in place.
	sched.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, disp.callCount())
}

func TestMultipleJobsSameMinute(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "job-a", Schedule: "* * * * *", ChannelID: "CA", Prompt: "a", Enabled: true},
		{ID: "job-b", Schedule: "* * * * *", ChannelID: "CB", Prompt: "b", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	sched.tick(context.Background())
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 2, disp.callCount())
}

func TestDedupSurvivesRestart(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	// First scheduler fires the job.
	sched1, err := New([]Job{
		{ID: "restart-test", Schedule: "* * * * *", ChannelID: "C123", Prompt: "hello", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	sched1.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, disp.callCount())

	// Simulate restart: create a new scheduler with the same store.
	// The job should NOT fire again for the same minute.
	sched2, err := New([]Job{
		{ID: "restart-test", Schedule: "* * * * *", ChannelID: "C123", Prompt: "hello", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	sched2.tick(context.Background())
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, disp.callCount(), "job should not fire again after restart within the same minute")
}

func TestDedupConcurrentTicks(t *testing.T) {
	st := testStore(t)
	disp := &mockDispatcher{}

	sched, err := New([]Job{
		{ID: "concurrent-test", Schedule: "* * * * *", ChannelID: "C123", Prompt: "hello", Enabled: true},
	}, disp, st)
	require.NoError(t, err)

	// Fire 10 concurrent ticks -- only one should dispatch.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sched.tick(context.Background())
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1, disp.callCount(), "concurrent ticks should only dispatch once")
}
