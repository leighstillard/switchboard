// Package jcodeproto defines Go types for the jcode Unix-socket NDJSON protocol.
//
// Wire format: one JSON object per newline (NDJSON). Both requests (client -> jcode)
// and server events (jcode -> client) use a "type" field as the discriminant tag
// (matching Rust's #[serde(tag = "type")] behavior). All field names are snake_case.
//
// IMPORTANT: The jcode daemon can emit lines up to 32 MB (e.g. history events with
// full conversation context). Callers MUST use a bufio.Scanner with a buffer of at
// least 32 * 1024 * 1024 bytes.
package jcodeproto

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Request ID generator
// ---------------------------------------------------------------------------

// AtomicID is a monotonically increasing counter for generating unique request IDs.
// It is safe for concurrent use.
type AtomicID struct {
	val atomic.Uint64
}

// Next returns the next unique ID (starting from 1).
func (a *AtomicID) Next() uint64 {
	return a.val.Add(1)
}

// ---------------------------------------------------------------------------
// Requests (client -> jcode)
// ---------------------------------------------------------------------------

// SubscribeRequest connects to a session (new or existing).
type SubscribeRequest struct {
	Type                  string  `json:"type"`                             // always "subscribe"
	ID                    uint64  `json:"id"`                               // unique request id
	WorkingDir            *string `json:"working_dir,omitempty"`            // cwd for new sessions
	TargetSessionID       *string `json:"target_session_id,omitempty"`      // resume existing session
	ClientHasLocalHistory bool    `json:"client_has_local_history"`         // whether client cached history
	ClientInstanceID      *string `json:"client_instance_id,omitempty"`     // unique client instance
}

// MessageRequest sends a user prompt to the active session.
type MessageRequest struct {
	Type           string      `json:"type"`                      // always "message"
	ID             uint64      `json:"id"`                        // unique request id
	Content        string      `json:"content"`                   // user text
	Images         []ImagePair `json:"images,omitempty"`          // attached images
	SystemReminder *string     `json:"system_reminder,omitempty"` // injected system context
}

// ImagePair is a tuple of [media_type, base64_data].
type ImagePair [2]string

// CancelRequest aborts the current generation turn.
type CancelRequest struct {
	Type string `json:"type"` // always "cancel"
	ID   uint64 `json:"id"`
}

// PingRequest is a heartbeat/keepalive.
type PingRequest struct {
	Type string `json:"type"` // always "ping"
	ID   uint64 `json:"id"`
}

// ---------------------------------------------------------------------------
// Request constructors
// ---------------------------------------------------------------------------

// NewSubscribe creates a SubscribeRequest for a new session.
func NewSubscribe(id uint64, workingDir string) *SubscribeRequest {
	wd := workingDir
	return &SubscribeRequest{
		Type:                  "subscribe",
		ID:                    id,
		WorkingDir:            &wd,
		ClientHasLocalHistory: false,
	}
}

// NewSubscribeResume creates a SubscribeRequest to resume an existing session.
func NewSubscribeResume(id uint64, sessionID string, hasHistory bool) *SubscribeRequest {
	return &SubscribeRequest{
		Type:                  "subscribe",
		ID:                    id,
		TargetSessionID:       &sessionID,
		ClientHasLocalHistory: hasHistory,
	}
}

// NewMessage creates a MessageRequest with text content.
func NewMessage(id uint64, content string, systemReminder *string) *MessageRequest {
	return &MessageRequest{
		Type:           "message",
		ID:             id,
		Content:        content,
		SystemReminder: systemReminder,
	}
}

// NewCancel creates a CancelRequest.
func NewCancel(id uint64) *CancelRequest {
	return &CancelRequest{
		Type: "cancel",
		ID:   id,
	}
}

// NewPing creates a PingRequest.
func NewPing(id uint64) *PingRequest {
	return &PingRequest{
		Type: "ping",
		ID:   id,
	}
}

