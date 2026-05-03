// Package coalesce implements per-session message buffering that accumulates
// jcode events and flushes them lazily to Slack. Each active agent session
// gets one SessionCoalescer that batches text deltas, tool progress, and
// other output into periodic chat.update calls (at most 1 Hz).
package coalesce

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/jcodeproto"
	"github.com/format5/switchboard/internal/outbound"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// flushInterval is the minimum time between lazy flushes.
	flushInterval = 1 * time.Second
	// maxSlackTextLen is Slack's per-message character limit.
	maxSlackTextLen = 12000
	// toolSummaryPrefix for completed tools.
	toolCheckmark = "✓"
	// toolPendingPrefix for in-progress tools.
	toolSpinner = "⏳"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// ToolProgress tracks a single tool invocation.
type ToolProgress struct {
	ID       string
	Name     string
	Output   string // only populated on done
	Error    string // only populated on done with error
	Done     bool
	Exec     bool // true after tool_exec (actively running)
}

// Identity holds the display name and icon for a session's Slack messages.
type Identity struct {
	DisplayName string
	IconURL     string
}

// ImageUploadRequest is emitted when the coalescer encounters a generated image.
type ImageUploadRequest struct {
	ChannelID string
	ThreadTS  string
	Path      string
	Caption   string
}

// OutboundEnqueuer is the interface for submitting items to the outbound queue.
type OutboundEnqueuer interface {
	Enqueue(item *outbound.OutboundItem)
}

// ImageHandler is called when the coalescer needs to upload a generated image.
type ImageHandler func(req ImageUploadRequest)

// ---------------------------------------------------------------------------
// SessionCoalescer
// ---------------------------------------------------------------------------

// SessionCoalescer buffers jcode output for a single session and flushes it
// to Slack at a controlled rate.
type SessionCoalescer struct {
	mu sync.Mutex

	sessionID    string
	friendlyName string
	channelID    string
	threadTS     string
	workdir      string // for display (basename)

	// The Slack message being updated (nil = first flush creates it).
	progressMessageTS *string

	textBuffer     strings.Builder
	pendingTools   []ToolProgress
	completedTools []ToolProgress

	upstreamProvider *string // captured for footer on final flush

	lastFlush time.Time
	dirty     bool
	finalized bool // true after Done/Error/Interrupted

	identity Identity
	outbound OutboundEnqueuer
	onImage  ImageHandler

	// Flush timer
	timer  *time.Timer
	done   chan struct{}
	closed bool
}

// NewSessionCoalescer creates a coalescer for a session.
func NewSessionCoalescer(
	sessionID, friendlyName, channelID, threadTS, workdir string,
	identity Identity,
	out OutboundEnqueuer,
	onImage ImageHandler,
) *SessionCoalescer {
	sc := &SessionCoalescer{
		sessionID:    sessionID,
		friendlyName: friendlyName,
		channelID:    channelID,
		threadTS:     threadTS,
		workdir:      workdir,
		identity:     identity,
		outbound:     out,
		onImage:      onImage,
		lastFlush:    time.Now(),
		done:         make(chan struct{}),
	}

	// Start the periodic flush timer.
	sc.timer = time.AfterFunc(flushInterval, sc.timerFlush)

	return sc
}

// Close stops the coalescer and performs a final flush if dirty.
func (sc *SessionCoalescer) Close() {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	sc.timer.Stop()
	sc.mu.Unlock()

	close(sc.done)
}

