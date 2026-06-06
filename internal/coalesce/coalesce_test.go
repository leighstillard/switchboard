package coalesce

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/agent"
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

func TestCoalescer_TextDelta(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-1", "fox", "C123", "ts1", "/workspace/test",
		Identity{DisplayName: "Test Worker"}, out, nil)
	defer coal.Close()

	// Send text deltas.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Hello "})
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "world!"})

	// Trigger flush via Done.
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "garbled output"})
	coal.HandleEvent(agent.Event{Type: agent.EventTextReplace, Text: "clean output"})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t1", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolExec, ToolID: "t1", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t1", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{
		Type: agent.EventTurnError, ErrorMessage: "rate limited",
	})

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

	coal.HandleEvent(agent.Event{Type: agent.EventProvider, ProviderName: "gpt-4o"})
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "response"})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "streaming..."})

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
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t1", ToolName: "Read",
		ToolInput: map[string]any{"file_path": "/home/user/workspace/auth.go"},
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t1", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t2", ToolName: "Bash",
		ToolInput: map[string]any{"command": "go test ./..."},
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolExec, ToolID: "t2", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t2", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	items := out.getItems()
	found := false
	for _, item := range items {
		if contains(item.Text, "Testing ./...") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Testing ./...' in output")
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
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t3", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t3", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t4", ToolName: "Grep",
		ToolInput: map[string]any{"pattern": "TODO"},
	})

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

	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: directiveText})

	// Trigger a final flush.
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: directiveText})

	// Final flush.
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: text})

	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: directiveText})

	// Wait for a timer flush (first render).
	time.Sleep(1500 * time.Millisecond)

	// Append more text (triggers another flush with the same directive still in buffer).
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: " Even more text."})

	// Wait for second timer flush.
	time.Sleep(1500 * time.Millisecond)

	// Final flush.
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

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

// TestCoalescer_OverflowFromManyTools verifies that many completed tools
// trigger an overflow split even when the text buffer itself is short.
// Uses unique file paths so sequential dedup doesn't collapse them.
func TestCoalescer_OverflowFromManyTools(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-overflow", "ant", "C999", "ts99", "/workspace/big",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Add a small text buffer.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Starting work...\n"})

	// Simulate 150 tool calls with UNIQUE file names.
	for i := 0; i < 150; i++ {
		toolID := fmt.Sprintf("tool-%d", i)
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolStart, ToolID: toolID, ToolName: "Read",
			ToolInput: map[string]any{"file_path": fmt.Sprintf("/workspace/handler_%d.go", i)},
		})
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolDone, ToolID: toolID, ToolName: "Read",
		})
	}

	// Final text + done.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "All done reviewing files."})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	// Wait for async callbacks.
	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) < 2 {
		t.Fatalf("expected at least 2 outbound items (overflow split), got %d", len(items))
	}

	// Verify that no single item's text exceeds a reasonable limit.
	for i, item := range items {
		if len(item.Text) > 4500 {
			t.Errorf("item %d text too long (%d chars), should have been split", i, len(item.Text))
		}
	}

	// Verify the final text appears in the last item.
	lastItem := items[len(items)-1]
	if !contains(lastItem.Text, "All done reviewing files") {
		t.Error("expected final text in last outbound item")
		t.Logf("last item text: %s", lastItem.Text[:min(200, len(lastItem.Text))])
	}
}