// ---------------------------------------------------------------------------
// Server Events (jcode -> client)
// ---------------------------------------------------------------------------

// ServerEvent is the raw envelope for any event received from jcode.
// Callers should use ParseServerEvent to extract the type, then unmarshal
// the Raw bytes into the appropriate typed struct.
type ServerEvent struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"` // full JSON line for re-parsing
}

// ParseServerEvent extracts the event type and preserves the raw JSON for
// further unmarshaling into a specific event struct.
// Returns (eventType, rawJSON, error).
func ParseServerEvent(line []byte) (string, json.RawMessage, error) {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", nil, fmt.Errorf("jcodeproto: unmarshal event type: %w", err)
	}
	if ev.Type == "" {
		return "", nil, fmt.Errorf("jcodeproto: event missing \"type\" field")
	}
	return ev.Type, json.RawMessage(line), nil
}

// ---------------------------------------------------------------------------
// Event type constants
// ---------------------------------------------------------------------------

const (
	// Core lifecycle events
	EventAck      = "ack"
	EventDone     = "done"
	EventError    = "error"
	EventPong     = "pong"
	EventSession  = "session" // legacy, may not be emitted

	// Swarm/session status (primary way to get session_id)
	EventSwarmStatus = "swarm_status"

	// Streaming content events
	EventTextDelta   = "text_delta"
	EventTextReplace = "text_replace"
	EventMessageEnd  = "message_end"
	EventInterrupted = "interrupted"

	// Tool events
	EventToolStart = "tool_start"
	EventToolExec  = "tool_exec"
	EventToolDone  = "tool_done"

	// Media events
	EventGeneratedImage = "generated_image"

	// Notifications
	EventNotification = "notification"

	// Infrastructure events
	EventUpstreamProvider = "upstream_provider"
	EventConnectionType   = "connection_type"
	EventConnectionPhase  = "connection_phase"
	EventTokens           = "tokens"
	EventReloading        = "reloading"
	EventHistory          = "history"

	// Events we receive but pass through (log at debug level):
	// side_panel_state, memory_activity, compaction, batch_progress,
	// mcp_status, swarm_plan, model_changed, token_usage, etc.
)

// ---------------------------------------------------------------------------
// Typed event structs (v1 subset the bridge actively handles)
// ---------------------------------------------------------------------------

// AckEvent acknowledges receipt of a request.
type AckEvent struct {
	ID uint64 `json:"id"`
}

// DoneEvent signals a request has completed.
type DoneEvent struct {
	ID uint64 `json:"id"`
}

// ErrorEvent signals a request-level error.
type ErrorEvent struct {
	ID             uint64  `json:"id"`
	Message        string  `json:"message"`
	RetryAfterSecs *uint64 `json:"retry_after_secs,omitempty"`
}

// PongEvent responds to a ping.
type PongEvent struct {
	ID uint64 `json:"id"`
}

// SessionEvent provides the session ID after subscribing (legacy).
type SessionEvent struct {
	SessionID string `json:"session_id"`
}

// SwarmStatusEvent reports the current session(s) state.
// After a subscribe, the first member's session_id is the active session.
type SwarmStatusEvent struct {
	Members []SwarmMember `json:"members"`
}

// SwarmMember is a single session within the swarm.
type SwarmMember struct {
	SessionID       string `json:"session_id"`
	FriendlyName    string `json:"friendly_name"`
	Status          string `json:"status"` // "ready", "running", etc.
	Detail          string `json:"detail,omitempty"`
	Role            string `json:"role"`
	IsHeadless      bool   `json:"is_headless"`
	LiveAttachments int    `json:"live_attachments"`
	StatusAgeSecs   int    `json:"status_age_secs"`
}

// TokensEvent reports token usage for a turn.
type TokensEvent struct {
	Input             int `json:"input"`
	Output            int `json:"output"`
	CacheReadInput    int `json:"cache_read_input"`
	CacheCreationInput int `json:"cache_creation_input"`
}