// HandleEvent processes a single jcode server event.
func (sc *SessionCoalescer) HandleEvent(ev *jcodeproto.ServerEvent) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.closed {
		return
	}

	switch ev.Type {
	case jcodeproto.EventTextDelta:
		var e jcodeproto.TextDeltaEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.textBuffer.WriteString(e.Text)
			sc.dirty = true
			sc.checkOverflow()
		}

	case jcodeproto.EventTextReplace:
		var e jcodeproto.TextReplaceEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.textBuffer.Reset()
			sc.textBuffer.WriteString(e.Text)
			sc.dirty = true
		}

	case jcodeproto.EventToolStart:
		var e jcodeproto.ToolStartEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.pendingTools = append(sc.pendingTools, ToolProgress{
				ID:   e.ID,
				Name: e.Name,
			})
			sc.dirty = true
		}

	case jcodeproto.EventToolExec:
		var e jcodeproto.ToolExecEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					sc.pendingTools[i].Exec = true
					break
				}
			}
			sc.dirty = true
		}

	case jcodeproto.EventToolDone:
		var e jcodeproto.ToolDoneEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			// Move from pending to completed.
			tp := ToolProgress{
				ID:   e.ID,
				Name: e.Name,
				Done: true,
			}
			if e.Error != nil {
				tp.Error = *e.Error
			}
			// Truncate output for display.
			if len(e.Output) > 200 {
				tp.Output = e.Output[:200] + "..."
			} else {
				tp.Output = e.Output
			}
			sc.completedTools = append(sc.completedTools, tp)

			// Remove from pending.
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					sc.pendingTools = append(sc.pendingTools[:i], sc.pendingTools[i+1:]...)
					break
				}
			}
			sc.dirty = true
		}

	case jcodeproto.EventUpstreamProvider:
		var e jcodeproto.UpstreamProviderEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.upstreamProvider = &e.Provider
		}

	case jcodeproto.EventGeneratedImage:
		var e jcodeproto.GeneratedImageEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			caption := "Generated image"
			if e.RevisedPrompt != nil {
				caption = *e.RevisedPrompt
			}
			if sc.onImage != nil {
				sc.onImage(ImageUploadRequest{
					ChannelID: sc.channelID,
					ThreadTS:  sc.threadTS,
					Path:      e.Path,
					Caption:   caption,
				})
			}
		}

	case jcodeproto.EventMessageEnd:
		sc.flushLocked(false)

	case jcodeproto.EventDone:
		sc.finalized = true
		sc.flushLocked(true)

	case jcodeproto.EventError:
		var e jcodeproto.ErrorEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.textBuffer.WriteString(fmt.Sprintf("\n\n❌ **Error:** %s", e.Message))
			sc.dirty = true
		}
		sc.finalized = true
		sc.flushLocked(true)

	case jcodeproto.EventInterrupted:
		sc.textBuffer.WriteString("\n\n⚠️ _Agent interrupted_")
		sc.dirty = true
		sc.finalized = true
		sc.flushLocked(true)

	case jcodeproto.EventNotification:
		var e jcodeproto.NotificationEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			fromName := "agent"
			if e.FromName != nil {
				fromName = *e.FromName
			}
			sc.textBuffer.WriteString(fmt.Sprintf("\n\n📢 _%s: %s_", fromName, e.Message))
			sc.dirty = true
		}
	}
}

// SetFriendlyName updates the display name (e.g., after session event).
func (sc *SessionCoalescer) SetFriendlyName(name string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.friendlyName = name
}

// ProgressMessageTS returns the current progress message timestamp, if any.
func (sc *SessionCoalescer) ProgressMessageTS() *string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.progressMessageTS
}

// ---------------------------------------------------------------------------
// Internal flush logic
// ---------------------------------------------------------------------------

// timerFlush is called by the periodic timer.
func (sc *SessionCoalescer) timerFlush() {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.closed {
		return
	}

	if sc.dirty && time.Since(sc.lastFlush) >= flushInterval {
		sc.flushLocked(false)
	}

	// Reschedule.
	sc.timer.Reset(flushInterval)
}

