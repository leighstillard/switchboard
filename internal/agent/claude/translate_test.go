package claude

import (
	"testing"

	"github.com/format5/switchboard/internal/agent"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collectEvents feeds all lines through a fresh translator and returns events.
func collectEvents(t *testing.T, lines []string) []agent.Event {
	t.Helper()
	tr := newTranslator()
	var all []agent.Event
	for _, line := range lines {
		evs := tr.translateLine([]byte(line))
		all = append(all, evs...)
	}
	return all
}

func requireEventTypes(t *testing.T, events []agent.Event, want []agent.EventType) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d\nevents: %+v", len(events), len(want), events)
	}
	for i := range want {
		if events[i].Type != want[i] {
			t.Errorf("events[%d].Type = %v, want %v", i, events[i].Type, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSessionReady(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"c5714ea1-f992-45a8-96d0-9abcc2b845a1","model":"claude-sonnet-4-20250514","cwd":"/home/user","tools":[]}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventSessionReady})

	if events[0].SessionID != "c5714ea1-f992-45a8-96d0-9abcc2b845a1" {
		t.Errorf("SessionID = %q", events[0].SessionID)
	}
	if events[0].Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q", events[0].Model)
	}
}

func TestTextStreaming(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":"message_start","message":{"id":"msg_1","type":"message"}}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventTextDelta,
		agent.EventTextDelta,
		agent.EventMessageEnd,
	})

	if events[0].Text != "Hello" {
		t.Errorf("events[0].Text = %q", events[0].Text)
	}
	if events[1].Text != " world" {
		t.Errorf("events[1].Text = %q", events[1].Text)
	}
}

func TestToolUseLifecycle(t *testing.T) {
	lines := []string{
		// Tool use block starts
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"Bash"}}`,
		// Tool input streaming
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		// Tool block stops -> ToolExec
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		// User message with tool_result -> ToolDone
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01ABC","content":"file1.txt\nfile2.txt","is_error":false}]}}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart,
		agent.EventToolInputDelta,
		agent.EventToolInputDelta,
		agent.EventToolExec,
		agent.EventToolDone,
	})

	// ToolStart
	if events[0].ToolID != "toolu_01ABC" || events[0].ToolName != "Bash" {
		t.Errorf("ToolStart: id=%q name=%q", events[0].ToolID, events[0].ToolName)
	}

	// ToolInputDelta carries the tool ID
	if events[1].ToolID != "toolu_01ABC" {
		t.Errorf("ToolInputDelta[0].ToolID = %q", events[1].ToolID)
	}
	if events[1].PartialJSON != `{"cmd":` {
		t.Errorf("ToolInputDelta[0].PartialJSON = %q", events[1].PartialJSON)
	}

	// ToolExec
	if events[3].ToolID != "toolu_01ABC" || events[3].ToolName != "Bash" {
		t.Errorf("ToolExec: id=%q name=%q", events[3].ToolID, events[3].ToolName)
	}

	// ToolDone - name threaded from ToolStart
	if events[4].ToolID != "toolu_01ABC" || events[4].ToolName != "Bash" {
		t.Errorf("ToolDone: id=%q name=%q", events[4].ToolID, events[4].ToolName)
	}
	if events[4].IsError {
		t.Error("ToolDone.IsError should be false")
	}
}

func TestToolDoneWithError(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_err","name":"Write"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_err","content":"permission denied","is_error":true}]}}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart,
		agent.EventToolExec,
		agent.EventToolDone,
	})
	if !events[2].IsError {
		t.Error("ToolDone.IsError should be true")
	}
}

func TestThinkingBlocksSkipped(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc123"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Result"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":1}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventTextDelta,
	})
	if events[0].Text != "Result" {
		t.Errorf("expected text 'Result', got %q", events[0].Text)
	}
}

func TestHookEventsSkipped(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"hook_started","hook_id":"h1"}`,
		`{"type":"system","subtype":"hook_response","hook_id":"h1"}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after hooks"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventTextDelta,
	})
}

func TestAssistantEventsSkipped(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"full message"}]}}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventTextDelta})
}

func TestMessageEnd(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":"message_stop"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventMessageEnd})
}

func TestTurnDone(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"success","cost_usd":0.001,"duration_ms":1234}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventTurnDone})
}

func TestTurnError(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"error","error":"rate limited"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventTurnError})
	if events[0].ErrorMessage != "rate limited" {
		t.Errorf("ErrorMessage = %q", events[0].ErrorMessage)
	}
}

func TestTurnErrorFallbackMessage(t *testing.T) {
	lines := []string{
		`{"type":"result","subtype":"timeout"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventTurnError})
	if events[0].ErrorMessage == "" {
		t.Error("expected non-empty error message for non-success result")
	}
}

