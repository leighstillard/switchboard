// Package claude implements the agent.Backend interface for Claude Code CLI.
//
// Each session corresponds to a Claude Code CLI session (identified by UUID).
// Turns are executed by spawning `claude -p` processes; streaming events are
// translated from Claude's stream-json format into the normalized agent.Event
// vocabulary.
package claude

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/format5/switchboard/internal/agent"
)

// ---------------------------------------------------------------------------
// Translator: stateful per-session stream-json -> agent.Event mapper
// ---------------------------------------------------------------------------

// blockKind distinguishes content block types within a message.
type blockKind int

const (
	blockText    blockKind = iota
	blockToolUse
	blockThinking
	blockUnknown
)

// blockInfo tracks an open content block by its index.
type blockInfo struct {
	kind   blockKind
	toolID string // only for blockToolUse
	name   string // only for blockToolUse
}

// translator holds per-session state for translating Claude stream-json lines
// into agent.Event values.
type translator struct {
	// openBlocks tracks content blocks by their index within the current
	// message. Populated on content_block_start, consumed on content_block_stop.
	openBlocks map[int]*blockInfo

	// toolNames maps tool_use_id to tool name so we can thread the name
	// through to EventToolDone when processing user/tool_result events.
	toolNames map[string]string

	// lastToolBlockIndex tracks the most recently started tool_use block
	// index, used to assert the ordering invariant.
	lastToolBlockIndex int
	hasOpenToolBlock   bool
}

func newTranslator() *translator {
	return &translator{
		openBlocks:         make(map[int]*blockInfo),
		toolNames:          make(map[string]string),
		lastToolBlockIndex: -1,
	}
}

// translateLine parses a single JSON line from Claude's stream-json output
// and returns zero or more agent.Events.
func (t *translator) translateLine(line []byte) []agent.Event {
	if len(line) == 0 {
		return nil
	}

	// Parse the top-level envelope.
	var envelope struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		Raw     json.RawMessage `json:"-"`
	}

	// We need the raw JSON for further parsing, and also the envelope fields.
	if err := json.Unmarshal(line, &envelope); err != nil {
		slog.Debug("claude: failed to parse line", "err", err, "line", string(line))
		return nil
	}

	switch envelope.Type {
	case "system":
		return t.handleSystem(line)
	case "stream_event":
		return t.handleStreamEvent(line)
	case "assistant":
		// Full accumulated message; redundant with streaming deltas.
		return nil
	case "user":
		return t.handleUser(line)
	case "result":
		return t.handleResult(line)
	case "rate_limit_event":
		return nil
	default:
		// Silently skip unknown event types (hooks, etc.)
		return nil
	}
}

