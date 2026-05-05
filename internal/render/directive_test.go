package render

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Directive extraction tests
// ---------------------------------------------------------------------------

func TestExtractDirectives_NoDirectives(t *testing.T) {
	text := "Hello world, this is normal text with no directives."
	result := ExtractDirectives(text, false)
	if result.CleanText != text {
		t.Errorf("CleanText mismatch:\ngot:  %q\nwant: %q", result.CleanText, text)
	}
	if len(result.Blocks) != 0 {
		t.Errorf("expected no blocks, got %d", len(result.Blocks))
	}
}

func TestExtractDirectives_PlainCodeBlock(t *testing.T) {
	// Plain code blocks (no language tag or other languages) must NOT be intercepted
	text := "Here's some code:\n```\nfoo bar\n```\nand more text"
	result := ExtractDirectives(text, false)
	if result.CleanText != text {
		t.Errorf("Plain code block was modified:\ngot:  %q\nwant: %q", result.CleanText, text)
	}
}

func TestExtractDirectives_PythonCodeBlock(t *testing.T) {
	text := "Here's Python:\n```python\ndef hello():\n    pass\n```\nend"
	result := ExtractDirectives(text, false)
	if result.CleanText != text {
		t.Errorf("Python code block was modified:\ngot:  %q\nwant: %q", result.CleanText, text)
	}
}

func TestExtractDirectives_ValidPlanDirective(t *testing.T) {
	text := `Before text.
` + "```switchboard\n" + `{
  "render": "plan",
  "title": "Deploy to prod",
  "tasks": [
    {"id": "1", "title": "Build image", "status": "complete"},
    {"id": "2", "title": "Run tests", "status": "in_progress"},
    {"id": "3", "title": "Deploy", "status": "pending"}
  ]
}
` + "```" + `
After text.`

	result := ExtractDirectives(text, false)

	// Directive should be removed from text
	if strings.Contains(result.CleanText, "switchboard") {
		t.Error("directive was not removed from clean text")
	}
	if !strings.Contains(result.CleanText, "Before text.") {
		t.Error("text before directive was lost")
	}
	if !strings.Contains(result.CleanText, "After text.") {
		t.Error("text after directive was lost")
	}

	// Should produce blocks
	if len(result.Blocks) == 0 {
		t.Fatal("expected blocks from plan directive")
	}

	// Check fallback text
	if result.FallbackText == "" {
		t.Error("expected fallback text")
	}
	if !strings.Contains(result.FallbackText, "Deploy to prod") {
		t.Errorf("fallback should mention plan title, got: %q", result.FallbackText)
	}
}

func TestExtractDirectives_UnknownDirective(t *testing.T) {
	text := "Before\n```switchboard\n{\"render\": \"invented\", \"data\": 123}\n```\nAfter"
	result := ExtractDirectives(text, false)

	// Unknown directive should be LEFT in text (non-strict mode)
	if !strings.Contains(result.CleanText, "switchboard") {
		t.Error("unknown directive was removed - should be left as visible code block")
	}
	if !strings.Contains(result.CleanText, "invented") {
		t.Error("unknown directive content was removed")
	}
	if len(result.Blocks) != 0 {
		t.Errorf("unknown directive produced %d blocks, want 0", len(result.Blocks))
	}
}

func TestExtractDirectives_InvalidJSON(t *testing.T) {
	text := "Before\n```switchboard\n{not valid json\n```\nAfter"
	result := ExtractDirectives(text, false)

	// Invalid JSON should be left in text
	if !strings.Contains(result.CleanText, "not valid json") {
		t.Error("invalid directive was removed from text")
	}
}

func TestExtractDirectives_MissingRenderField(t *testing.T) {
	text := "Before\n```switchboard\n{\"title\": \"no render field\"}\n```\nAfter"
	result := ExtractDirectives(text, false)

	// Missing render field = invalid, leave in text
	if !strings.Contains(result.CleanText, "no render field") {
		t.Error("directive without render field was removed")
	}
}

