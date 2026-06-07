package claude

import (
	"bufio"
	"os"
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
// Real persistent-mode schema (no --include-partial-messages):
// text and tools arrive as FULL `assistant` messages, NOT stream_event deltas.
// Verified by capture against the real CLI (testdata/real_session.ndjson).
// ---------------------------------------------------------------------------

func TestSessionReady(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"c5714ea1-f992-45a8-96d0-9abcc2b845a1","model":"claude-sonnet-4-20250514","cwd":"/home/user","permissionMode":"default","apiKeySource":"none"}`,
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

func TestAssistantText(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"HELLO-ONE"}]},"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventTextDelta,
		agent.EventMessageEnd,
	})
	if events[0].Text != "HELLO-ONE" {
		t.Errorf("Text = %q, want HELLO-ONE", events[0].Text)
	}
}

func TestAssistantThinkingDropped(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think..."}]},"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	// Thinking produces no text; the message still ends with MessageEnd.
	requireEventTypes(t, events, []agent.EventType{agent.EventMessageEnd})
}

func TestAssistantEmptyTextDropped(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":""}]},"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventMessageEnd})
}

func TestAssistantToolUse(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01KZ","name":"Bash","input":{"command":"echo hello-from-bash","description":"echo"}}]},"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart,
		agent.EventToolExec,
		agent.EventMessageEnd,
	})
	if events[0].ToolID != "toolu_01KZ" || events[0].ToolName != "Bash" {
		t.Errorf("ToolStart id=%q name=%q", events[0].ToolID, events[0].ToolName)
	}
	if events[0].ToolInput["command"] != "echo hello-from-bash" {
		t.Errorf("ToolStart.ToolInput[command] = %v", events[0].ToolInput["command"])
	}
	if events[1].ToolID != "toolu_01KZ" || events[1].ToolName != "Bash" {
		t.Errorf("ToolExec id=%q name=%q", events[1].ToolID, events[1].ToolName)
	}
}

func TestToolResult(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01KZ","name":"Bash","input":{"command":"echo hi"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01KZ","type":"tool_result","content":"hi","is_error":false}]},"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart,
		agent.EventToolExec,
		agent.EventMessageEnd,
		agent.EventToolDone,
	})
	done := events[3]
	if done.ToolID != "toolu_01KZ" || done.ToolName != "Bash" {
		t.Errorf("ToolDone id=%q name=%q (name must be threaded from tool_use)", done.ToolID, done.ToolName)
	}
	if done.IsError {
		t.Error("ToolDone.IsError should be false")
	}
}

func TestToolResultError(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_err","name":"Write","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_err","type":"tool_result","content":"permission denied","is_error":true}]}}`,
	}
	events := collectEvents(t, lines)
	last := events[len(events)-1]
	if last.Type != agent.EventToolDone || !last.IsError {
		t.Errorf("expected ToolDone with IsError=true, got %+v", last)
	}
}

func TestReplayedUserSkipped(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"Reply with exactly: HELLO-ONE"},"isReplay":true,"session_id":"s1"}`,
	}
	events := collectEvents(t, lines)
	if len(events) != 0 {
		t.Errorf("replayed user string message should produce no events, got %d: %+v", len(events), events)
	}
}

func TestTurnDoneNoDuplicateText(t *testing.T) {
	// The `result` line carries result:"HELLO-ONE" which repeats the last
	// assistant text. The translator MUST NOT emit it as text (duplicate-text bug).
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"HELLO-ONE"}]}}`,
		`{"type":"result","subtype":"success","result":"HELLO-ONE","session_id":"s1","usage":{"input_tokens":10,"output_tokens":7}}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventTextDelta,
		agent.EventMessageEnd,
		agent.EventTurnDone,
	})
	// Exactly one text event, carrying the text once.
	textCount := 0
	for _, e := range events {
		if e.Type == agent.EventTextDelta {
			textCount++
		}
	}
	if textCount != 1 {
		t.Errorf("expected exactly 1 text event (no duplicate from result), got %d", textCount)
	}
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

func TestTurnErrorInequalityNotAllowList(t *testing.T) {
	// Any non-"success" subtype must map to TurnError (inequality, not enum).
	lines := []string{
		`{"type":"result","subtype":"error_max_budget_usd"}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{agent.EventTurnError})
	if events[0].ErrorMessage == "" {
		t.Error("expected non-empty error message for non-success result")
	}
}