// TestCoalescer_SequentialToolDedup verifies that sequential identical tool
// descriptions are collapsed into a single entry with a count.
func TestCoalescer_SequentialToolDedup(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-dedup", "fox", "C300", "ts300", "/workspace/dedup",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Querying databases...\n"})

	// Three sequential identical tool calls.
	for i := 0; i < 3; i++ {
		toolID := fmt.Sprintf("pg-%d", i)
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolStart, ToolID: toolID, ToolName: "postgres",
			ToolInput: map[string]any{"query": "SELECT * FROM users"},
		})
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolDone, ToolID: toolID, ToolName: "postgres",
		})
	}

	// Then a different tool.
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "read-1", ToolName: "Read",
		ToolInput: map[string]any{"file_path": "/workspace/config.go"},
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "read-1", ToolName: "Read",
	})

	// Then two more identical tool calls.
	for i := 0; i < 2; i++ {
		toolID := fmt.Sprintf("pg-extra-%d", i)
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolStart, ToolID: toolID, ToolName: "postgres",
			ToolInput: map[string]any{"query": "SELECT * FROM users"},
		})
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolDone, ToolID: toolID, ToolName: "postgres",
		})
	}

	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	lastItem := items[len(items)-1]

	if !contains(lastItem.Text, "\u00d73") {
		t.Error("expected \u00d73 dedup count for first batch of postgres calls")
		t.Logf("text: %s", lastItem.Text)
	}

	if !contains(lastItem.Text, "\u00d72") {
		t.Error("expected \u00d72 dedup count for second batch of postgres calls")
		t.Logf("text: %s", lastItem.Text)
	}

	if !contains(lastItem.Text, "config.go") {
		t.Error("expected Reading config.go in output")
		t.Logf("text: %s", lastItem.Text)
	}

	idx3 := strings.Index(lastItem.Text, "\u00d73")
	idxRead := strings.Index(lastItem.Text, "config.go")
	idx2 := strings.Index(lastItem.Text, "\u00d72")
	if idx3 >= idxRead || idxRead >= idx2 {
		t.Errorf("tool entries should be interleaved in order: \u00d73 (at %d) < config.go (at %d) < \u00d72 (at %d)",
			idx3, idxRead, idx2)
		t.Logf("text: %s", lastItem.Text)
	}
}

// TestCoalescer_InlineToolOrdering verifies that tool summaries appear inline
// between text segments rather than all collected at the end.
func TestCoalescer_InlineToolOrdering(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-inline", "cat", "C400", "ts400", "/workspace/inline",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Text, then tool, then more text.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Let me check the file.\n"})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t1", ToolName: "Read",
		ToolInput: map[string]any{"file_path": "/workspace/main.go"},
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t1", ToolName: "Read",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "The file looks good.\n"})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	lastItem := items[len(items)-1]

	// The tool should appear BETWEEN the two text segments.
	idxCheck := strings.Index(lastItem.Text, "check the file")
	idxTool := strings.Index(lastItem.Text, "main.go")
	idxGood := strings.Index(lastItem.Text, "looks good")

	if idxCheck < 0 || idxTool < 0 || idxGood < 0 {
		t.Fatalf("missing expected content in output: check=%d tool=%d good=%d\ntext: %s",
			idxCheck, idxTool, idxGood, lastItem.Text)
	}

	if !(idxCheck < idxTool && idxTool < idxGood) {
		t.Errorf("tool should be inline between text segments: check(%d) < tool(%d) < good(%d)",
			idxCheck, idxTool, idxGood)
		t.Logf("text: %s", lastItem.Text)
	}
}