func TestExtractDirectives_UnsupportedVersion(t *testing.T) {
	text := `Before
` + "```switchboard\n" + `{"render": "plan", "version": 99, "title": "X", "tasks": [{"id":"1","title":"T","status":"pending"}]}
` + "```" + `
After`
	result := ExtractDirectives(text, false)

	// Unsupported version = leave in text
	if !strings.Contains(result.CleanText, "version") {
		t.Error("unsupported version directive was removed")
	}
}

func TestExtractDirectives_StrictMode_DropsInvalid(t *testing.T) {
	text := "Before\n```switchboard\n{\"render\": \"invented\"}\n```\nAfter"
	result := ExtractDirectives(text, true)

	// Strict mode: invalid directives are dropped (not left in text)
	if strings.Contains(result.CleanText, "invented") {
		t.Error("strict mode should drop invalid directives, but it was left in text")
	}
}

func TestExtractDirectives_MultipleDirectives(t *testing.T) {
	text := `Start.
` + "```switchboard\n" + `{"render": "plan", "title": "A", "tasks": [{"id":"1","title":"T1","status":"complete"}]}
` + "```" + `
Middle text.
` + "```switchboard\n" + `{"render": "todos", "items": [{"text": "Buy milk", "done": false}]}
` + "```" + `
End.`

	result := ExtractDirectives(text, false)

	if strings.Contains(result.CleanText, "switchboard") {
		t.Error("valid directives not removed from text")
	}
	if !strings.Contains(result.CleanText, "Start.") {
		t.Error("start text lost")
	}
	if !strings.Contains(result.CleanText, "Middle text.") {
		t.Error("middle text lost")
	}
	if !strings.Contains(result.CleanText, "End.") {
		t.Error("end text lost")
	}

	// Should have blocks from both directives
	if len(result.Blocks) < 2 {
		t.Errorf("expected blocks from 2 directives, got %d blocks", len(result.Blocks))
	}
}

func TestExtractDirectives_PlanMissingTasks(t *testing.T) {
	text := "```switchboard\n{\"render\": \"plan\", \"title\": \"X\"}\n```"
	result := ExtractDirectives(text, false)

	// Plan without tasks is invalid - should be left in text
	if !strings.Contains(result.CleanText, "plan") {
		t.Error("invalid plan (missing tasks) was removed")
	}
}

// ---------------------------------------------------------------------------
// Plan renderer tests
// ---------------------------------------------------------------------------

func TestRenderPlan_Valid(t *testing.T) {
	data := `{
		"render": "plan",
		"title": "Migrate DB",
		"tasks": [
			{"id": "1", "title": "Schema dump", "status": "complete"},
			{"id": "2", "title": "Apply POC", "status": "in_progress"},
			{"id": "3", "title": "Verify", "status": "pending"}
		]
	}`

	blocks, fallback, err := renderPlanDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(blocks) < 2 {
		t.Fatalf("expected at least 2 blocks, got %d", len(blocks))
	}

	// First block should be header
	if blocks[0]["type"] != "header" {
		t.Errorf("first block type = %v, want header", blocks[0]["type"])
	}

	// Verify fallback
	if !strings.Contains(fallback, "Migrate DB") {
		t.Errorf("fallback missing title: %q", fallback)
	}
	if !strings.Contains(fallback, "1/3") {
		t.Errorf("fallback missing progress: %q", fallback)
	}
}

// ---------------------------------------------------------------------------
// Brief renderer tests
// ---------------------------------------------------------------------------

func TestRenderBrief_Valid(t *testing.T) {
	data := `{
		"render": "brief",
		"title": "Auth System Analysis",
		"summary": "The auth system uses JWT tokens with 24h expiry.",
		"sources": [
			{"title": "RFC 7519", "url": "https://tools.ietf.org/html/rfc7519", "excerpt": "JWT spec"},
			{"title": "Auth docs", "url": "https://example.com/docs"}
		]
	}`

	blocks, fallback, err := renderBriefDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(blocks) < 3 {
		t.Fatalf("expected at least 3 blocks (header + summary + sources), got %d", len(blocks))
	}

	if !strings.Contains(fallback, "Auth System Analysis") {
		t.Errorf("fallback missing title: %q", fallback)
	}
}