func TestRateLimitEventSkipped(t *testing.T) {
	lines := []string{
		`{"type":"rate_limit_event","remaining":100}`,
	}
	events := collectEvents(t, lines)
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestToolBlockOrderingInvariant(t *testing.T) {
	// This tests that the translator tracks open tool blocks and asserts
	// the invariant. In practice, Anthropic's SSE is sequential so this
	// shouldn't happen, but we test the tracking is correct.
	tr := newTranslator()

	// Start first tool block
	evs := tr.translateLine([]byte(`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read"}}`))
	if len(evs) != 1 || evs[0].Type != agent.EventToolStart {
		t.Fatalf("expected ToolStart, got %+v", evs)
	}

	if !tr.hasOpenToolBlock {
		t.Error("expected hasOpenToolBlock = true after tool start")
	}

	// Stop first tool block (ToolExec)
	evs = tr.translateLine([]byte(`{"type":"stream_event","event":"content_block_stop","index":0}`))
	if len(evs) != 1 || evs[0].Type != agent.EventToolExec {
		t.Fatalf("expected ToolExec, got %+v", evs)
	}

	if tr.hasOpenToolBlock {
		t.Error("expected hasOpenToolBlock = false after tool stop")
	}

	// Start second tool block - should be fine
	evs = tr.translateLine([]byte(`{"type":"stream_event","event":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"Write"}}`))
	if len(evs) != 1 || evs[0].Type != agent.EventToolStart {
		t.Fatalf("expected ToolStart for second tool, got %+v", evs)
	}
}

func TestMultipleToolResults(t *testing.T) {
	lines := []string{
		// Two tool use blocks
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"Read"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_B","name":"Bash"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":1}`,
		// User message with both results
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_A","content":"data","is_error":false},{"type":"tool_result","tool_use_id":"toolu_B","content":"output","is_error":false}]}}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart,  // toolu_A
		agent.EventToolExec,   // toolu_A
		agent.EventToolStart,  // toolu_B
		agent.EventToolExec,   // toolu_B
		agent.EventToolDone,   // toolu_A
		agent.EventToolDone,   // toolu_B
	})

	// Verify tool names are threaded correctly
	if events[4].ToolName != "Read" {
		t.Errorf("ToolDone[0].ToolName = %q, want Read", events[4].ToolName)
	}
	if events[5].ToolName != "Bash" {
		t.Errorf("ToolDone[1].ToolName = %q, want Bash", events[5].ToolName)
	}
}

func TestFullTurnSequence(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-sonnet-4-20250514"}`,
		`{"type":"stream_event","event":"message_start","message":{"id":"msg_1","type":"message"}}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I'll check."}}`,
		`{"type":"stream_event","event":"content_block_stop","index":1}`,
		`{"type":"stream_event","event":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_X","name":"Bash"}}`,
		`{"type":"stream_event","event":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":2}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I'll check."}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_X","content":"file.go","is_error":false}]}}`,
		`{"type":"stream_event","event":"message_start","message":{"id":"msg_2","type":"message"}}`,
		`{"type":"stream_event","event":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"stream_event","event":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Found file.go"}}`,
		`{"type":"stream_event","event":"content_block_stop","index":0}`,
		`{"type":"stream_event","event":"message_stop"}`,
		`{"type":"result","subtype":"success","cost_usd":0.01}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventSessionReady,   // system/init
		agent.EventTextDelta,       // "I'll check."
		agent.EventToolStart,       // Bash
		agent.EventToolInputDelta,  // input streaming
		agent.EventToolExec,        // block stop
		agent.EventMessageEnd,      // message_stop
		agent.EventToolDone,        // tool_result
		agent.EventTextDelta,       // "Found file.go"
		agent.EventMessageEnd,      // message_stop
		agent.EventTurnDone,        // result/success
	})
}

func TestEmptyLines(t *testing.T) {
	tr := newTranslator()
	events := tr.translateLine([]byte(""))
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty line, got %d", len(events))
	}
	events = tr.translateLine(nil)
	if len(events) != 0 {
		t.Errorf("expected 0 events for nil line, got %d", len(events))
	}
}

func TestUnknownEventTypes(t *testing.T) {
	lines := []string{
		`{"type":"unknown_future_event","data":"something"}`,
	}
	events := collectEvents(t, lines)
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}