func TestToolBlockOrderingInvariant(t *testing.T) {
	// Two tool_use blocks in one assistant message: each ToolStart must be
	// immediately followed by its ToolExec — never two open tool blocks.
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"Read","input":{}},{"type":"tool_use","id":"toolu_B","name":"Bash","input":{}}]}}`,
	}
	events := collectEvents(t, lines)
	requireEventTypes(t, events, []agent.EventType{
		agent.EventToolStart, // A
		agent.EventToolExec,  // A
		agent.EventToolStart, // B
		agent.EventToolExec,  // B
		agent.EventMessageEnd,
	})
	if events[0].ToolID != "toolu_A" || events[1].ToolID != "toolu_A" {
		t.Errorf("block A not Start->Exec contiguous: %+v", events[:2])
	}
	if events[2].ToolID != "toolu_B" || events[3].ToolID != "toolu_B" {
		t.Errorf("block B not Start->Exec contiguous: %+v", events[2:4])
	}
}

func TestMultipleToolResultsThreaded(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"Read","input":{}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_B","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A","type":"tool_result","content":"data","is_error":false},{"tool_use_id":"toolu_B","type":"tool_result","content":"out","is_error":false}]}}`,
	}
	events := collectEvents(t, lines)
	var dones []agent.Event
	for _, e := range events {
		if e.Type == agent.EventToolDone {
			dones = append(dones, e)
		}
	}
	if len(dones) != 2 {
		t.Fatalf("expected 2 ToolDone, got %d", len(dones))
	}
	if dones[0].ToolName != "Read" {
		t.Errorf("ToolDone[0].ToolName = %q, want Read", dones[0].ToolName)
	}
	if dones[1].ToolName != "Bash" {
		t.Errorf("ToolDone[1].ToolName = %q, want Bash", dones[1].ToolName)
	}
}

func TestToolNamesEvictedAfterDone(t *testing.T) {
	// In a long-running persistent session the toolNames map must not grow
	// unbounded — each entry is dropped once its ToolDone is emitted.
	tr := newTranslator()
	tr.translateLine([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}`))
	if _, ok := tr.toolNames["toolu_1"]; !ok {
		t.Fatal("toolNames should contain toolu_1 after tool_use")
	}
	tr.translateLine([]byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"ok","is_error":false}]}}`))
	if _, ok := tr.toolNames["toolu_1"]; ok {
		t.Error("toolNames[toolu_1] should be evicted after ToolDone")
	}
}

func TestSystemNonInitSkipped(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"thinking_tokens","count":100}`,
		`{"type":"system","subtype":"hook_started","hook_id":"h1"}`,
	}
	events := collectEvents(t, lines)
	if len(events) != 0 {
		t.Errorf("non-init system events should be skipped, got %d", len(events))
	}
}

func TestRateLimitEventSkipped(t *testing.T) {
	events := collectEvents(t, []string{`{"type":"rate_limit_event","remaining":100}`})
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestEmptyLines(t *testing.T) {
	tr := newTranslator()
	if evs := tr.translateLine([]byte("")); len(evs) != 0 {
		t.Errorf("empty line: got %d events", len(evs))
	}
	if evs := tr.translateLine(nil); len(evs) != 0 {
		t.Errorf("nil line: got %d events", len(evs))
	}
}

func TestUnknownEventTypes(t *testing.T) {
	events := collectEvents(t, []string{`{"type":"unknown_future_event","data":"x"}`})
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

// TestRealSessionReplay replays the captured real CLI session and asserts the
// authoritative properties: a SessionReady, real assistant text (the empty-text
// bug is gone), tool lifecycle present, exactly 3 TurnDone (3 turns), and no
// TurnError.
func TestRealSessionReplay(t *testing.T) {
	f, err := os.Open("testdata/real_session.ndjson")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	tr := newTranslator()
	var events []agent.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		events = append(events, tr.translateLine(sc.Bytes())...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	var sessionReady, turnDone, turnErr, textDeltas, toolStart, toolDone int
	var sawText bool
	for _, e := range events {
		switch e.Type {
		case agent.EventSessionReady:
			sessionReady++
		case agent.EventTurnDone:
			turnDone++
		case agent.EventTurnError:
			turnErr++
		case agent.EventTextDelta:
			textDeltas++
			if e.Text != "" {
				sawText = true
			}
		case agent.EventToolStart:
			toolStart++
		case agent.EventToolDone:
			toolDone++
		}
	}
	if sessionReady < 1 {
		t.Error("expected at least one SessionReady")
	}
	if !sawText {
		t.Error("expected non-empty assistant text (empty-response bug regression)")
	}
	if turnDone != 3 {
		t.Errorf("expected 3 TurnDone (3 turns), got %d", turnDone)
	}
	if turnErr != 0 {
		t.Errorf("expected 0 TurnError, got %d", turnErr)
	}
	if toolStart < 1 || toolDone < 1 {
		t.Errorf("expected tool lifecycle (start=%d done=%d)", toolStart, toolDone)
	}
}
