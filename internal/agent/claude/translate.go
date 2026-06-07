// Package claude implements the agent.Backend interface for the Claude Code CLI.
//
// Each session corresponds to one long-running `claude` process driven over
// stdin/stdout with bidirectional stream-json (`--input-format stream-json
// --output-format stream-json`). In this persistent, interactive mode the CLI
// does NOT emit `stream_event`/`content_block_delta` lines (those are
// `--print`/`--include-partial-messages` only); text and tool calls arrive as
// full `assistant` messages. The translator maps that native stream-json into
// the normalized agent.Event vocabulary.
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

// translator holds per-session state for translating Claude stream-json lines
// into agent.Event values.
type translator struct {
	// toolNames maps tool_use_id to tool name so we can thread the name
	// through to EventToolDone when processing user/tool_result events
	// (claude's tool_result does not carry the tool name).
	toolNames map[string]string
}

func newTranslator() *translator {
	return &translator{
		toolNames: make(map[string]string),
	}
}

// translateLine parses a single JSON line from Claude's stream-json output
// and returns zero or more agent.Events.
func (t *translator) translateLine(line []byte) []agent.Event {
	if len(line) == 0 {
		return nil
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		slog.Debug("claude: failed to parse line", "err", err, "line", string(line))
		return nil
	}

	switch envelope.Type {
	case "system":
		return t.handleSystem(line)
	case "assistant":
		return t.handleAssistant(line)
	case "user":
		return t.handleUser(line)
	case "result":
		return t.handleResult(line)
	default:
		// rate_limit_event, stream_event (never seen in persistent mode),
		// hooks, and any unknown future type are silently skipped.
		return nil
	}
}

// handleSystem processes system events. Only `init` is mapped (→ SessionReady);
// other subtypes (hook_started, thinking_tokens, …) are skipped.
func (t *translator) handleSystem(line []byte) []agent.Event {
	var ev struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	if ev.Subtype != "init" {
		return nil
	}

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
}

// handleAssistant processes a full `assistant` message. Its content array holds
// text / thinking / tool_use blocks. Text is emitted once (no duplicate from
// the later `result`); thinking is dropped; each tool_use becomes a contiguous
// ToolStart→ToolExec pair (full input is already present — there are no input
// deltas in persistent mode). A MessageEnd terminates every assistant message,
// giving coalesce a mid-turn progress flush (non-finalizing).
func (t *translator) handleAssistant(line []byte) []agent.Event {
	var ev struct {
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	var events []agent.Event
	for _, block := range ev.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, agent.Event{
					Type: agent.EventTextDelta,
					Text: block.Text,
				})
			}
		case "thinking":
			// Thinking content is silently dropped.
		case "tool_use":
			t.toolNames[block.ID] = block.Name
			// LOAD-BEARING INVARIANT (coalesce routes input deltas to the most
			// recently started non-exec tool): emit ToolStart immediately
			// followed by ToolExec so no two tool blocks are ever open at once.
			events = append(events,
				agent.Event{
					Type:      agent.EventToolStart,
					ToolID:    block.ID,
					ToolName:  block.Name,
					ToolInput: block.Input,
				},
				agent.Event{
					Type:     agent.EventToolExec,
					ToolID:   block.ID,
					ToolName: block.Name,
				},
			)
		}
	}
	events = append(events, agent.Event{Type: agent.EventMessageEnd})
	return events
}

// handleUser processes user events. The replayed user prompt (string content)
// is skipped; tool_result blocks (array content) become ToolDone, with the
// tool name threaded from the originating tool_use block.
func (t *translator) handleUser(line []byte) []agent.Event {
	var ev struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}

	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
		// Content is a string (the replayed user prompt) — ignore.
		return nil
	}

	var events []agent.Event
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		events = append(events, agent.Event{
			Type:     agent.EventToolDone,
			ToolID:   block.ToolUseID,
			ToolName: t.toolNames[block.ToolUseID],
			IsError:  block.IsError,
		})
		// Drop the name mapping so the map does not grow unbounded across a
		// long-running persistent session.
		delete(t.toolNames, block.ToolUseID)
	}
	return events
}

// handleResult processes the terminal result event. subtype=="success" →
// TurnDone; any other subtype → TurnError (inequality, not an allow-list, so a
// future error subtype is never silently treated as success). The result's
// `result` text is deliberately NOT emitted — it repeats the last assistant
// message and would duplicate text.
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

	msg := ev.ErrorMessage
	if msg == "" {
		msg = fmt.Sprintf("claude turn failed: %s", ev.Subtype)
	}
	return []agent.Event{{
		Type:         agent.EventTurnError,
		ErrorMessage: msg,
	}}
}
