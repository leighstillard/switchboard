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
	"github.com/format5/switchboard/internal/render"
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
	ID          string
	Name        string
	Description string // human-friendly, e.g. "Reading `auth.go`"
	Output      string // only populated on done
	Error       string // only populated on done with error
	Done        bool
	Exec        bool // true after tool_exec (actively running)
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
	toolInputBuf   strings.Builder // accumulates tool_input deltas for current pending tool

	// Block Kit blocks from render directives (accumulated during the turn).
	directiveBlocks []map[string]interface{}
	// Fallback text from directives (for clients that can't render blocks).
	directiveFallback string

	upstreamProvider *string // captured for footer on final flush

	firstTurn bool // true until the first turn completes (show header)
	lastFlush time.Time
	dirty     bool
	finalized bool // true after Done/Error/Interrupted

	identity Identity
	outbound OutboundEnqueuer
	onImage  ImageHandler

	// Drift monitor for tool description word count (Feature 1c).
	driftMonitor *render.DriftMonitor

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
		firstTurn:    true,
		lastFlush:    time.Now(),
		driftMonitor: render.NewDriftMonitor(7), // default threshold
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
			desc := render.Describe(e.Name, e.Input)
			sc.driftMonitor.Record(desc)
			if sc.driftMonitor.IsAboveThreshold() {
				slog.Warn("tool description drift above threshold",
					"session_id", sc.sessionID,
					"avg_words", sc.driftMonitor.Average(),
				)
			}
			sc.pendingTools = append(sc.pendingTools, ToolProgress{
				ID:          e.ID,
				Name:        e.Name,
				Description: desc,
			})
			sc.dirty = true
		}

	case jcodeproto.EventToolInput:
		var e jcodeproto.ToolInputEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.toolInputBuf.WriteString(e.Delta)
		}

	case jcodeproto.EventToolExec:
		var e jcodeproto.ToolExecEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					sc.pendingTools[i].Exec = true
					// Parse accumulated tool input and update description.
					if sc.toolInputBuf.Len() > 0 {
						var input map[string]any
						if json.Unmarshal([]byte(sc.toolInputBuf.String()), &input) == nil {
							desc := render.Describe(e.Name, input)
							sc.driftMonitor.Record(desc)
							sc.pendingTools[i].Description = desc
						}
						sc.toolInputBuf.Reset()
					}
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

			// Carry description from the pending tool entry.
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					tp.Description = sc.pendingTools[i].Description
					sc.pendingTools = append(sc.pendingTools[:i], sc.pendingTools[i+1:]...)
					break
				}
			}

			sc.completedTools = append(sc.completedTools, tp)
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
		// Reset state for the next turn (same session, new response).
		sc.resetForNextTurn()

	case jcodeproto.EventError:
		var e jcodeproto.ErrorEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			sc.textBuffer.WriteString(fmt.Sprintf("\n\n❌ *Error:* %s", e.Message))
			sc.dirty = true
		}
		sc.finalized = true
		sc.flushLocked(true)
		sc.resetForNextTurn()

	case jcodeproto.EventInterrupted:
		sc.textBuffer.WriteString("\n\n⚠️ _Agent interrupted_")
		sc.dirty = true
		sc.finalized = true
		sc.flushLocked(true)
		sc.resetForNextTurn()

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