// TestCoalescer_LateOnPostedAfterOverflowRollover verifies that a delayed
// OnPosted callback from the first progress message does not repoint
// progressMessageTS after a same-turn overflow rollover started a new message.
// Regression test for the turnID-only guard missing same-turn rollovers.
func TestCoalescer_LateOnPostedAfterOverflowRollover(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-rollover", "ant", "C777", "ts77", "/workspace/roll",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Drive enough unique tool calls to trigger at least one overflow split.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Starting...\n"})
	for i := 0; i < 150; i++ {
		toolID := fmt.Sprintf("tool-%d", i)
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolStart, ToolID: toolID, ToolName: "Read",
			ToolInput: map[string]any{"file_path": fmt.Sprintf("/workspace/handler_%d.go", i)},
		})
		coal.HandleEvent(agent.Event{
			Type: agent.EventToolDone, ToolID: toolID, ToolName: "Read",
		})
	}
	// After the overflow rollover, progressMessageTS was reset to nil. Send a
	// MessageEnd to force a flush that PostMessages the second (current)
	// message. Note: deliberately do NOT send Done, so the turn stays in
	// progress and progressMessageTS reflects the current message — exactly the
	// state a late callback from the first message could corrupt.
	coal.HandleEvent(agent.Event{Type: agent.EventTextDelta, Text: "Continuing after rollover."})
	coal.HandleEvent(agent.Event{Type: agent.EventMessageEnd})

	// Wait for async OnPosted callbacks to settle.
	time.Sleep(200 * time.Millisecond)

	// Collect all PostMessage items (each overflow rollover posts a new one).
	var posts []*outbound.OutboundItem
	for _, item := range out.getItems() {
		if item.Action == outbound.ActionPostMessage && item.OnPosted != nil {
			posts = append(posts, item)
		}
	}
	if len(posts) < 2 {
		t.Fatalf("expected at least 2 PostMessage items (overflow rollover), got %d", len(posts))
	}

	// Record the current TS, then replay the FIRST post's (now-stale) OnPosted
	// callback as if it arrived late. It captured the turnID of the first
	// message; after the overflow rollover bumped turnID, the guard must reject
	// it and leave progressMessageTS unchanged.
	before := coal.ProgressMessageTS()
	posts[0].OnPosted("stale-ts-from-first-message")
	after := coal.ProgressMessageTS()

	if after == nil || *after != *before {
		var b, a string
		if before != nil {
			b = *before
		}
		if after != nil {
			a = *after
		}
		t.Fatalf("late OnPosted from first message overwrote TS: before=%q after=%q", b, a)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestFormatCountdown(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{9*time.Minute + 30*time.Second, "9m 30s"},
		{1*time.Minute + 5*time.Second, "1m 5s"},
		{45 * time.Second, "45s"},
		{10 * time.Second, "10s"},
		{0, "0s"},
		{1*time.Hour + 5*time.Minute, "1h 5m"},
		{2 * time.Hour, "2h 0m"},
	}
	for _, tt := range tests {
		got := formatCountdown(tt.d)
		if got != tt.want {
			t.Errorf("formatCountdown(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestStartCountdownSetsTarget(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-cd", "timer", "C999", "ts9", "/workspace/countdown",
		Identity{DisplayName: "Test"}, out, nil)
	defer coal.Close()

	input := map[string]any{
		"delaySeconds": float64(120),
		"reason":       "waiting for build",
	}

	coal.mu.Lock()
	coal.startCountdown(input)
	coal.mu.Unlock()

	coal.mu.Lock()
	defer coal.mu.Unlock()

	if coal.countdownTarget == nil {
		t.Fatal("expected countdownTarget to be set")
	}
	remaining := time.Until(*coal.countdownTarget)
	if remaining < 115*time.Second || remaining > 125*time.Second {
		t.Errorf("expected ~120s remaining, got %v", remaining)
	}
	if coal.countdownElapsed {
		t.Error("expected countdownElapsed to be false")
	}
}

// ---------------------------------------------------------------------------
// ToolInputDelta routing: empty ID (jcode) vs non-empty ID (claude)
// ---------------------------------------------------------------------------

func TestCoalescer_ToolInputDelta_EmptyID_JcodeCompat(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-input-jcode", "fox", "C500", "ts500", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Start a tool, send input with empty ID (jcode path), then exec.
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{
		Type:        agent.EventToolInputDelta,
		ToolID:      "", // jcode: empty ID
		PartialJSON: `{"command":"go test"}`,
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolExec, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	// Should have parsed the input and generated a description from it.
	found := false
	for _, item := range items {
		if contains(item.Text, "Go tests") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected parsed tool input description (jcode empty-ID path)")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}

func TestCoalescer_ToolInputDelta_WithID_ClaudePath(t *testing.T) {
	out := &mockOutbound{}
	coal := NewSessionCoalescer("sess-input-claude", "fox", "C501", "ts501", "/workspace",
		Identity{DisplayName: "Worker"}, out, nil)
	defer coal.Close()

	// Start a tool, send input with explicit ID (claude path), then exec.
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolStart, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{
		Type:        agent.EventToolInputDelta,
		ToolID:      "t1", // claude: explicit ID
		PartialJSON: `{"command":"go test"}`,
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolExec, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{
		Type: agent.EventToolDone, ToolID: "t1", ToolName: "Bash",
	})
	coal.HandleEvent(agent.Event{Type: agent.EventTurnDone})

	time.Sleep(200 * time.Millisecond)

	items := out.getItems()
	if len(items) == 0 {
		t.Fatal("expected outbound items")
	}

	// Should have the same result as the jcode path.
	found := false
	for _, item := range items {
		if contains(item.Text, "Go tests") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected parsed tool input description (claude ID path)")
		for _, item := range items {
			t.Logf("  item text: %s", item.Text)
		}
	}
}
