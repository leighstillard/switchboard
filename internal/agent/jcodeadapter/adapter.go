// Package jcodeadapter wraps the existing jcode.Client to satisfy
// agent.Backend, translating jcodeproto.ServerEvent into agent.Event.
package jcodeadapter

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/format5/switchboard/internal/agent"
	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/jcodeproto"
)

// Adapter wraps a jcode.Client and implements agent.Backend.
type Adapter struct {
	client *jcode.Client
}

// New creates a new jcode adapter backed by the given client.
func New(client *jcode.Client) *Adapter {
	return &Adapter{client: client}
}

// Client returns the underlying jcode.Client for direct access when needed
// (e.g. daemon management).
func (a *Adapter) Client() *jcode.Client {
	return a.client
}

func (a *Adapter) Subscribe(ctx context.Context, workdir string) (string, <-chan agent.Event, error) {
	sessionID, rawEvents, err := a.client.Subscribe(ctx, workdir)
	if err != nil {
		return "", nil, err
	}
	events := make(chan agent.Event, cap(rawEvents))
	go translateLoop(sessionID, rawEvents, events)
	return sessionID, events, nil
}

func (a *Adapter) SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan agent.Event, error) {
	rawEvents, err := a.client.SubscribeExisting(ctx, sessionID, workdir)
	if err != nil {
		return nil, err
	}
	events := make(chan agent.Event, cap(rawEvents))
	go translateLoop(sessionID, rawEvents, events)
	return events, nil
}

func (a *Adapter) SendMessage(ctx context.Context, sessionID, content string, images []agent.Image) error {
	// Convert agent.Image to jcodeproto.ImagePair.
	var pairs []jcodeproto.ImagePair
	for _, img := range images {
		// jcode expects [media_type, base64_data]; the router already provides
		// decoded bytes, but jcode's wire format is base64. For now we pass
		// through as-is since the router constructs ImagePairs directly.
		// This will need base64 encoding when images are actually used.
		pairs = append(pairs, jcodeproto.ImagePair{img.MediaType, string(img.Data)})
	}
	return a.client.SendMessage(ctx, sessionID, content, pairs)
}

func (a *Adapter) Cancel(ctx context.Context, sessionID string) error {
	return a.client.Cancel(ctx, sessionID)
}

func (a *Adapter) Close() error {
	return a.client.Close()
}

// ---------------------------------------------------------------------------
// Translation
// ---------------------------------------------------------------------------

// translateLoop reads jcodeproto.ServerEvent from rawEvents and writes
// agent.Event to events. It closes events when rawEvents is closed.
func translateLoop(sessionID string, rawEvents <-chan *jcodeproto.ServerEvent, events chan<- agent.Event) {
	defer close(events)
	for raw := range rawEvents {
		for _, ev := range Translate(raw) {
			events <- ev
		}
	}
}

// Translate converts a single jcodeproto.ServerEvent into zero or more
// agent.Events. Exported for testing.
func Translate(ev *jcodeproto.ServerEvent) []agent.Event {
	switch ev.Type {
	case jcodeproto.EventSwarmStatus:
		var e jcodeproto.SwarmStatusEvent
		if json.Unmarshal(ev.Raw, &e) != nil || len(e.Members) == 0 {
			return nil
		}
		return []agent.Event{{
			Type:      agent.EventSessionReady,
			SessionID: e.Members[0].SessionID,
		}}

	case jcodeproto.EventSession:
		var e jcodeproto.SessionEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:      agent.EventSessionReady,
			SessionID: e.SessionID,
		}}

	case jcodeproto.EventTextDelta:
		var e jcodeproto.TextDeltaEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{Type: agent.EventTextDelta, Text: e.Text}}

	case jcodeproto.EventTextReplace:
		var e jcodeproto.TextReplaceEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{Type: agent.EventTextReplace, Text: e.Text}}

	case jcodeproto.EventToolStart:
		var e jcodeproto.ToolStartEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:      agent.EventToolStart,
			ToolID:    e.ID,
			ToolName:  e.Name,
			ToolInput: e.Input,
		}}

	case jcodeproto.EventToolInput:
		var e jcodeproto.ToolInputEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		// jcode tool_input events carry no tool ID; emit with empty ToolID
		// so coalesce falls back to its "most recently started non-exec tool"
		// heuristic (behavior-preserving).
		return []agent.Event{{
			Type:        agent.EventToolInputDelta,
			ToolID:      "", // intentionally empty for jcode
			PartialJSON: e.Delta,
		}}

	case jcodeproto.EventToolExec:
		var e jcodeproto.ToolExecEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:     agent.EventToolExec,
			ToolID:   e.ID,
			ToolName: e.Name,
		}}

	case jcodeproto.EventToolDone:
		var e jcodeproto.ToolDoneEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:     agent.EventToolDone,
			ToolID:   e.ID,
			ToolName: e.Name,
			IsError:  e.Error != nil,
		}}

	case jcodeproto.EventMessageEnd:
		return []agent.Event{{Type: agent.EventMessageEnd}}

	case jcodeproto.EventDone:
		return []agent.Event{{Type: agent.EventTurnDone}}

	case jcodeproto.EventError:
		var e jcodeproto.ErrorEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:         agent.EventTurnError,
			ErrorMessage: e.Message,
		}}

	case jcodeproto.EventInterrupted:
		return []agent.Event{{Type: agent.EventInterrupted}}

	case jcodeproto.EventGeneratedImage:
		var e jcodeproto.GeneratedImageEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		caption := "Generated image"
		if e.RevisedPrompt != nil {
			caption = *e.RevisedPrompt
		}
		return []agent.Event{{
			Type:         agent.EventImageGenerated,
			ImagePath:    e.Path,
			ImageCaption: caption,
		}}

	case jcodeproto.EventNotification:
		var e jcodeproto.NotificationEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		from := "agent"
		if e.FromName != nil {
			from = *e.FromName
		}
		return []agent.Event{{
			Type:             agent.EventNotification,
			NotificationKind: e.NotificationType.Kind,
			NotificationFrom: from,
			NotificationMsg:  e.Message,
		}}

	case jcodeproto.EventUpstreamProvider:
		var e jcodeproto.UpstreamProviderEvent
		if json.Unmarshal(ev.Raw, &e) != nil {
			return nil
		}
		return []agent.Event{{
			Type:         agent.EventProvider,
			ProviderName: e.Provider,
		}}

	case jcodeproto.EventHistory:
		// History events are handled by the router directly (for was_interrupted
		// check). Pass through as a SessionReady with the session_id.
		var partial struct {
			SessionID      string `json:"session_id"`
			WasInterrupted *bool  `json:"was_interrupted,omitempty"`
		}
		if json.Unmarshal(ev.Raw, &partial) != nil {
			return nil
		}
		// We don't emit a normalized event for history; the router handles
		// this at the raw level. Return nil so the adapter silently drops it.
		return nil

	default:
		// Infrastructure events (ack, pong, tokens, connection_type, etc.)
		// are silently dropped. They're handled by the jcode client internally.
		slog.Debug("jcodeadapter: dropping unhandled event", "type", ev.Type)
		return nil
	}
}