// resetForNextTurn clears per-turn state so the coalescer is ready for
// subsequent messages on the same session. Must be called with mu held.
func (sc *SessionCoalescer) resetForNextTurn() {
	sc.textBuffer.Reset()
	sc.pendingTools = nil
	sc.completedTools = nil
	sc.toolInputBuf.Reset()
	sc.directiveBlocks = nil
	sc.directiveFallback = ""
	sc.progressMessageTS = nil // next turn gets a new Slack message
	sc.firstTurn = false       // header only on the first turn
	sc.finalized = false
	sc.dirty = false
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
	if text == "" && len(sc.directiveBlocks) == 0 && !isFinal {
		return
	}

	// If text is empty but we have directive blocks, use the fallback text
	// so Slack notifications show something meaningful.
	if text == "" && len(sc.directiveBlocks) > 0 {
		if sc.directiveFallback != "" {
			text = sc.directiveFallback
		} else {
			text = " " // Slack requires non-empty text
		}
	}

	// Don't create a new Slack message if there's no meaningful content.
	// This avoids posting header-only messages during recovery when a
	// stale "done" event arrives for a turn that already completed.
	if sc.progressMessageTS == nil && sc.textBuffer.Len() == 0 && len(sc.pendingTools) == 0 && len(sc.completedTools) == 0 {
		sc.dirty = false
		return
	}

	priority := 2 // chat.update
	if isFinal {
		priority = 1 // terminal flush
	}

	// When directive blocks are present, skip username/icon override.
	// Slack strips Block Kit from messages sent with chat:write.customize.
	username := sc.identity.DisplayName
	iconURL := sc.identity.IconURL
	if len(sc.directiveBlocks) > 0 {
		username = ""
		iconURL = ""
	}

	if sc.progressMessageTS == nil {
		// First flush: PostMessage to create the progress message.
		// Mark as "awaiting TS" immediately to prevent concurrent flushes
		// (e.g. timer goroutine) from also posting while we wait.
		placeholder := ""
		sc.progressMessageTS = &placeholder

		tsCh := make(chan string, 1)
		item := &outbound.OutboundItem{
			Priority:  3, // chat.postMessage
			ChannelID: sc.channelID,
			ThreadTS:  sc.threadTS,
			Action:    outbound.ActionPostMessage,
			Text:      text,
			Blocks:    sc.directiveBlocks,
			Username:  username,
			IconURL:   iconURL,
			OnPosted: func(ts string) {
				sc.SetProgressMessageTS(ts)
				tsCh <- ts
			},
		}
		sc.outbound.Enqueue(item)
		// Wait for the TS to come back (unlock mu while waiting to avoid deadlock).
		sc.mu.Unlock()
		select {
		case <-tsCh:
		case <-time.After(10 * time.Second):
			slog.Warn("coalescer: timed out waiting for PostMessage TS", "session_id", sc.sessionID)
		}
		sc.mu.Lock()
	} else if *sc.progressMessageTS != "" {
		// Subsequent flush: UpdateMessage.
		item := &outbound.OutboundItem{
			Priority:  priority,
			ChannelID: sc.channelID,
			Action:    outbound.ActionUpdateMessage,
			Text:      text,
			Blocks:    sc.directiveBlocks,
			MessageTS: *sc.progressMessageTS,
			Username:  username,
			IconURL:   iconURL,
		}
		sc.outbound.Enqueue(item)
	}
	// else: progressMessageTS is placeholder ("") — still waiting for initial
	// PostMessage TS. Skip this flush; the pending post will include latest content.

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
// Uses Slack mrkdwn format (not standard Markdown).
// Also processes any render directives in the text buffer, accumulating blocks.
func (sc *SessionCoalescer) renderMessage(isFinal bool) string {
	var sb strings.Builder

	// Reset directive blocks before re-extracting from the full buffer.
	// This prevents duplication across incremental flushes.
	sc.directiveBlocks = nil
	sc.directiveFallback = ""

	// Header only on the first turn of a new session.
	if sc.firstTurn {
		emoji := "🤖"
		name := sc.friendlyName
		if name == "" && len(sc.sessionID) > 8 {
			name = sc.sessionID[:8]
		} else if name == "" {
			name = sc.sessionID
		}
		workdirBase := filepath.Base(sc.workdir)
		sb.WriteString(fmt.Sprintf("*%s %s @ %s*\n\n", emoji, name, workdirBase))
	}

	// Text content: extract directives, then convert remaining Markdown → mrkdwn.
	text := sc.textBuffer.String()
	if text != "" {
		// Process render directives if any are present.
		if render.HasDirectives(text) {
			result := render.ExtractDirectives(text, false)
			text = result.CleanText
			if len(result.Blocks) > 0 {
				sc.directiveBlocks = append(sc.directiveBlocks, result.Blocks...)
			}
			if result.FallbackText != "" {
				sc.directiveFallback = result.FallbackText
			}
		}

		if text != "" {
			sb.WriteString(MarkdownToMrkdwn(text))
			sb.WriteString("\n")
		}
	}

	// Tool summary (completed).
	if len(sc.completedTools) > 0 {
		sb.WriteString("\n")
		for _, t := range sc.completedTools {
			label := t.Description
			if label == "" {
				label = t.Name
			}
			if t.Error != "" {
				sb.WriteString(fmt.Sprintf("❌ %s\n", label))
			} else {
				sb.WriteString(fmt.Sprintf("%s %s\n", toolCheckmark, label))
			}
		}
	}

	// Pending tools.
	if len(sc.pendingTools) > 0 {
		sb.WriteString("\n")
		for _, t := range sc.pendingTools {
			label := t.Description
			if label == "" {
				label = t.Name
			}
			status := "starting"
			if t.Exec {
				status = "running"
			}
			sb.WriteString(fmt.Sprintf("%s %s (%s)\n", toolSpinner, label, status))
		}
	}

	// Provider footer (final flush only, if non-default).
	if isFinal && sc.upstreamProvider != nil {
		sb.WriteString(fmt.Sprintf("\n_%s_\n", *sc.upstreamProvider))
	}

	return sb.String()
}
