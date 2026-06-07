package router

import (
	"context"
	"testing"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/llmrouter"
)

// ---------------------------------------------------------------------------
// Mock backend for testing
// ---------------------------------------------------------------------------

type mockBackend struct {
	name string
}

func (m *mockBackend) Subscribe(ctx context.Context, workdir string) (string, <-chan agent.Event, error) {
	return "", nil, nil
}
func (m *mockBackend) SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan agent.Event, error) {
	return nil, nil
}
func (m *mockBackend) SendMessage(ctx context.Context, sessionID, content string, images []agent.Image) error {
	return nil
}
func (m *mockBackend) Cancel(ctx context.Context, sessionID string) error {
	return nil
}
func (m *mockBackend) CloseSession(ctx context.Context, sessionID string) error {
	return nil
}
func (m *mockBackend) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Backend selector tests
// ---------------------------------------------------------------------------

func TestBackendFor_DefaultJcode(t *testing.T) {
	jcodeBe := &mockBackend{name: "jcode"}
	claudeBe := &mockBackend{name: "claude"}

	r := &Router{
		cfg: &config.Config{
			Channels: []config.ChannelConfig{
				{ID: "C111", Name: "test"},
			},
		},
		backend:       jcodeBe,
		claudeBackend: claudeBe,
	}

	be, name := r.backendFor("C111")
	if name != "jcode" {
		t.Errorf("expected jcode, got %q", name)
	}
	if be != jcodeBe {
		t.Error("expected jcode backend instance")
	}
}

func TestBackendFor_GlobalDefault(t *testing.T) {
	jcodeBe := &mockBackend{name: "jcode"}
	claudeBe := &mockBackend{name: "claude"}

	r := &Router{
		cfg: &config.Config{
			Routing: config.RoutingConfig2{
				Backend: config.BackendRoutingConfig{Default: "claude"},
			},
			Channels: []config.ChannelConfig{
				{ID: "C111", Name: "test"},
			},
		},
		backend:       jcodeBe,
		claudeBackend: claudeBe,
	}

	be, name := r.backendFor("C111")
	if name != "claude" {
		t.Errorf("expected claude, got %q", name)
	}
	if be != claudeBe {
		t.Error("expected claude backend instance")
	}
}

func TestBackendFor_ChannelOverride(t *testing.T) {
	jcodeBe := &mockBackend{name: "jcode"}
	claudeBe := &mockBackend{name: "claude"}

	r := &Router{
		cfg: &config.Config{
			Routing: config.RoutingConfig2{
				Backend: config.BackendRoutingConfig{Default: "jcode"},
			},
			Channels: []config.ChannelConfig{
				{ID: "C111", Name: "jcode-channel"},
				{ID: "C222", Name: "claude-channel", Backend: "claude"},
			},
		},
		backend:       jcodeBe,
		claudeBackend: claudeBe,
	}

	// C111: no override -> default jcode
	be, name := r.backendFor("C111")
	if name != "jcode" {
		t.Errorf("C111: expected jcode, got %q", name)
	}
	if be != jcodeBe {
		t.Error("C111: expected jcode backend")
	}

	// C222: override to claude
	be, name = r.backendFor("C222")
	if name != "claude" {
		t.Errorf("C222: expected claude, got %q", name)
	}
	if be != claudeBe {
		t.Error("C222: expected claude backend")
	}
}

func TestBackendFor_ClaudeNilFallback(t *testing.T) {
	jcodeBe := &mockBackend{name: "jcode"}

	r := &Router{
		cfg: &config.Config{
			Routing: config.RoutingConfig2{
				Backend: config.BackendRoutingConfig{Default: "claude"},
			},
			Channels: []config.ChannelConfig{
				{ID: "C111", Name: "test"},
			},
		},
		backend:       jcodeBe,
		claudeBackend: nil, // Claude not configured
	}

	// Claude is default but nil: should fall back to jcode.
	be, name := r.backendFor("C111")
	if name != "jcode" {
		t.Errorf("expected jcode fallback, got %q", name)
	}
	if be != jcodeBe {
		t.Error("expected jcode backend when claude is nil")
	}
}

func TestBackendForSession(t *testing.T) {
	jcodeBe := &mockBackend{name: "jcode"}
	claudeBe := &mockBackend{name: "claude"}

	r := &Router{
		cfg:            &config.Config{},
		backend:        jcodeBe,
		claudeBackend:  claudeBe,
		sessionBackend: map[string]string{"sess-1": "claude", "sess-2": "jcode"},
	}

	// In-memory lookup.
	be := r.backendForSession("sess-1", "")
	if be != claudeBe {
		t.Error("sess-1: expected claude backend from in-memory map")
	}

	// Fallback to store value.
	be = r.backendForSession("sess-3", "claude")
	if be != claudeBe {
		t.Error("sess-3: expected claude backend from store fallback")
	}

	// Default jcode.
	be = r.backendForSession("sess-4", "")
	if be != jcodeBe {
		t.Error("sess-4: expected jcode backend as default")
	}
}

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

func TestCleanModelName(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-20250514":  "claude-sonnet-4",
		"claude-3-5-haiku-20241022": "claude-3-5-haiku",
		"claude-sonnet-4-6":         "claude-sonnet-4-6", // not a date snapshot
		"claude-opus-4-8":           "claude-opus-4-8",
		"claude-sonnet-4":           "claude-sonnet-4",
		"":                          "",
	}
	for in, want := range cases {
		if got := cleanModelName(in); got != want {
			t.Errorf("cleanModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionLabel(t *testing.T) {
	cases := []struct {
		sessionID, model, want string
	}{
		{"session_snake_1777_abc", "claude-sonnet-4-20250514", "snake"},   // jcode animal wins
		{"574853d1-1014-43b8-a8be-bb631a5fb7c1", "claude-sonnet-4-20250514", "claude-sonnet-4"}, // claude UUID → clean model
		{"574853d1-1014-43b8-a8be-bb631a5fb7c1", "", ""},                   // no model → empty (caller skips)
	}
	for _, c := range cases {
		if got := sessionLabel(c.sessionID, c.model); got != c.want {
			t.Errorf("sessionLabel(%q,%q) = %q, want %q", c.sessionID, c.model, got, c.want)
		}
	}
}
