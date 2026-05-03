package jcodeproto

import (
	"encoding/json"
	"testing"
)

func TestParseServerEvent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
		wantErr  bool
	}{
		{
			name:     "text_delta",
			input:    `{"type":"text_delta","text":"hello world"}`,
			wantType: EventTextDelta,
		},
		{
			name:     "ack",
			input:    `{"type":"ack","id":42}`,
			wantType: EventAck,
		},
		{
			name:     "done",
			input:    `{"type":"done","id":1}`,
			wantType: EventDone,
		},
		{
			name:     "session",
			input:    `{"type":"session","session_id":"abc-123"}`,
			wantType: EventSession,
		},
		{
			name:     "tool_start",
			input:    `{"type":"tool_start","id":"t1","name":"Bash"}`,
			wantType: EventToolStart,
		},
		{
			name:     "tool_done",
			input:    `{"type":"tool_done","id":"t1","name":"Bash","output":"ok"}`,
			wantType: EventToolDone,
		},
		{
			name:     "error",
			input:    `{"type":"error","id":5,"message":"rate limited","retry_after_secs":30}`,
			wantType: EventError,
		},
		{
			name:     "upstream_provider",
			input:    `{"type":"upstream_provider","provider":"gpt-4o"}`,
			wantType: EventUpstreamProvider,
		},
		{
			name:     "reloading",
			input:    `{"type":"reloading"}`,
			wantType: EventReloading,
		},
		{
			name:    "empty object",
			input:   `{}`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evType, raw, err := ParseServerEvent([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if evType != tt.wantType {
				t.Errorf("type = %q, want %q", evType, tt.wantType)
			}
			if raw == nil {
				t.Error("raw is nil")
			}
		})
	}
}

func TestUnmarshalTextDelta(t *testing.T) {
	input := `{"type":"text_delta","text":"hello world"}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev TextDeltaEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Text != "hello world" {
		t.Errorf("text = %q, want %q", ev.Text, "hello world")
	}
}

func TestUnmarshalToolDone(t *testing.T) {
	input := `{"type":"tool_done","id":"tool-1","name":"Read","output":"file contents","error":null}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev ToolDoneEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ID != "tool-1" {
		t.Errorf("id = %q, want %q", ev.ID, "tool-1")
	}
	if ev.Name != "Read" {
		t.Errorf("name = %q, want %q", ev.Name, "Read")
	}
	if ev.Output != "file contents" {
		t.Errorf("output = %q, want %q", ev.Output, "file contents")
	}
	if ev.Error != nil {
		t.Errorf("error = %v, want nil", ev.Error)
	}
}

func TestUnmarshalToolDoneWithError(t *testing.T) {
	input := `{"type":"tool_done","id":"t2","name":"Bash","output":"","error":"exit code 1"}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev ToolDoneEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Error == nil || *ev.Error != "exit code 1" {
		t.Errorf("error = %v, want %q", ev.Error, "exit code 1")
	}
}

func TestUnmarshalErrorEvent(t *testing.T) {
	input := `{"type":"error","id":5,"message":"rate limited","retry_after_secs":30}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev ErrorEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ID != 5 {
		t.Errorf("id = %d, want 5", ev.ID)
	}
	if ev.Message != "rate limited" {
		t.Errorf("message = %q, want %q", ev.Message, "rate limited")
	}
	if ev.RetryAfterSecs == nil || *ev.RetryAfterSecs != 30 {
		t.Errorf("retry_after_secs = %v, want 30", ev.RetryAfterSecs)
	}
}

func TestUnmarshalHistoryEvent(t *testing.T) {
	input := `{"type":"history","id":1,"session_id":"sess-abc","messages":[],"was_interrupted":true}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev HistoryEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.SessionID != "sess-abc" {
		t.Errorf("session_id = %q, want %q", ev.SessionID, "sess-abc")
	}
	if ev.WasInterrupted == nil || !*ev.WasInterrupted {
		t.Error("was_interrupted should be true")
	}
}

func TestUnmarshalNotification(t *testing.T) {
	input := `{"type":"notification","from_session":"s1","from_name":"fox","notification_type":{"kind":"message","scope":"dm"},"message":"hello"}`
	_, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	var ev NotificationEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.FromSession != "s1" {
		t.Errorf("from_session = %q, want %q", ev.FromSession, "s1")
	}
	if ev.NotificationType.Kind != "message" {
		t.Errorf("notification_type.kind = %q, want %q", ev.NotificationType.Kind, "message")
	}
}

func TestNewSubscribe(t *testing.T) {
	req := NewSubscribe(1, "/home/user/workspace/test")
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["type"] != "subscribe" {
		t.Errorf("type = %v, want subscribe", parsed["type"])
	}
	if parsed["id"] != float64(1) {
		t.Errorf("id = %v, want 1", parsed["id"])
	}
	if parsed["working_dir"] != "/home/user/workspace/test" {
		t.Errorf("working_dir = %v", parsed["working_dir"])
	}
	if parsed["client_has_local_history"] != false {
		t.Errorf("client_has_local_history = %v, want false", parsed["client_has_local_history"])
	}
}

func TestNewSubscribeResume(t *testing.T) {
	req := NewSubscribeResume(2, "session-xyz", true)
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["type"] != "subscribe" {
		t.Errorf("type = %v, want subscribe", parsed["type"])
	}
	if parsed["target_session_id"] != "session-xyz" {
		t.Errorf("target_session_id = %v", parsed["target_session_id"])
	}
	if parsed["client_has_local_history"] != true {
		t.Errorf("client_has_local_history = %v, want true", parsed["client_has_local_history"])
	}
}

func TestNewMessage(t *testing.T) {
	req := NewMessage(3, "refactor foo.go", nil)
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed["type"] != "message" {
		t.Errorf("type = %v, want message", parsed["type"])
	}
	if parsed["content"] != "refactor foo.go" {
		t.Errorf("content = %v", parsed["content"])
	}
}

func TestNewMessageWithImages(t *testing.T) {
	req := NewMessage(4, "what is this?", nil)
	req.Images = []ImagePair{{"image/png", "base64data..."}}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}

	images, ok := parsed["images"].([]interface{})
	if !ok || len(images) != 1 {
		t.Fatalf("images = %v", parsed["images"])
	}
	pair := images[0].([]interface{})
	if pair[0] != "image/png" || pair[1] != "base64data..." {
		t.Errorf("image pair = %v", pair)
	}
}

func TestAtomicID(t *testing.T) {
	var id AtomicID
	if got := id.Next(); got != 1 {
		t.Errorf("first = %d, want 1", got)
	}
	if got := id.Next(); got != 2 {
		t.Errorf("second = %d, want 2", got)
	}
	if got := id.Next(); got != 3 {
		t.Errorf("third = %d, want 3", got)
	}
}

func TestUnknownEventType(t *testing.T) {
	// Events we don't handle should still parse successfully (type extraction).
	input := `{"type":"swarm_status","members":[]}`
	evType, raw, err := ParseServerEvent([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if evType != "swarm_status" {
		t.Errorf("type = %q, want %q", evType, "swarm_status")
	}
	if raw == nil {
		t.Error("raw should not be nil")
	}
}
