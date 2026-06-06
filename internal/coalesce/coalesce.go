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
	// maxSlackTextLen is the threshold at which we split into a new message.
	// Slack's chat.update limit is ~4000 chars for the text field, but the
	// rendered message includes headers and tool descriptions on top of the
	// raw text buffer. We use 3000 as a safe buffer-length threshold to
	// account for the rendering overhead.
	maxSlackTextLen = 3000
	// toolCheckmark for completed tools.
	toolCheckmark = "✓"
	// toolSpinner for in-progress tools.
	toolSpinner = "⏳"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// segmentKind discriminates between text and tool entries in the output stream.
type segmentKind int

const (
	segText segmentKind = iota
	segTool
)

// segment is a single unit in the interleaved output stream: either a chunk
// of text or a completed tool entry.
type segment struct {
	kind segmentKind

	// Text segment fields.
	text strings.Builder

	// Tool segment fields.
	description string // human-friendly label
	isError     bool
	count       int // >=1; incremented for sequential dedup
}

// ToolProgress tracks a single in-flight tool invocation.
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

	// segments is an ordered stream of text chunks and completed tool entries,
	// interleaved in the order they were produced. This replaces the old
	// separate textBuffer + completedTools lists.
	segments []segment

	pendingTools  []ToolProgress
	toolInputBufs map[string]*strings.Builder // per-tool-ID input accumulator

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

	// strictDirectives controls whether render directive validation is strict.
	strictDirectives bool

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
		sessionID:     sessionID,
		friendlyName:  friendlyName,
		channelID:     channelID,
		threadTS:      threadTS,
		workdir:       workdir,
		identity:      identity,
		outbound:      out,
		onImage:       onImage,
		firstTurn:     true,
		lastFlush:     time.Now(),
		toolInputBufs: make(map[string]*strings.Builder),
		driftMonitor:  render.NewDriftMonitor(7), // default threshold
		done:          make(chan struct{}),
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

// ---------------------------------------------------------------------------
// Segment helpers (must be called with mu held)
// ---------------------------------------------------------------------------

// currentTextSegment returns the trailing text segment, creating one if the
// last segment is not a text segment.
func (sc *SessionCoalescer) currentTextSegment() *segment {
	if len(sc.segments) > 0 {
		last := &sc.segments[len(sc.segments)-1]
		if last.kind == segText {
			return last
		}
	}
	sc.segments = append(sc.segments, segment{kind: segText})
	return &sc.segments[len(sc.segments)-1]
}

// appendToolSegment adds a completed tool entry. If the previous segment is
// a tool with the same description (and no error), increment its count
// instead of creating a new entry (sequential dedup).
func (sc *SessionCoalescer) appendToolSegment(description string, isError bool) {
	if !isError && len(sc.segments) > 0 {
		last := &sc.segments[len(sc.segments)-1]
		if last.kind == segTool && !last.isError && last.description == description {
			last.count++
			return
		}
	}
	sc.segments = append(sc.segments, segment{
		kind:        segTool,
		description: description,
		isError:     isError,
		count:       1,
	})
}

// allText concatenates all text segments (used for directive extraction).
func (sc *SessionCoalescer) allText() string {
	var sb strings.Builder
	for i := range sc.segments {
		if sc.segments[i].kind == segText {
			sb.WriteString(sc.segments[i].text.String())
		}
	}
	return sb.String()
}

