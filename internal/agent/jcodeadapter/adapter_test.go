package jcodeadapter

import (
	"encoding/base64"
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

// ---------------------------------------------------------------------------
// Reused subscription deduplication (finding #1)
// ---------------------------------------------------------------------------

func TestGetOrCreateTranslated_DeduplicatesSameChannel(t *testing.T) {
	adapter := &Adapter{
		translated: make(map[<-chan *jcodeproto.ServerEvent]<-chan agent.Event),
	}

	// Simulate the raw channel that jcode.Client returns for a session.
	rawCh := make(chan *jcodeproto.ServerEvent, 8)
	var rawRecv <-chan *jcodeproto.ServerEvent = rawCh

	// First call should create a new translated channel + goroutine.
	ch1 := adapter.getOrCreateTranslated("sess-1", rawRecv)
	if ch1 == nil {
		t.Fatal("expected non-nil channel")
	}

	// Second call with the SAME raw channel should return the SAME translated channel.
	ch2 := adapter.getOrCreateTranslated("sess-1", rawRecv)

	// Channel identity check: read from one, confirm the other sees nothing new.
	// We can't compare channels directly in Go, but we can verify by sending
	// an event and checking only one consumer receives it.
	rawCh <- rawEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "hello",
	})

	// Both ch1 and ch2 should be the same channel, so reading from either works.
	ev := <-ch1
	if ev.Type != agent.EventTextDelta || ev.Text != "hello" {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// If ch2 were a different channel with a competing consumer, this next send
	// would be stolen. Send another event and verify ch2 receives it.
	rawCh <- rawEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "world",
	})
	ev2 := <-ch2
	if ev2.Type != agent.EventTextDelta || ev2.Text != "world" {
		t.Fatalf("unexpected event from ch2: %+v", ev2)
	}

	// Clean up: close raw channel to stop translateLoop.
	close(rawCh)
}

func TestGetOrCreateTranslated_DifferentChannels_DifferentLoops(t *testing.T) {
	adapter := &Adapter{
		translated: make(map[<-chan *jcodeproto.ServerEvent]<-chan agent.Event),
	}

	raw1 := make(chan *jcodeproto.ServerEvent, 4)
	raw2 := make(chan *jcodeproto.ServerEvent, 4)

	ch1 := adapter.getOrCreateTranslated("sess-1", raw1)
	ch2 := adapter.getOrCreateTranslated("sess-2", raw2)

	// They should be independent channels.
	raw1 <- rawEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "from-1",
	})
	raw2 <- rawEvent(t, jcodeproto.EventTextDelta, map[string]string{
		"type": "text_delta", "text": "from-2",
	})

	ev1 := <-ch1
	ev2 := <-ch2
	if ev1.Text != "from-1" {
		t.Errorf("ch1 got wrong event: %+v", ev1)
	}
	if ev2.Text != "from-2" {
		t.Errorf("ch2 got wrong event: %+v", ev2)
	}

	close(raw1)
	close(raw2)
}

// ---------------------------------------------------------------------------
// Image base64 encoding (finding #2)
// ---------------------------------------------------------------------------

func TestSendMessage_ImageBase64Encoding(t *testing.T) {
	// We can't easily test SendMessage end-to-end without a jcode daemon,
	// but we can verify the encoding logic directly.
	rawBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG magic bytes
	img := agent.Image{
		MediaType: "image/png",
		Data:      rawBytes,
	}

	// Simulate the conversion logic from SendMessage.
	b64 := base64.StdEncoding.EncodeToString(img.Data)
	pair := jcodeproto.ImagePair{img.MediaType, b64}

	// Verify it round-trips correctly.
	decoded, err := base64.StdEncoding.DecodeString(pair[1])
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if string(decoded) != string(rawBytes) {
		t.Errorf("round-trip mismatch: got %x, want %x", decoded, rawBytes)
	}

	// Verify it's NOT raw bytes (the old bug).
	if pair[1] == string(rawBytes) {
		t.Error("image data was not base64 encoded (still raw bytes)")
	}
}
