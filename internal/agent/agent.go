// Package agent defines the backend-agnostic agent event vocabulary and
// Backend interface used by the router and coalescer. Both jcode and Claude
// Code are adapters that translate their native event streams into this
// normalized vocabulary.
package agent

import "context"

// ---------------------------------------------------------------------------
// Event types
// ---------------------------------------------------------------------------

// EventType discriminates normalized agent events.
type EventType int

const (
	EventSessionReady EventType = iota
	EventTextDelta
	EventTextReplace
	EventToolStart
	EventToolInputDelta
	EventToolExec
	EventToolDone
	EventMessageEnd
	EventTurnDone
	EventTurnError
	EventInterrupted
	EventImageGenerated
	EventNotification
	EventProvider
)

// Event is the normalized event emitted by all agent backends.
// Only the fields relevant to the event's Type are populated.
type Event struct {
	Type EventType

	// SessionReady
	SessionID string
	Model     string

	// TextDelta / TextReplace
	Text string

	// ToolStart / ToolExec / ToolDone / ToolInputDelta
	ToolID   string
	ToolName string
	ToolInput map[string]any // ToolStart: parsed input args

	// ToolInputDelta
	// Empty ID means "route to most recently started non-exec tool" (jcode compat).
	PartialJSON string

	// ToolDone
	IsError bool

	// TurnError
	ErrorMessage string

	// ImageGenerated
	ImagePath    string
	ImageCaption string

	// Notification
	NotificationKind string
	NotificationFrom string
	NotificationMsg  string

	// Provider
	ProviderName string
}

// ---------------------------------------------------------------------------
// Image (backend-neutral)
// ---------------------------------------------------------------------------

// Image carries decoded image data for sending to an agent.
type Image struct {
	MediaType string // e.g. "image/png"
	Data      []byte // raw image bytes
}

// ---------------------------------------------------------------------------
// Backend interface
// ---------------------------------------------------------------------------

// Backend is the contract that both jcode and Claude Code adapters implement.
// The router calls these methods to manage agent sessions.
type Backend interface {
	// Subscribe creates a new session in the given workdir. Returns the
	// session ID and a channel of normalized events.
	Subscribe(ctx context.Context, workdir string) (sessionID string, events <-chan Event, err error)

	// SubscribeExisting reconnects to an existing session by ID.
	SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan Event, error)

	// SendMessage sends a user message to the specified session.
	SendMessage(ctx context.Context, sessionID, content string, images []Image) error

	// Cancel aborts the current generation turn for the specified session.
	Cancel(ctx context.Context, sessionID string) error

	// CloseSession permanently tears down ONE session (process/connection +
	// event channel, exactly once) without affecting other sessions. It does
	// not touch the store — persistence is owned by the router/store layer.
	CloseSession(ctx context.Context, sessionID string) error

	// Close shuts down all sessions and releases resources.
	Close() error
}