func TestRenderBrief_NoSources(t *testing.T) {
	data := `{"render": "brief", "title": "Quick Note", "summary": "Short summary."}`
	blocks, _, err := renderBriefDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still render without sources (header + section)
	if len(blocks) < 2 {
		t.Errorf("expected at least 2 blocks, got %d", len(blocks))
	}
}

// ---------------------------------------------------------------------------
// Poll renderer tests
// ---------------------------------------------------------------------------

func TestRenderPoll_Valid(t *testing.T) {
	data := `{
		"render": "poll",
		"question": "Which framework?",
		"options": [
			{"text": "React", "id": "react"},
			{"text": "Vue", "id": "vue"},
			{"text": "Svelte", "id": "svelte"}
		]
	}`

	blocks, fallback, err := renderPollDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// section + divider + 3 option blocks = 5
	if len(blocks) < 5 {
		t.Fatalf("expected at least 5 blocks, got %d", len(blocks))
	}

	if !strings.Contains(fallback, "Which framework?") {
		t.Errorf("fallback missing question: %q", fallback)
	}
	if !strings.Contains(fallback, "3 options") {
		t.Errorf("fallback missing option count: %q", fallback)
	}
}

// ---------------------------------------------------------------------------
// Tickets renderer tests
// ---------------------------------------------------------------------------

func TestRenderTickets_Valid(t *testing.T) {
	data := `{
		"render": "tickets",
		"title": "Sprint 42",
		"tickets": [
			{"id": "PROJ-101", "title": "Fix login bug", "status": "open", "assignee": "leigh"},
			{"id": "PROJ-102", "title": "Add dark mode", "status": "in_progress", "priority": "high"},
			{"id": "PROJ-103", "title": "Update deps", "status": "closed"}
		]
	}`

	blocks, fallback, err := renderTicketsDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// header + 3 ticket sections = 4
	if len(blocks) < 4 {
		t.Fatalf("expected at least 4 blocks, got %d", len(blocks))
	}

	if !strings.Contains(fallback, "Sprint 42") {
		t.Errorf("fallback missing title: %q", fallback)
	}
	if !strings.Contains(fallback, "3 items") {
		t.Errorf("fallback missing count: %q", fallback)
	}
}

func TestRenderTickets_NoTitle(t *testing.T) {
	data := `{"render": "tickets", "tickets": [{"id": "1", "title": "Fix", "status": "open"}]}`
	blocks, _, err := renderTicketsDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No header block when title is empty
	if len(blocks) == 0 {
		t.Error("expected at least 1 block")
	}
	// First block should be section (no header)
	if blocks[0]["type"] == "header" {
		t.Error("should not have header when title is empty")
	}
}

// ---------------------------------------------------------------------------
// Todos renderer tests
// ---------------------------------------------------------------------------

func TestRenderTodos_Valid(t *testing.T) {
	data := `{
		"render": "todos",
		"title": "Shopping",
		"items": [
			{"text": "Buy milk", "done": true},
			{"text": "Buy eggs", "done": false},
			{"text": "Buy bread", "done": false}
		]
	}`

	blocks, fallback, err := renderTodosDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(blocks) < 3 {
		t.Fatalf("expected at least 3 blocks, got %d", len(blocks))
	}

	if !strings.Contains(fallback, "Shopping") {
		t.Errorf("fallback missing title: %q", fallback)
	}
	if !strings.Contains(fallback, "1/3 done") {
		t.Errorf("fallback missing progress: %q", fallback)
	}
}

