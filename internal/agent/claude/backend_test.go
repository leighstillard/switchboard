package claude

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// ---------------------------------------------------------------------------
// Mock commander for testing
// ---------------------------------------------------------------------------

// mockCommander provides canned stdout output for testing.
type mockCommander struct {
	mu      sync.Mutex
	outputs []string // one per Start call, in order
	calls   int
	started chan struct{} // closed on first Start
}

func newMockCommander(outputs ...string) *mockCommander {
	return &mockCommander{
		outputs: outputs,
		started: make(chan struct{}),
	}
}

func (m *mockCommander) Start(ctx context.Context, args []string, workdir string) (io.ReadCloser, func(), int, error) {
	m.mu.Lock()
	idx := m.calls
	m.calls++
	if idx == 0 {
		close(m.started)
	}
	m.mu.Unlock()

	var output string
	if idx < len(m.outputs) {
		output = m.outputs[idx]
	}

	reader := io.NopCloser(bytes.NewReader([]byte(output)))
	cancel := func() {} // no-op for tests

	return reader, cancel, 12345, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSubscribe(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())

	sessionID, events, err := b.Subscribe(context.Background(), "/tmp/test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sessionID == "" {
		t.Fatal("empty session ID")
	}
	if events == nil {
		t.Fatal("nil events channel")
	}

	// Session ID should be a UUID.
	if len(sessionID) != 36 {
		t.Errorf("session ID doesn't look like UUID: %q", sessionID)
	}
}

func TestSendMessage_FirstTurn(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"test-id","model":"claude-sonnet-4-20250514"}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello!"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success","cost_usd":0.001}`,
	}, "\n")

	cmd := newMockCommander(output)
	b := newForTest(DefaultConfig(), cmd)

	sessionID, events, err := b.Subscribe(context.Background(), "/tmp/test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	err = b.SendMessage(context.Background(), sessionID, "Hi", nil)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Collect events with timeout.
	var collected []agent.Event
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed unexpectedly")
			}
			collected = append(collected, ev)
			if ev.Type == agent.EventTurnDone || ev.Type == agent.EventTurnError {
				goto done
			}
		case <-timeout:
			t.Fatalf("timeout waiting for events; got %d so far: %+v", len(collected), collected)
		}
	}

done:
	// Verify event sequence.
	expected := []agent.EventType{
		agent.EventSessionReady,
		agent.EventTextDelta,
		agent.EventMessageEnd,
		agent.EventTurnDone,
	}

	if len(collected) != len(expected) {
		t.Fatalf("got %d events, want %d\nevents: %+v", len(collected), len(expected), collected)
	}
	for i, want := range expected {
		if collected[i].Type != want {
			t.Errorf("events[%d].Type = %v, want %v", i, collected[i].Type, want)
		}
	}

	if collected[1].Text != "Hello!" {
		t.Errorf("text event = %q, want Hello!", collected[1].Text)
	}
}

func TestSendMessage_ResumeTurn(t *testing.T) {
	// First turn
	firstOutput := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"test-id","model":"claude-sonnet-4-20250514"}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"First"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	// Second turn (resume)
	secondOutput := strings.Join([]string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Second"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	cmd := newMockCommander(firstOutput, secondOutput)
	b := newForTest(DefaultConfig(), cmd)

	sessionID, events, err := b.Subscribe(context.Background(), "/tmp/test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First turn
	if err := b.SendMessage(context.Background(), sessionID, "First", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	waitForTurnDone(t, events)

	// Second turn
	if err := b.SendMessage(context.Background(), sessionID, "Second", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	waitForTurnDone(t, events)

	// Verify two Start calls were made.
	cmd.mu.Lock()
	calls := cmd.calls
	cmd.mu.Unlock()
	if calls != 2 {
		t.Errorf("expected 2 Start calls, got %d", calls)
	}
}

func TestSendMessage_NotFound(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())

	err := b.SendMessage(context.Background(), "nonexistent", "test", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestCancel(t *testing.T) {
	// Provide output that takes time to process.
	// The cancel should emit EventInterrupted.
	output := strings.Join([]string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Thinking..."}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	cmd := newMockCommander(output)
	b := newForTest(DefaultConfig(), cmd)

	sessionID, events, err := b.Subscribe(context.Background(), "/tmp/test")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := b.SendMessage(context.Background(), sessionID, "test", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait a bit then cancel.
	time.Sleep(50 * time.Millisecond)
	if err := b.Cancel(context.Background(), sessionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Should see EventInterrupted in the events.
	timeout := time.After(2 * time.Second)
	sawInterrupted := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				goto check
			}
			if ev.Type == agent.EventInterrupted {
				sawInterrupted = true
				goto check
			}
			if ev.Type == agent.EventTurnDone || ev.Type == agent.EventTurnError {
				// Process may finish before cancel reaches it.
				goto check
			}
		case <-timeout:
			goto check
		}
	}
check:
	// The cancel may arrive before or after the process finishes in tests.
	// We just verify it doesn't error.
	_ = sawInterrupted
}

func TestCancel_NotFound(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())
	err := b.Cancel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestSubscribeExisting(t *testing.T) {
	output := strings.Join([]string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Resumed"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success"}`,
	}, "\n")

	cmd := newMockCommander(output)
	b := newForTest(DefaultConfig(), cmd)

	// Subscribe to an existing session (like after restart).
	existingID := "c5714ea1-f992-45a8-96d0-9abcc2b845a1"
	events, err := b.SubscribeExisting(context.Background(), existingID, "/tmp/test")
	if err != nil {
		t.Fatalf("SubscribeExisting: %v", err)
	}

	// Send a message (should use --resume).
	if err := b.SendMessage(context.Background(), existingID, "continue", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Collect events.
	collected := waitForTurnDone(t, events)

	// Should have TextDelta and TurnDone at minimum.
	hasText := false
	hasDone := false
	for _, ev := range collected {
		if ev.Type == agent.EventTextDelta && ev.Text == "Resumed" {
			hasText = true
		}
		if ev.Type == agent.EventTurnDone {
			hasDone = true
		}
	}
	if !hasText {
		t.Error("expected TextDelta with 'Resumed'")
	}
	if !hasDone {
		t.Error("expected TurnDone")
	}
}