// flushLocked builds and enqueues the Slack message. Must be called with mu held.
func (sc *SessionCoalescer) flushLocked(isFinal bool) {
	if !sc.dirty && !isFinal {
		return
	}

	text := sc.renderMessage(isFinal)
	if text == "" && !isFinal {
		return
	}

	priority := 2 // chat.update
	if isFinal {
		priority = 1 // terminal flush
	}

	if sc.progressMessageTS == nil {
		// First flush: PostMessage to create the progress message.
		item := &outbound.OutboundItem{
			Priority:  3, // chat.postMessage
			ChannelID: sc.channelID,
			ThreadTS:  sc.threadTS,
			Action:    outbound.ActionPostMessage,
			Text:      text,
			Username:  sc.identity.DisplayName,
			IconURL:   sc.identity.IconURL,
		}
		// We need the TS back to switch to updates. Use a callback approach:
		// Actually, the outbound queue doesn't return the TS to us.
		// We need a different approach: post directly and capture TS.
		// For now, enqueue as PostMessage - the router will handle TS capture.
		sc.outbound.Enqueue(item)
		// Mark as "awaiting TS" - next flushes will be queued until we get it.
		placeholder := ""
		sc.progressMessageTS = &placeholder
	} else if *sc.progressMessageTS != "" {
		// Subsequent flush: UpdateMessage.
		item := &outbound.OutboundItem{
			Priority:  priority,
			ChannelID: sc.channelID,
			Action:    outbound.ActionUpdateMessage,
			Text:      text,
			MessageTS: *sc.progressMessageTS,
			Username:  sc.identity.DisplayName,
			IconURL:   sc.identity.IconURL,
		}
		sc.outbound.Enqueue(item)
	}

	sc.dirty = false
	sc.lastFlush = time.Now()
}

// SetProgressMessageTS is called by the router once the initial PostMessage
// returns a timestamp from Slack.
func (sc *SessionCoalescer) SetProgressMessageTS(ts string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.progressMessageTS = &ts
	slog.Debug("coalescer: progress message TS set", "session_id", sc.sessionID, "ts", ts)
}

// checkOverflow splits into a new message if we're approaching Slack's limit.
func (sc *SessionCoalescer) checkOverflow() {
	if sc.textBuffer.Len() > maxSlackTextLen {
		// Flush current content as a finalized message, then reset.
		sc.flushLocked(false)
		// Start new progress message.
		sc.progressMessageTS = nil
		sc.textBuffer.Reset()
		sc.completedTools = nil
	}
}

// renderMessage constructs the Slack message text from current state.
func (sc *SessionCoalescer) renderMessage(isFinal bool) string {
	var sb strings.Builder

	// Header.
	emoji := "🤖"
	name := sc.friendlyName
	if name == "" {
		name = sc.sessionID[:8]
	}
	workdirBase := filepath.Base(sc.workdir)
	sb.WriteString(fmt.Sprintf("**%s %s @ %s**\n\n", emoji, name, workdirBase))

	// Text content.
	text := sc.textBuffer.String()
	if text != "" {
		sb.WriteString(text)
		sb.WriteString("\n")
	}

	// Tool summary (completed).
	if len(sc.completedTools) > 0 {
		sb.WriteString("\n")
		for _, t := range sc.completedTools {
			if t.Error != "" {
				sb.WriteString(fmt.Sprintf("❌ %s\n", t.Name))
			} else {
				sb.WriteString(fmt.Sprintf("%s %s\n", toolCheckmark, t.Name))
			}
		}
	}

	// Pending tools.
	if len(sc.pendingTools) > 0 {
		sb.WriteString("\n")
		for _, t := range sc.pendingTools {
			status := "starting"
			if t.Exec {
				status = "running"
			}
			sb.WriteString(fmt.Sprintf("%s %s (%s)\n", toolSpinner, t.Name, status))
		}
	}

	// Provider footer (final flush only, if non-default).
	if isFinal && sc.upstreamProvider != nil {
		sb.WriteString(fmt.Sprintf("\n_%s_\n", *sc.upstreamProvider))
	}

	return sb.String()
}
