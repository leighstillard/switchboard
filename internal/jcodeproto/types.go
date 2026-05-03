// Package jcodeproto defines hand-rolled types for the jcode Unix socket protocol.
// These types mirror the wire format without depending on external protobuf tooling.
package jcodeproto

// MessageType identifies the kind of protocol message.
type MessageType string

const (
	// Client -> Server
	MsgTypeSessionCreate  MessageType = "session.create"
	MsgTypeSessionResume  MessageType = "session.resume"
	MsgTypeSessionDestroy MessageType = "session.destroy"
	MsgTypeUserMessage    MessageType = "user.message"
	MsgTypeUserCancel     MessageType = "user.cancel"

	// Server -> Client
	MsgTypeSessionCreated MessageType = "session.created"
	MsgTypeAgentMessage   MessageType = "agent.message"
	MsgTypeAgentToolUse   MessageType = "agent.tool_use"
	MsgTypeAgentDone      MessageType = "agent.done"
	MsgTypeError          MessageType = "error"
)

// Envelope is the framing wrapper for all protocol messages.
type Envelope struct {
	Type      MessageType `json:"type"`
	SessionID string      `json:"session_id,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
	Payload   any         `json:"payload,omitempty"`
}

// SessionCreatePayload is the payload for session.create messages.
type SessionCreatePayload struct {
	Workdir string `json:"workdir"`
	Model   string `json:"model,omitempty"`
}

// SessionCreatedPayload is the response to session.create.
type SessionCreatedPayload struct {
	SessionID string `json:"session_id"`
}

// UserMessagePayload carries a user message to a session.
type UserMessagePayload struct {
	Content string `json:"content"`
}

// AgentMessagePayload carries an agent response fragment.
type AgentMessagePayload struct {
	Content string `json:"content"`
	Done    bool   `json:"done"`
}

// AgentToolUsePayload indicates the agent is using a tool.
type AgentToolUsePayload struct {
	Tool  string `json:"tool"`
	Input string `json:"input,omitempty"`
}

// ErrorPayload carries error information.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