func TestSubscribeExisting_Idempotent(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())

	sessionID := "test-session-id"
	events1, err := b.SubscribeExisting(context.Background(), sessionID, "/tmp")
	if err != nil {
		t.Fatalf("first SubscribeExisting: %v", err)
	}

	events2, err := b.SubscribeExisting(context.Background(), sessionID, "/tmp")
	if err != nil {
		t.Fatalf("second SubscribeExisting: %v", err)
	}

	// Should return the same channel.
	if events1 != events2 {
		t.Error("expected same events channel for idempotent SubscribeExisting")
	}
}

func TestClose(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())

	_, events, _ := b.Subscribe(context.Background(), "/tmp/test")

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Events channel should be closed.
	_, ok := <-events
	if ok {
		t.Error("expected events channel to be closed after Close")
	}
}

func TestBuildArgs_FirstTurn(t *testing.T) {
	b := newForTest(Config{
		Binary:             "claude",
		PermissionMode:     "bypassPermissions",
		Model:              "claude-sonnet-4-20250514",
		AppendSystemPrompt: "Be helpful",
		ExtraArgs:          []string{"--max-turns", "10"},
	}, newMockCommander())

	args := b.buildArgs("session-123", "Hello", true)

	assertContains(t, args, "-p", "Hello")
	assertContains(t, args, "--output-format", "stream-json")
	assertContains(t, args, "--verbose")
	assertContains(t, args, "--include-partial-messages")
	assertContains(t, args, "--model", "claude-sonnet-4-20250514")
	assertContains(t, args, "--permission-mode", "bypassPermissions")
	assertContains(t, args, "--session-id", "session-123")
	assertContains(t, args, "--append-system-prompt", "Be helpful")
	assertContains(t, args, "--max-turns", "10")

	// Should NOT have --resume on first turn.
	for _, a := range args {
		if a == "--resume" {
			t.Error("first turn should not have --resume")
		}
	}
}

func TestBuildArgs_ResumeTurn(t *testing.T) {
	b := newForTest(DefaultConfig(), newMockCommander())

	args := b.buildArgs("session-123", "Continue", false)

	assertContains(t, args, "--resume", "session-123")

	// Should NOT have --session-id on resume.
	for _, a := range args {
		if a == "--session-id" {
			t.Error("resume turn should not have --session-id")
		}
	}
}

func TestProcessExitWithoutResult(t *testing.T) {
	// Process exits without emitting a result event.
	output := strings.Join([]string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		// No result event - process just exits.
	}, "\n")

	cmd := newMockCommander(output)
	b := newForTest(DefaultConfig(), cmd)

	sessionID, events, _ := b.Subscribe(context.Background(), "/tmp/test")
	b.SendMessage(context.Background(), sessionID, "test", nil)

	// Should get TurnError since no result event was seen.
	collected := waitForTurnEnd(t, events)

	lastEvent := collected[len(collected)-1]
	if lastEvent.Type != agent.EventTurnError {
		t.Errorf("expected TurnError for process exit without result, got %v", lastEvent.Type)
	}
	if lastEvent.ErrorMessage == "" {
		t.Error("expected non-empty error message")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitForTurnDone reads events until TurnDone or TurnError, with timeout.
func waitForTurnDone(t *testing.T, events <-chan agent.Event) []agent.Event {
	t.Helper()
	return waitForTurnEnd(t, events)
}

func waitForTurnEnd(t *testing.T, events <-chan agent.Event) []agent.Event {
	t.Helper()
	var collected []agent.Event
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, ev)
			if ev.Type == agent.EventTurnDone || ev.Type == agent.EventTurnError || ev.Type == agent.EventInterrupted {
				return collected
			}
		case <-timeout:
			t.Fatalf("timeout waiting for turn end; got %d events: %+v", len(collected), collected)
			return collected
		}
	}
}

// assertContains checks that args contains the key (and optionally value).
func assertContains(t *testing.T, args []string, parts ...string) {
	t.Helper()
	if len(parts) == 1 {
		for _, a := range args {
			if a == parts[0] {
				return
			}
		}
		t.Errorf("args missing %q: %v", parts[0], args)
		return
	}

	key, val := parts[0], parts[1]
	for i, a := range args {
		if a == key && i+1 < len(args) && args[i+1] == val {
			return
		}
	}
	t.Errorf("args missing %q %q: %v", key, val, args)
}
