package jcodeadapter

import (
	"encoding/json"
	"testing"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/jcodeproto"
)

func rawEvent(t *testing.T, evType string, data interface{}) *jcodeproto.ServerEvent {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return &jcodeproto.ServerEvent{Type: evType, Raw: raw}
}

func TestTranslate_TextDelta(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "Hello world",
	})
	result := Translate(ev)
	if len(result) != 1 {
		t.Fatalf("expected 1 event, got %d", len(result))
	}
	if result[0].Type != agent.EventTextDelta {
		t.Errorf("expected EventTextDelta, got %v", result[0].Type)
	}
	if result[0].Text != "Hello world" {
		t.Errorf("expected text 'Hello world', got %q", result[0].Text)
	}
}

func TestTranslate_TextReplace(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventTextReplace, map[string]string{
		"type": "text_replace", "text": "Replaced",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventTextReplace || result[0].Text != "Replaced" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestTranslate_ToolStart(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventToolStart, map[string]interface{}{
		"type": "tool_start", "id": "t1", "name": "Read",
		"input": map[string]interface{}{"file_path": "/tmp/foo.go"},
	})
	result := Translate(ev)
	if len(result) != 1 {
		t.Fatalf("expected 1 event, got %d", len(result))
	}
	if result[0].Type != agent.EventToolStart {
		t.Errorf("expected EventToolStart, got %v", result[0].Type)
	}
	if result[0].ToolID != "t1" || result[0].ToolName != "Read" {
		t.Errorf("unexpected tool: id=%q name=%q", result[0].ToolID, result[0].ToolName)
	}
	if result[0].ToolInput["file_path"] != "/tmp/foo.go" {
		t.Errorf("unexpected input: %v", result[0].ToolInput)
	}
}

func TestTranslate_ToolInput_EmptyID(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventToolInput, map[string]string{
		"type": "tool_input", "delta": `{"key":"val"}`,
	})
	result := Translate(ev)
	if len(result) != 1 {
		t.Fatalf("expected 1 event, got %d", len(result))
	}
	if result[0].Type != agent.EventToolInputDelta {
		t.Errorf("expected EventToolInputDelta, got %v", result[0].Type)
	}
	// jcode tool_input has no ID -- ToolID must be empty.
	if result[0].ToolID != "" {
		t.Errorf("expected empty ToolID for jcode tool_input, got %q", result[0].ToolID)
	}
	if result[0].PartialJSON != `{"key":"val"}` {
		t.Errorf("unexpected delta: %q", result[0].PartialJSON)
	}
}

func TestTranslate_ToolExec(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventToolExec, map[string]string{
		"type": "tool_exec", "id": "t1", "name": "Read",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventToolExec {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].ToolID != "t1" || result[0].ToolName != "Read" {
		t.Errorf("unexpected tool: id=%q name=%q", result[0].ToolID, result[0].ToolName)
	}
}

func TestTranslate_ToolDone(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t1", "name": "Read", "output": "contents",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventToolDone {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].IsError {
		t.Error("expected IsError=false")
	}
}

func TestTranslate_ToolDone_WithError(t *testing.T) {
	errMsg := "file not found"
	ev := rawEvent(t, jcodeproto.EventToolDone, map[string]interface{}{
		"type": "tool_done", "id": "t1", "name": "Read",
		"output": "", "error": errMsg,
	})
	result := Translate(ev)
	if len(result) != 1 || !result[0].IsError {
		t.Errorf("expected IsError=true, got %+v", result)
	}
}

func TestTranslate_TurnDone(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventDone, map[string]interface{}{
		"type": "done", "id": float64(1),
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventTurnDone {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestTranslate_TurnError(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventError, map[string]interface{}{
		"type": "error", "id": float64(1), "message": "rate limited",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventTurnError {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].ErrorMessage != "rate limited" {
		t.Errorf("expected error message 'rate limited', got %q", result[0].ErrorMessage)
	}
}

func TestTranslate_Interrupted(t *testing.T) {
	ev := &jcodeproto.ServerEvent{Type: jcodeproto.EventInterrupted, Raw: json.RawMessage(`{"type":"interrupted"}`)}
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventInterrupted {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestTranslate_MessageEnd(t *testing.T) {
	ev := &jcodeproto.ServerEvent{Type: jcodeproto.EventMessageEnd, Raw: json.RawMessage(`{"type":"message_end"}`)}
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventMessageEnd {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestTranslate_SessionReady_SwarmStatus(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventSwarmStatus, map[string]interface{}{
		"type": "swarm_status",
		"members": []interface{}{
			map[string]interface{}{
				"session_id":    "session_fox_123",
				"friendly_name": "fox",
				"status":        "ready",
			},
		},
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventSessionReady {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].SessionID != "session_fox_123" {
		t.Errorf("expected session_id 'session_fox_123', got %q", result[0].SessionID)
	}
}

func TestTranslate_Provider(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventUpstreamProvider, map[string]string{
		"type": "upstream_provider", "provider": "claude-opus-4",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventProvider {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].ProviderName != "claude-opus-4" {
		t.Errorf("expected provider 'claude-opus-4', got %q", result[0].ProviderName)
	}
}

func TestTranslate_GeneratedImage(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventGeneratedImage, map[string]interface{}{
		"type": "generated_image", "id": "img1",
		"path": "/tmp/image.png", "output_format": "png",
		"revised_prompt": "a cute cat",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventImageGenerated {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].ImagePath != "/tmp/image.png" {
		t.Errorf("unexpected path: %q", result[0].ImagePath)
	}
	if result[0].ImageCaption != "a cute cat" {
		t.Errorf("unexpected caption: %q", result[0].ImageCaption)
	}
}

func TestTranslate_Notification(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventNotification, map[string]interface{}{
		"type":              "notification",
		"from_session":      "session_fox",
		"from_name":         "Fox Agent",
		"notification_type": map[string]string{"kind": "info"},
		"message":           "task complete",
	})
	result := Translate(ev)
	if len(result) != 1 || result[0].Type != agent.EventNotification {
		t.Errorf("unexpected result: %+v", result)
	}
	if result[0].NotificationFrom != "Fox Agent" {
		t.Errorf("unexpected from: %q", result[0].NotificationFrom)
	}
	if result[0].NotificationMsg != "task complete" {
		t.Errorf("unexpected msg: %q", result[0].NotificationMsg)
	}
}

func TestTranslate_InfraEvents_Dropped(t *testing.T) {
	// Infrastructure events should return nil (silently dropped).
	for _, evType := range []string{
		jcodeproto.EventAck, jcodeproto.EventPong,
		jcodeproto.EventTokens, jcodeproto.EventConnectionType,
		jcodeproto.EventReloading,
	} {
		ev := &jcodeproto.ServerEvent{Type: evType, Raw: json.RawMessage(`{"type":"` + evType + `"}`)}
		result := Translate(ev)
		if len(result) != 0 {
			t.Errorf("expected infra event %q to be dropped, got %+v", evType, result)
		}
	}
}

func TestTranslate_History_Dropped(t *testing.T) {
	ev := rawEvent(t, jcodeproto.EventHistory, map[string]interface{}{
		"type":       "history",
		"session_id": "session_fox",
		"id":         float64(1),
		"messages":   []interface{}{},
	})
	result := Translate(ev)
	if len(result) != 0 {
		t.Errorf("expected history event to be dropped, got %+v", result)
	}
}
