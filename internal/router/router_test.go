package router

import (
	"sync"
	"testing"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/jcodeproto"
	"github.com/format5/switchboard/internal/llmrouter"
)

// ---------------------------------------------------------------------------
// Issue 2: handleTurnEnd should not send ✅ for error/interrupted events
// ---------------------------------------------------------------------------

// TestShouldNotifySuccess validates the logic for when to send the success
// notification. Only EventDone should trigger ✅; errors and interruptions
// should be silent (the coalescer already shows them).
func TestShouldNotifySuccess(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		want      bool
	}{
		{"done event sends success", jcodeproto.EventDone, true},
		{"error event skips success", jcodeproto.EventError, false},
		{"interrupted event skips success", jcodeproto.EventInterrupted, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldNotifySuccess(tt.eventType)
			if got != tt.want {
				t.Errorf("shouldNotifySuccess(%q) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Config reload race: r.cfg is read on hot paths while Reload swaps it on
// SIGHUP. The atomic.Pointer must allow concurrent reads and writes without a
// data race. Run with -race to validate.
// ---------------------------------------------------------------------------

func TestConfigReloadRace(t *testing.T) {
	r := &Router{}
	r.cfg.Store(&config.Config{
		Channels: []config.ChannelConfig{
			{ID: "C123", Workdir: "/tmp/a", Identity: "alpha"},
		},
	})

	var wg sync.WaitGroup
	const iterations = 1000

	// Writer: swap the config pointer repeatedly (mirrors Reload's r.cfg.Store).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			r.cfg.Store(&config.Config{
				Channels: []config.ChannelConfig{
					{ID: "C123", Workdir: "/tmp/a", Identity: "alpha"},
				},
			})
		}
	}()

	// Readers: hit resolveChannel (reads r.cfg) concurrently. The matching
	// channel returns before any edge access, so nil deps are safe here.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if wd, _ := r.resolveChannel("C123"); wd != "/tmp/a" {
					t.Errorf("resolveChannel: got %q, want /tmp/a", wd)
					return
				}
			}
		}()
	}

	wg.Wait()
}

// ---------------------------------------------------------------------------
// Issue 4: Drop queued nextKey on failed next-thread turn start
// ---------------------------------------------------------------------------

// TestDropNextKeyOnSendFailure validates that when SendMessage fails for a
// next-thread batch, the nextKey is removed from the coalescerQueue so it
// doesn't block future routing.
func TestDropNextKeyOnSendFailure(t *testing.T) {
	// Simulate state: sessionID has two keys queued (current popped, next remains).
	sessionID := "session_fox_123_abc"
	nextKey := "C123:1234567890.123456"

	queue := map[string][]string{
		sessionID: {nextKey},
	}

	// Simulate: SendMessage for nextKey failed.
	// After failure, the queue entry for nextKey should be removed.
	// This mirrors the fix we'll add: pop nextKey from queue on send failure.
	dropNextKeyFromQueue(queue, sessionID, nextKey)

	if q := queue[sessionID]; len(q) != 0 {
		t.Errorf("expected queue to be empty after dropping nextKey, got %v", q)
	}
}

// TestDropNextKeyOnSendFailure_PreservesOtherKeys validates that dropping a
// nextKey only removes the front entry, preserving any subsequent entries.
func TestDropNextKeyOnSendFailure_PreservesOtherKeys(t *testing.T) {
	sessionID := "session_fox_123_abc"
	nextKey := "C123:1234567890.123456"
	otherKey := "C456:9876543210.654321"

	queue := map[string][]string{
		sessionID: {nextKey, otherKey},
	}

	dropNextKeyFromQueue(queue, sessionID, nextKey)

	if q := queue[sessionID]; len(q) != 1 || q[0] != otherKey {
		t.Errorf("expected queue to contain only otherKey, got %v", q)
	}
}

// TestDropNextKeyOnSendFailure_RemovesAppendedTail reproduces the case where
// handleTurnEnd appends the coalKey to the TAIL of the queue while a different
// thread is already at the front. On send failure the appended tail entry must
// be removed, not the front entry (which belongs to another active thread).
func TestDropNextKeyOnSendFailure_RemovesAppendedTail(t *testing.T) {
	sessionID := "session_fox_123_abc"
	frontKey := "C456:9876543210.654321" // another thread already at front
	coalKey := "C123:1234567890.123456"  // the key handleTurnEnd appended

	queue := map[string][]string{
		sessionID: {frontKey, coalKey},
	}

	dropNextKeyFromQueue(queue, sessionID, coalKey)

	if q := queue[sessionID]; len(q) != 1 || q[0] != frontKey {
		t.Errorf("expected queue to retain only frontKey, got %v", q)
	}
}

// ---------------------------------------------------------------------------
// Issue 5: Validate LLM thread_id against provided threads
// ---------------------------------------------------------------------------

// TestValidateLLMThreadID checks that the LLM's suggested thread_id is
// validated against the actual thread list provided to the LLM.
func TestValidateLLMThreadID(t *testing.T) {
	threads := []llmrouter.ThreadContext{
		{ChannelID: "C111", ThreadTS: "1000.001"},
		{ChannelID: "C222", ThreadTS: "2000.002"},
		{ChannelID: "C333", ThreadTS: "3000.003"},
	}

	tests := []struct {
		name     string
		threadID string
		valid    bool
	}{
		{"valid thread_id matches first", "C111:1000.001", true},
		{"valid thread_id matches second", "C222:2000.002", true},
		{"valid thread_id matches third", "C333:3000.003", true},
		{"invalid channel", "C999:1000.001", false},
		{"invalid thread_ts", "C111:9999.999", false},
		{"completely fabricated", "CXXX:0000.000", false},
		{"malformed - no colon", "C111-1000.001", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidLLMThreadID(tt.threadID, threads)
			if got != tt.valid {
				t.Errorf("isValidLLMThreadID(%q) = %v, want %v", tt.threadID, got, tt.valid)
			}
		})
	}
}