// TextDeltaEvent is an incremental text append.
type TextDeltaEvent struct {
	Text string `json:"text"`
}

// TextReplaceEvent replaces the entire current text buffer.
type TextReplaceEvent struct {
	Text string `json:"text"`
}

// MessageEndEvent signals the end of an assistant message (no additional fields).
type MessageEndEvent struct{}

// InterruptedEvent signals the turn was cancelled (no additional fields).
type InterruptedEvent struct{}

// ToolStartEvent signals the start of a tool invocation.
// The Input field captures tool arguments from the wire JSON for generating
// human-friendly descriptions (Feature 1c). It is nil if the wire event
// doesn't include an "input" object.
type ToolStartEvent struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// ToolExecEvent signals a tool is actively executing.
type ToolExecEvent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ToolDoneEvent signals a tool invocation has completed.
type ToolDoneEvent struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Output string  `json:"output"`
	Error  *string `json:"error,omitempty"`
}

// GeneratedImageEvent signals an image was generated by a tool.
type GeneratedImageEvent struct {
	ID            string  `json:"id"`
	Path          string  `json:"path"`
	MetadataPath  *string `json:"metadata_path,omitempty"`
	OutputFormat  string  `json:"output_format"`
	RevisedPrompt *string `json:"revised_prompt,omitempty"`
}

// NotificationEvent carries an inter-session or system notification.
type NotificationEvent struct {
	FromSession      string           `json:"from_session"`
	FromName         *string          `json:"from_name,omitempty"`
	NotificationType NotificationType `json:"notification_type"`
	Message          string           `json:"message"`
}

// NotificationType is a tagged union (uses "kind" as the discriminant).
type NotificationType struct {
	Kind string          `json:"kind"`
	Raw  json.RawMessage `json:"-"` // full notification_type object for re-parsing
}

// UnmarshalJSON implements custom unmarshaling for NotificationType to capture
// the "kind" tag while preserving the raw JSON for variant-specific fields.
func (nt *NotificationType) UnmarshalJSON(data []byte) error {
	var obj struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	nt.Kind = obj.Kind
	nt.Raw = json.RawMessage(data)
	return nil
}

// MarshalJSON implements custom marshaling for NotificationType.
func (nt NotificationType) MarshalJSON() ([]byte, error) {
	if nt.Raw != nil {
		return []byte(nt.Raw), nil
	}
	return json.Marshal(struct {
		Kind string `json:"kind"`
	}{Kind: nt.Kind})
}

// Well-known notification kinds.
const (
	NotificationKindInfo    = "info"
	NotificationKindWarning = "warning"
	NotificationKindError   = "error"
	NotificationKindRequest = "request"
)

// UpstreamProviderEvent reports the active LLM provider.
type UpstreamProviderEvent struct {
	Provider string `json:"provider"`
}

// ReloadingEvent signals the daemon is reloading (e.g. after update).
type ReloadingEvent struct {
	NewSocket *string `json:"new_socket,omitempty"`
}

// HistoryEvent carries the full conversation history for a session.
// Most fields are kept as RawMessage since the bridge only needs a subset.
type HistoryEvent struct {
	ID             uint64          `json:"id"`
	SessionID      string          `json:"session_id"`
	Messages       json.RawMessage `json:"messages"`        // array of conversation messages
	WasInterrupted *bool           `json:"was_interrupted,omitempty"`
	// Additional fields (model, token counts, etc.) are intentionally not
	// decoded here; access them via the Raw field if needed.
}

// ---------------------------------------------------------------------------
// Buffer size constant
// ---------------------------------------------------------------------------

// MaxLineSize is the maximum NDJSON line size that jcode can emit (32 MB).
// Callers must configure their bufio.Scanner buffer to at least this size.
const MaxLineSize = 32 * 1024 * 1024
