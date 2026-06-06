package router

import (
	"testing"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/llmrouter"
)

// ---------------------------------------------------------------------------
// Issue 2: handleTurnEnd should not send ✅ for error/interrupted events
// ---------------------------------------------------------------------------

// TestShouldNotifySuccess validates the logic for when to send the success
// notification. Only EventTurnDone should trigger ✅; errors and interruptions
// should be silent (the coalescer already shows them).
func TestShouldNotifySuccess(t *testing.T) {
	tests := []struct {
		name      string
		eventType agent.EventType
		want      bool
	}{
		{"done event sends success", agent.EventTurnDone, true},
		{"error event skips success", agent.EventTurnError, false},
		{"interrupted event skips success", agent.EventInterrupted, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldNotifySuccess(tt.eventType)
			if got != tt.want {
				t.Errorf("shouldNotifySuccess(%v) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
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
