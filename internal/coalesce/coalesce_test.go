package coalesce

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/jcodeproto"
	"github.com/format5/switchboard/internal/outbound"
)

// mockOutbound captures enqueued items for testing.
type mockOutbound struct {
	mu    sync.Mutex
	items []*outbound.OutboundItem
}

func (m *mockOutbound) Enqueue(item *outbound.OutboundItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append(m.items, item)
	// Simulate the real outbound worker: call OnPosted for PostMessage items.
	if item.Action == outbound.ActionPostMessage && item.OnPosted != nil {
		ts := fmt.Sprintf("mock-%d.%06d", len(m.items), len(m.items))
		go item.OnPosted(ts)
	}
}

func (m *mockOutbound) getItems() []*outbound.OutboundItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*outbound.OutboundItem{}, m.items...)
}

func makeEvent(t *testing.T, evType string, data interface{}) *jcodeproto.ServerEvent {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return &jcodeproto.ServerEvent{Type: evType, Raw: raw}
}

func TestCoalescer_TextDelta(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-1", "fox", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Test Worker"}, out, nil)
	defer coal.Close()

	// Send text deltas.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "Hello ",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "world!",
	}))

	// Trigger flush via Done.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected at least one outbound item")
	}

	// The message should contain our text.
	found := false
	for _, item := range items {
		if contains(item.Text, "Hello world!") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected text to contain 'Hello world!', got items: %+v", items)
	}
}

func TestCoalescer_TextReplace(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-2", "cat", "C456", "ts2", "/workspace/other",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Send text delta then replace.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "garbled output",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextReplace, map[string]string{
		"type": "text_replace", "text": "clean output",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	// Should contain "clean output" and NOT "garbled output".
	for _, item := range items {
		if contains(item.Text, "garbled") {
			t.Error("text should not contain 'garbled output' after replace")
		}
	}
}

func TestCoalescer_ToolProgress(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-3", "dog", "C789", "ts3", "/workspace/tools",
		Identity{DisplayName: "Tool Worker"}, out, nil)
	defer coal.Close()

	// Tool lifecycle.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolStart, map[string]string{
		"type": "tool_start", "id": "t1", "name": "Read",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolExec, map[string]string{
		"type": "tool_exec", "id": "t1", "name": "Read",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t1", "name": "Read", "output": "file contents",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	// Should show completed tool.
	found := false
	for _, item := range items {
		if contains(item.Text, "Read") && contains(item.Text, toolCheckmark) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected completed tool 'Read' with checkmark in output")
	}
}

func TestCoalescer_ErrorEvent(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-4", "bear", "C000", "ts4", "/workspace/err",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	coal.HandleEvent(makeEvent(t, jcodeproto.EventError, map[string]interface{}{
		"type": "error", "id": float64(1), "message": "rate limited",
	}))

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	found := false
	for _, item := range items {
		if contains(item.Text, "rate limited") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected error message in output")
	}
}

func TestCoalescer_UpstreamProvider(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-5", "owl", "C111", "ts5", "/workspace/prov",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	coal.HandleEvent(makeEvent(t, jcodeproto.EventUpstreamProvider, map[string]string{
		"type": "upstream_provider", "provider": "gpt-4o",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "response",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "gpt-4o") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected upstream provider in final flush")
	}
}

func TestCoalescer_LazyFlush(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-6", "ant", "C222", "ts6", "/workspace/lazy",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Send text delta without triggering a terminal event.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "streaming...",
	}))

	// Should not have flushed yet (less than 1s).
	items := out.getItems()
	if len(items) != 0 {
		t.Error("should not have flushed immediately")
	}

	// Wait for the timer to fire.
	time.Sleep(1500 * time.Millisecond)

	items = out.getItems()
	if len(items) == 0 {
		t.Error("should have flushed after timer interval")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