func TestRenderTodos_DefaultTitle(t *testing.T) {
	data := `{"render": "todos", "items": [{"text": "Item", "done": false}]}`
	blocks, fallback, err := renderTodosDirective([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) == 0 {
		t.Error("expected blocks")
	}
	if !strings.Contains(fallback, "To-do") {
		t.Errorf("fallback missing default title: %q", fallback)
	}
}

// ---------------------------------------------------------------------------
// Alert renderer tests
// ---------------------------------------------------------------------------

func TestRenderAlert_Error(t *testing.T) {
	blocks := RenderAlert(AlertError, "Connection failed")
	if len(blocks) == 0 {
		t.Fatal("expected blocks")
	}
	// Verify block content
	text := extractBlockText(blocks[0])
	if !strings.Contains(text, "❌") {
		t.Errorf("error alert missing ❌ emoji: %q", text)
	}
	if !strings.Contains(text, "Connection failed") {
		t.Errorf("alert missing message: %q", text)
	}
}

func TestRenderAlert_Warning(t *testing.T) {
	blocks := RenderAlert(AlertWarning, "Rate limited")
	text := extractBlockText(blocks[0])
	if !strings.Contains(text, "⚠️") {
		t.Errorf("warning alert missing ⚠️ emoji: %q", text)
	}
}

func TestRenderAlert_Success(t *testing.T) {
	blocks := RenderAlert(AlertSuccess, "Deploy complete")
	text := extractBlockText(blocks[0])
	if !strings.Contains(text, "✅") {
		t.Errorf("success alert missing ✅ emoji: %q", text)
	}
}

func TestAlertFallbackText(t *testing.T) {
	fb := AlertFallbackText(AlertError, "Something went wrong")
	if !strings.Contains(fb, "[ERROR]") {
		t.Errorf("fallback missing level: %q", fb)
	}
	if !strings.Contains(fb, "Something went wrong") {
		t.Errorf("fallback missing message: %q", fb)
	}
}

// ---------------------------------------------------------------------------
// HasDirectives tests
// ---------------------------------------------------------------------------

func TestHasDirectives_True(t *testing.T) {
	text := "blah\n```switchboard\n{}\n```\nmore"
	if !HasDirectives(text) {
		t.Error("HasDirectives should return true")
	}
}

func TestHasDirectives_False(t *testing.T) {
	text := "blah\n```python\ncode\n```\nmore"
	if HasDirectives(text) {
		t.Error("HasDirectives should return false for python block")
	}
}

func TestHasDirectives_EmptyText(t *testing.T) {
	if HasDirectives("") {
		t.Error("HasDirectives should return false for empty text")
	}
}

// ---------------------------------------------------------------------------
// Golden file style: verify block JSON structure is valid
// ---------------------------------------------------------------------------

func TestBlocksAreValidJSON(t *testing.T) {
	directives := []string{
		`{"render":"plan","title":"T","tasks":[{"id":"1","title":"A","status":"complete"}]}`,
		`{"render":"brief","title":"B","summary":"S"}`,
		`{"render":"poll","question":"Q?","options":[{"text":"O1"},{"text":"O2"}]}`,
		`{"render":"tickets","tickets":[{"id":"1","title":"T","status":"open"}]}`,
		`{"render":"todos","items":[{"text":"I","done":false}]}`,
	}

	for _, d := range directives {
		text := "```switchboard\n" + d + "\n```"
		result := ExtractDirectives(text, false)
		for i, block := range result.Blocks {
			// Verify each block can be marshaled to valid JSON
			data, err := json.Marshal(block)
			if err != nil {
				t.Errorf("directive %s: block %d marshal error: %v", d[:20], i, err)
			}
			// Verify it has a "type" field
			var m map[string]interface{}
			json.Unmarshal(data, &m)
			if _, ok := m["type"]; !ok {
				t.Errorf("directive %s: block %d missing 'type' field", d[:20], i)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractBlockText(block map[string]interface{}) string {
	textObj, ok := block["text"].(map[string]interface{})
	if !ok {
		return ""
	}
	text, _ := textObj["text"].(string)
	return text
}