// handleSystem processes system events (init, hook_started, hook_response).
func (t *translator) handleSystem(line []byte) []agent.Event {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	switch ev.Subtype {
	case "init":
		var init struct {
			SessionID string `json:"session_id"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal(line, &init); err != nil {
			return nil
		}
		return []agent.Event{{
			Type:      agent.EventSessionReady,
			SessionID: init.SessionID,
			Model:     init.Model,
		}}
	default:
		// hook_started, hook_response, etc. - silently skip
		return nil
	}
}

// handleStreamEvent processes Anthropic SSE wrapper events.
func (t *translator) handleStreamEvent(line []byte) []agent.Event {
	// Extract the inner event type and data.
	var wrapper struct {
		Event string `json:"event"` // message_start, content_block_start, etc.
	}
	if err := json.Unmarshal(line, &wrapper); err != nil {
		return nil
	}

	switch wrapper.Event {
	case "content_block_start":
		return t.handleContentBlockStart(line)
	case "content_block_delta":
		return t.handleContentBlockDelta(line)
	case "content_block_stop":
		return t.handleContentBlockStop(line)
	case "message_stop":
		return []agent.Event{{Type: agent.EventMessageEnd}}
	case "message_start", "message_delta":
		// No agent events needed for these.
		return nil
	default:
		return nil
	}
}

// handleContentBlockStart processes content_block_start events.
func (t *translator) handleContentBlockStart(line []byte) []agent.Event {
	var ev struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	switch ev.ContentBlock.Type {
	case "thinking":
		t.openBlocks[ev.Index] = &blockInfo{kind: blockThinking}
		return nil

	case "text":
		t.openBlocks[ev.Index] = &blockInfo{kind: blockText}
		return nil

	case "tool_use":
		// LOAD-BEARING INVARIANT: no ToolStart for block N+1 before ToolExec for block N.
		if t.hasOpenToolBlock {
			slog.Error("claude: tool block ordering invariant violated",
				"new_index", ev.Index, "open_index", t.lastToolBlockIndex)
		}

		bi := &blockInfo{
			kind:   blockToolUse,
			toolID: ev.ContentBlock.ID,
			name:   ev.ContentBlock.Name,
		}
		t.openBlocks[ev.Index] = bi
		t.toolNames[ev.ContentBlock.ID] = ev.ContentBlock.Name
		t.lastToolBlockIndex = ev.Index
		t.hasOpenToolBlock = true

		return []agent.Event{{
			Type:     agent.EventToolStart,
			ToolID:   ev.ContentBlock.ID,
			ToolName: ev.ContentBlock.Name,
		}}

	default:
		t.openBlocks[ev.Index] = &blockInfo{kind: blockUnknown}
		return nil
	}
}

// handleContentBlockDelta processes content_block_delta events.
func (t *translator) handleContentBlockDelta(line []byte) []agent.Event {
	var ev struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	switch ev.Delta.Type {
	case "thinking_delta", "signature_delta":
		// Silently skip thinking content and signatures.
		return nil

	case "text_delta":
		return []agent.Event{{
			Type: agent.EventTextDelta,
			Text: ev.Delta.Text,
		}}

	case "input_json_delta":
		bi := t.openBlocks[ev.Index]
		var toolID string
		if bi != nil && bi.kind == blockToolUse {
			toolID = bi.toolID
		}
		return []agent.Event{{
			Type:        agent.EventToolInputDelta,
			ToolID:      toolID,
			PartialJSON: ev.Delta.PartialJSON,
		}}

	default:
		return nil
	}
}

// handleContentBlockStop processes content_block_stop events.
func (t *translator) handleContentBlockStop(line []byte) []agent.Event {
	var ev struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	bi, ok := t.openBlocks[ev.Index]
	if !ok {
		return nil
	}
	delete(t.openBlocks, ev.Index)

	if bi.kind == blockToolUse {
		t.hasOpenToolBlock = false
		return []agent.Event{{
			Type:     agent.EventToolExec,
			ToolID:   bi.toolID,
			ToolName: bi.name,
		}}
	}

	return nil
}

// handleUser processes user events which may contain tool_result blocks.
func (t *translator) handleUser(line []byte) []agent.Event {
	var ev struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	// Content can be a string or an array of content blocks.
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
		// Not an array - likely a string prompt. Ignore.
		return nil
	}

	var events []agent.Event
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		name := t.toolNames[block.ToolUseID]
		events = append(events, agent.Event{
			Type:     agent.EventToolDone,
			ToolID:   block.ToolUseID,
			ToolName: name,
			IsError:  block.IsError,
		})
	}
	return events
}

// handleResult processes result events (success or error).
func (t *translator) handleResult(line []byte) []agent.Event {
	var ev struct {
		Subtype      string `json:"subtype"`
		ErrorMessage string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	if ev.Subtype == "success" {
		return []agent.Event{{Type: agent.EventTurnDone}}
	}

	// Any non-success subtype is an error.
	msg := ev.ErrorMessage
	if msg == "" {
		msg = fmt.Sprintf("claude turn failed: %s", ev.Subtype)
	}
	return []agent.Event{{
		Type:         agent.EventTurnError,
		ErrorMessage: msg,
	}}
}