// hasContent returns true if there's any meaningful content to render.
func (sc *SessionCoalescer) hasContent() bool {
	// Directive blocks count as content (set by renderMessage).
	if len(sc.directiveBlocks) > 0 {
		return true
	}
	for i := range sc.segments {
		switch sc.segments[i].kind {
		case segText:
			if sc.segments[i].text.Len() > 0 {
				return true
			}
		case segTool:
			return true
		}
	}
	return len(sc.pendingTools) > 0
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
			seg := sc.currentTextSegment()
			seg.text.WriteString(e.Text)
			sc.dirty = true
			sc.checkOverflow()
		}

	case jcodeproto.EventTextReplace:
		var e jcodeproto.TextReplaceEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			// Replace only text segments; preserve tool segments.
			kept := sc.segments[:0]
			for i := range sc.segments {
				if sc.segments[i].kind == segTool {
					kept = append(kept, sc.segments[i])
				}
			}
			sc.segments = kept
			seg := sc.currentTextSegment()
			seg.text.WriteString(e.Text)
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
			// Route input to the last pending tool that hasn't been exec'd yet.
			// tool_input events don't carry a tool ID, so we assume input
			// belongs to the most recently started non-exec tool.
			var targetID string
			for i := len(sc.pendingTools) - 1; i >= 0; i-- {
				if !sc.pendingTools[i].Exec {
					targetID = sc.pendingTools[i].ID
					break
				}
			}
			if targetID != "" {
				buf, ok := sc.toolInputBufs[targetID]
				if !ok {
					buf = &strings.Builder{}
					sc.toolInputBufs[targetID] = buf
				}
				buf.WriteString(e.Delta)
			}
		}

	case jcodeproto.EventToolExec:
		var e jcodeproto.ToolExecEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					sc.pendingTools[i].Exec = true
					// Parse accumulated tool input and update description.
					if buf, ok := sc.toolInputBufs[e.ID]; ok && buf.Len() > 0 {
						rawInput := buf.String()
						var input map[string]any
						if err := json.Unmarshal([]byte(rawInput), &input); err == nil {
							desc := render.Describe(e.Name, input)
							sc.driftMonitor.Record(desc)
							sc.pendingTools[i].Description = desc
						} else {
							slog.Debug("coalescer: failed to parse tool input",
								"tool", e.Name, "err", err, "raw_len", len(rawInput))
						}
						delete(sc.toolInputBufs, e.ID)
					}
					break
				}
			}
			sc.dirty = true
		}

	case jcodeproto.EventToolDone:
		var e jcodeproto.ToolDoneEvent
		if json.Unmarshal(ev.Raw, &e) == nil {
			// Resolve description from the pending tool entry.
			desc := e.Name
			isError := e.Error != nil
			for i := range sc.pendingTools {
				if sc.pendingTools[i].ID == e.ID {
					desc = sc.pendingTools[i].Description
					sc.pendingTools = append(sc.pendingTools[:i], sc.pendingTools[i+1:]...)
					delete(sc.toolInputBufs, e.ID) // clean up any remaining input buffer
					break
				}
			}
			if desc == "" {
				desc = e.Name
			}

			// Add to the interleaved stream (with sequential dedup).
			sc.appendToolSegment(desc, isError)
			sc.dirty = true
			sc.checkOverflow()
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
			seg := sc.currentTextSegment()
			seg.text.WriteString(fmt.Sprintf("\n\n❌ *Error:* %s", e.Message))
			sc.dirty = true
		}
		sc.finalized = true
		sc.flushLocked(true)
		sc.resetForNextTurn()

	case jcodeproto.EventInterrupted:
		seg := sc.currentTextSegment()
		seg.text.WriteString("\n\n⚠️ _Agent interrupted_")
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
			seg := sc.currentTextSegment()
			seg.text.WriteString(fmt.Sprintf("\n\n📢 _%s: %s_", fromName, e.Message))
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

// SetStrictDirectives enables or disables strict render directive validation.
func (sc *SessionCoalescer) SetStrictDirectives(strict bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.strictDirectives = strict
}

