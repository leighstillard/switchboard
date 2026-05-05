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

// ---------------------------------------------------------------------------
// Feature 1c: Terse tool descriptions
// ---------------------------------------------------------------------------

func TestCoalescer_ToolDescription_Read(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-desc-1", "fox", "C100", "ts100", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// ToolStart with input containing file_path.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolStart, map[string]interface{}{
		"type":  "tool_start",
		"id":    "t1",
		"name":  "Read",
		"input": map[string]interface{}{"file_path": "/home/user/workspace/auth.go"},
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t1", "name": "Read", "output": "contents",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "Reading `auth.go`") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Reading `auth.go`' in output")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_ToolDescription_Bash(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-desc-2", "fox", "C101", "ts101", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolStart, map[string]interface{}{
		"type":  "tool_start",
		"id":    "t2",
		"name":  "Bash",
		"input": map[string]interface{}{"command": "go test ./..."},
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolExec, map[string]interface{}{
		"type": "tool_exec", "id": "t2", "name": "Bash",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t2", "name": "Bash", "output": "ok",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "Running `go test ./...`") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Running `go test ./...`' in output")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_ToolDescription_Fallback(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-desc-3", "fox", "C102", "ts102", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Tool with no input -- should fall back to "Running Read".
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolStart, map[string]interface{}{
		"type": "tool_start", "id": "t3", "name": "Read",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t3", "name": "Read", "output": "ok",
	}))
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	}))

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "Running Read") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Running Read' fallback in output")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_ToolDescription_PendingShowsDescription(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-desc-4", "fox", "C103", "ts103", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	coal.HandleEvent(makeEvent(t, jcodeproto.EventToolStart, map[string]interface{}{
		"type":  "tool_start",
		"id":    "t4",
		"name":  "Grep",
		"input": map[string]interface{}{"pattern": "TODO"},
	}))

	// Wait for timer flush so we see the pending tool.
	time.Sleep(1500 * time.Millisecond)

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "Find `TODO` uses") && contains(item.Text, toolSpinner) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected pending tool with description 'Find `TODO` uses'")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// Directive integration tests (Feature 1a)
// ---------------------------------------------------------------------------

func TestCoalescer_DirectiveExtraction(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-dir", "owl", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Owl Worker"}, out, nil)
	defer coal.Close()

	// Send text that includes a render directive.
	directiveText := "Here's the plan:\n```switchboard\n{\"render\": \"plan\", \"title\": \"Deploy\", \"tasks\": [{\"id\": \"1\", \"title\": \"Build\", \"status\": \"complete\"}, {\"id\": \"2\", \"title\": \"Test\", \"status\": \"pending\"}]}\n```\nDone explaining."

	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta",
		"text": directiveText,
	}))

	// Trigger a final flush.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done",
		"id":   1,
	}))

	// Wait for flush.
	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected at least one outbound item")
	}

	// Find the item with blocks.
	var hasBlocks bool
	var hasCleanText bool
	for _, item := range items {
		if len(item.Blocks) > 0 {
			hasBlocks = true
		}
		// Directive should be removed from visible text
		if contains(item.Text, "Done explaining") && !contains(item.Text, "switchboard") {
			hasCleanText = true
		}
	}

	if !hasBlocks {
		t.Error("expected blocks from plan directive in outbound item")
		for _, item := range items {
			t.Logf("  item: text=%q blocks=%d", item.Text[:min(len(item.Text), 100)], len(item.Blocks))
		}
	}
	if !hasCleanText {
		t.Error("directive should be removed from text, leaving surrounding content")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_DirectiveInvalid_LeftInText(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-dir2", "bear", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Bear Worker"}, out, nil)
	defer coal.Close()

	// Send text with an invalid directive (unknown type).
	directiveText := "Check this:\n```switchboard\n{\"render\": \"unknown_type\", \"data\": 123}\n```\nEnd."

	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta",
		"text": directiveText,
	}))

	// Final flush.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done",
		"id":   1,
	}))

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected at least one outbound item")
	}

	// Invalid directives should remain visible in text.
	var foundInText bool
	for _, item := range items {
		if contains(item.Text, "unknown_type") {
			foundInText = true
			break
		}
	}
	if !foundInText {
		t.Error("invalid directive should remain visible in text")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_PlainCodeBlock_NotIntercepted(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-dir3", "cat", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Cat Worker"}, out, nil)
	defer coal.Close()

	// Send text with a plain code block (python).
	text := "Here's code:\n```python\ndef hello():\n    print('hi')\n```\nDone."

	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta",
		"text": text,
	}))

	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done",
		"id":   1,
	}))

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected at least one outbound item")
	}

	// No blocks should be produced from a python code block.
	for _, item := range items {
		if len(item.Blocks) > 0 {
			t.Error("python code block should NOT produce blocks")
		}
	}

	// The code should remain in the text.
	var found bool
	for _, item := range items {
		if contains(item.Text, "def hello") {
			found = true
			break
		}
	}
	if !found {
		t.Error("python code should remain in output text")
	}
}

func TestCoalescer_DirectiveNoDuplication_AcrossFlushes(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-dup", "elk", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Elk Worker"}, out, nil)
	defer coal.Close()

	// Send text with a directive.
	directiveText := "Here:\n```switchboard\n{\"render\": \"todos\", \"items\": [{\"text\": \"A\", \"done\": false}]}\n```\nMore text."

	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta",
		"text": directiveText,
	}))

	// Wait for a timer flush (first render).
	time.Sleep(1500 * time.Millisecond)

	// Append more text (triggers another flush with the same directive still in buffer).
	coal.HandleEvent(makeEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta",
		"text": " Even more text.",
	}))

	// Wait for second timer flush.
	time.Sleep(1500 * time.Millisecond)

	// Final flush.
	coal.HandleEvent(makeEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done",
		"id":   1,
	}))

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()

	// Check the last item (the final flush) - should have blocks but NOT duplicated.
	var lastItemWithBlocks *outbound.OutboundItem
	for i := len(items) - 1; i >= 0; i-- {
		if len(items[i].Blocks) > 0 {
			lastItemWithBlocks = items[i]
			break
		}
	}

	if lastItemWithBlocks == nil {
		t.Fatal("expected at least one item with blocks")
	}

	// Count header blocks (todos directive produces one header block).
	headerCount := 0
	for _, b := range lastItemWithBlocks.Blocks {
		if b["type"] == "header" {
			headerCount++
		}
	}

	if headerCount > 1 {
		t.Errorf("directive blocks duplicated: found %d header blocks, want 1", headerCount)
	}
}