// resetForNextTurn clears per-turn state so the coalescer is ready for
// subsequent messages on the same session. Must be called with mu held.
func (sc *SessionCoalescer) resetForNextTurn() {
	sc.segments = sc.segments[:0]
	sc.pendingTools = nil
	// Clear all per-tool input buffers.
	for k := range sc.toolInputBufs {
		delete(sc.toolInputBufs, k)
	}
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
	if sc.progressMessageTS == nil && !sc.hasContent() {
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
// It checks the estimated rendered length (text + inline tool summaries + overhead)
// rather than just the raw text, since tool summaries can accumulate.
func (sc *SessionCoalescer) checkOverflow() {
	estimated := sc.estimateRenderedLen()
	if estimated > maxSlackTextLen {
		// Flush current content as a finalized message, then reset.
		sc.flushLocked(false)
		// Start new progress message.
		sc.progressMessageTS = nil
		sc.segments = sc.segments[:0]
	}
}

// estimateRenderedLen returns an approximate character count for the rendered
// message, accounting for all segments, header, pending tools, and footer.
func (sc *SessionCoalescer) estimateRenderedLen() int {
	n := 0
	// Header (first turn only).
	if sc.firstTurn {
		n += 40
	}
	// Segments (text + inline tool summaries).
	for i := range sc.segments {
		switch sc.segments[i].kind {
		case segText:
			n += sc.segments[i].text.Len()
		case segTool:
			n += len(sc.segments[i].description) + 10 // emoji + count + newline
		}
	}
	// Pending tool summaries.
	for _, t := range sc.pendingTools {
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		n += len(desc) + 20 // emoji + status + newline
	}
	// Provider footer.
	if sc.upstreamProvider != nil {
		n += len(*sc.upstreamProvider) + 6
	}
	return n
}

// renderMessage constructs the Slack message text from current state.
// Text and tool summaries are interleaved in the order they were produced.
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

	// Check for directives across all text content.
	fullText := sc.allText()
	var directiveResult *render.DirectiveResult
	if fullText != "" && render.HasDirectives(fullText) {
		result := render.ExtractDirectives(fullText, sc.strictDirectives)
		directiveResult = &result
		if len(result.Blocks) > 0 {
			sc.directiveBlocks = append(sc.directiveBlocks, result.Blocks...)
		}
		if result.FallbackText != "" {
			sc.directiveFallback = result.FallbackText
		}
	}

	// Render segments in order.
	// When directives are present, we replace all text segments with the
	// cleaned text (directives stripped) while keeping tool segments in their
	// original interleaved positions. The first text segment gets the clean
	// text; subsequent text segments are skipped (their content is already
	// included in the concatenated clean text).
	if directiveResult != nil {
		cleanTextWritten := false
		for i := range sc.segments {
			seg := &sc.segments[i]
			switch seg.kind {
			case segText:
				if !cleanTextWritten {
					cleanTextWritten = true
					if directiveResult.CleanText != "" {
						sb.WriteString(MarkdownToMrkdwn(directiveResult.CleanText))
						if !strings.HasSuffix(directiveResult.CleanText, "\n") {
							sb.WriteString("\n")
						}
					}
				}
				// Skip subsequent text segments (already merged into CleanText).
			case segTool:
				sc.renderToolSegment(&sb, seg)
			}
		}
		// If there were no text segments at all (unlikely), still write clean text.
		if !cleanTextWritten && directiveResult.CleanText != "" {
			sb.WriteString(MarkdownToMrkdwn(directiveResult.CleanText))
			sb.WriteString("\n")
		}
	} else {
		for i := range sc.segments {
			seg := &sc.segments[i]
			switch seg.kind {
			case segText:
				text := seg.text.String()
				if text != "" {
					sb.WriteString(MarkdownToMrkdwn(text))
					// Only add trailing newline if the text doesn't already end with one.
					if !strings.HasSuffix(text, "\n") {
						sb.WriteString("\n")
					}
				}
			case segTool:
				sc.renderToolSegment(&sb, seg)
			}
		}
	}

	// Pending tools (always at the end, since they're in-progress).
	if len(sc.pendingTools) > 0 {
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

// renderToolSegment writes a single tool entry to the builder, including
// sequential dedup count if > 1.
func (sc *SessionCoalescer) renderToolSegment(sb *strings.Builder, seg *segment) {
	label := seg.description
	if seg.isError {
		if seg.count > 1 {
			sb.WriteString(fmt.Sprintf("❌ %s ×%d\n", label, seg.count))
		} else {
			sb.WriteString(fmt.Sprintf("❌ %s\n", label))
		}
	} else {
		if seg.count > 1 {
			sb.WriteString(fmt.Sprintf("%s %s ×%d\n", toolCheckmark, label, seg.count))
		} else {
			sb.WriteString(fmt.Sprintf("%s %s\n", toolCheckmark, label))
		}
	}
}
