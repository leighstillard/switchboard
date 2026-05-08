// Package router implements the decision-making core of Switchboard.
// It receives inbound Slack messages and webhook events, orchestrates
// jcode session lifecycle, and routes notifications to the appropriate
// Slack threads.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/coalesce"
	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/jcodeproto"
	"github.com/format5/switchboard/internal/llmrouter"
	"github.com/format5/switchboard/internal/outbound"
	"github.com/format5/switchboard/internal/slack"
	"github.com/format5/switchboard/internal/store"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// WebhookEvent is the normalized representation of an inbound webhook.
type WebhookEvent struct {
	Source      string
	EventType   string
	Payload     map[string]interface{}
	Headers     map[string]string
	RawBody     []byte
	Idempotency string
}

// Router orchestrates message flow between Slack, jcode, and webhook sources.
type Router struct {
	cfg      *config.Config
	store    *store.Store
	jcode    *jcode.Client
	edge     *slack.Edge
	outbound *outbound.Queue

	mu         sync.RWMutex
	routes     []config.RouteConfig
	coalescers map[string]*coalesce.SessionCoalescer // key: "channelID:threadTS"
	// coalescerQueue maps jcode session ID to an ordered queue of coalescer keys.
	// Events are routed to the first entry. On "done", the front is popped so
	// subsequent turns route to the next waiting coalescer (thread).
	coalescerQueue map[string][]string // jcodeSessionID -> []coalescerKey

	// turnRequester tracks which user triggered the current active turn,
	// keyed by coalescerKey. Used to @mention them when the turn completes.
	turnRequester map[string]string // coalescerKey -> userID

	inboundCh chan *slack.InboundMessage
	webhookCh chan *WebhookEvent

	llmRouter          *llmrouter.Router
	maxQueuePerSession int
	configPath         string
}

// New creates a Router with the given dependencies.
func New(
	cfg *config.Config,
	st *store.Store,
	jc *jcode.Client,
	edge *slack.Edge,
	out *outbound.Queue,
	configPath string,
) *Router {
	r := &Router{
		cfg:                cfg,
		store:              st,
		jcode:              jc,
		edge:               edge,
		outbound:           out,
		routes:             cfg.Routes,
		coalescers:         make(map[string]*coalesce.SessionCoalescer),
		coalescerQueue:     make(map[string][]string),
		turnRequester:      make(map[string]string),
		inboundCh:          make(chan *slack.InboundMessage, 64),
		webhookCh:          make(chan *WebhookEvent, 64),
		maxQueuePerSession: 5,
		configPath:         configPath,
	}

	// Initialize LLM router if configured.
	if cfg.Routing.LLM.Enabled {
		llmCfg := llmrouter.DefaultConfig()
		llmCfg.Enabled = true
		llmCfg.APIKey = cfg.Routing.LLM.APIKey
		if cfg.Routing.LLM.Model != "" {
			llmCfg.Model = cfg.Routing.LLM.Model
		}
		if cfg.Routing.LLM.ConfidenceThreshold > 0 {
			llmCfg.ConfidenceThreshold = cfg.Routing.LLM.ConfidenceThreshold
		}
		if cfg.Routing.LLM.MaxInputTokens > 0 {
			llmCfg.MaxInputTokens = cfg.Routing.LLM.MaxInputTokens
		}
		if cfg.Routing.LLM.IncludeThreadCount > 0 {
			llmCfg.IncludeThreadCount = cfg.Routing.LLM.IncludeThreadCount
		}
		if cfg.Routing.LLM.MonthlyBudgetUSD > 0 {
			llmCfg.MonthlyBudgetUSD = cfg.Routing.LLM.MonthlyBudgetUSD
		}
		r.llmRouter = llmrouter.New(llmCfg)
		slog.Info("router: LLM router enabled", "model", llmCfg.Model, "threshold", llmCfg.ConfidenceThreshold)
	}

	// Register as the inbound handler for Slack events.
	edge.SetInboundHandler(func(msg *slack.InboundMessage) {
		select {
		case r.inboundCh <- msg:
		default:
			slog.Warn("router: inbound channel full, dropping message",
				"channel", msg.ChannelID, "user", msg.UserID)
		}
	})

	return r
}

// Run starts the router event loop. It blocks until ctx is cancelled.
func (r *Router) Run(ctx context.Context) error {
	slog.Info("router: starting")

	// Recover active sessions from store.
	if err := r.recoverSessions(ctx); err != nil {
		slog.Error("router: session recovery failed", "err", err)
		// Non-fatal; continue with no recovered sessions.
	}

	// Start maintenance ticker (nightly cleanup).
	maintenanceTicker := time.NewTicker(6 * time.Hour)
	defer maintenanceTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("router: stopping")
			r.closeAllCoalescers()
			return ctx.Err()

		case msg := <-r.inboundCh:
			r.handleInbound(ctx, msg)

		case evt := <-r.webhookCh:
			r.handleWebhook(ctx, evt)

		case <-maintenanceTicker.C:
			r.runMaintenance()
		}
	}
}

// EnqueueWebhook submits a webhook event for processing by the router.
func (r *Router) EnqueueWebhook(evt *WebhookEvent) {
	select {
	case r.webhookCh <- evt:
	default:
		slog.Warn("router: webhook channel full", "source", evt.Source)
	}
}

// InjectMessage simulates an inbound Slack message for testing.
// This bypasses the Slack edge entirely and injects directly into the router.
func (r *Router) InjectMessage(channelID, threadTS, userID, text string) string {
	ts := fmt.Sprintf("%d.%06d", time.Now().Unix(), time.Now().Nanosecond()/1000)
	msg := &slack.InboundMessage{
		ChannelID:  channelID,
		ThreadTS:   threadTS,
		MessageTS:  ts,
		UserID:     userID,
		Text:       text,
		IsTopLevel: threadTS == "",
	}
	select {
	case r.inboundCh <- msg:
	default:
		slog.Warn("router: inject message dropped (channel full)")
	}
	return ts
}

// Reload updates the full configuration (called on SIGHUP).
// This replaces routes, channels, GitHub config, and identities.
func (r *Router) Reload(newCfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = newCfg
	r.routes = newCfg.Routes

	// Recreate or tear down LLM router based on new config.
	if newCfg.Routing.LLM.Enabled {
		llmCfg := llmrouter.DefaultConfig()
		llmCfg.Enabled = true
		llmCfg.APIKey = newCfg.Routing.LLM.APIKey
		if newCfg.Routing.LLM.Model != "" {
			llmCfg.Model = newCfg.Routing.LLM.Model
		}
		if newCfg.Routing.LLM.ConfidenceThreshold > 0 {
			llmCfg.ConfidenceThreshold = newCfg.Routing.LLM.ConfidenceThreshold
		}
		if newCfg.Routing.LLM.MaxInputTokens > 0 {
			llmCfg.MaxInputTokens = newCfg.Routing.LLM.MaxInputTokens
		}
		if newCfg.Routing.LLM.IncludeThreadCount > 0 {
			llmCfg.IncludeThreadCount = newCfg.Routing.LLM.IncludeThreadCount
		}
		if newCfg.Routing.LLM.MonthlyBudgetUSD > 0 {
			llmCfg.MonthlyBudgetUSD = newCfg.Routing.LLM.MonthlyBudgetUSD
		}
		r.llmRouter = llmrouter.New(llmCfg)
		slog.Info("router: LLM router (re)enabled on reload", "model", llmCfg.Model)
	} else {
		r.llmRouter = nil
	}

	slog.Info("router: config reloaded",
		"routes", len(newCfg.Routes),
		"channels", len(newCfg.Channels),
		"github_repos", len(newCfg.GitHub.Repos),
	)
}

// ---------------------------------------------------------------------------
// Inbound Slack message handling
// ---------------------------------------------------------------------------

func (r *Router) handleInbound(ctx context.Context, msg *slack.InboundMessage) {
	slog.Debug("router: inbound message",
		"channel", msg.ChannelID,
		"thread_ts", msg.ThreadTS,
		"user", msg.UserID,
		"top_level", msg.IsTopLevel,
	)

	// Check for commands.
	if r.handleCommand(ctx, msg) {
		return
	}

	// Resolve workdir for this channel.
	workdir, identity := r.resolveChannel(msg.ChannelID)
	if workdir == "" {
		if msg.IsAppMention && msg.UserID != "" {
			r.tryAutoOnboard(ctx, msg)
		} else if msg.UserID != "" {
			slog.Info("router: no workdir for channel (non-mention), ignoring", "channel", msg.ChannelID, "user", msg.UserID)
		}
		return
	}

	// Determine the thread_ts to use as the session key.
	threadTS := msg.ThreadTS
	if msg.IsTopLevel {
		threadTS = msg.MessageTS // top-level message: its own TS becomes the thread
	}

	// Look up existing session.
	session, err := r.store.GetSession(msg.ChannelID, threadTS)
	if err != nil {
		slog.Error("router: store lookup failed", "err", err)
		return
	}

	if session != nil {
		r.handleContinuation(ctx, msg, session, threadTS)
		return
	}

	// No existing session. Only create new sessions from top-level messages
	// (or DMs, or explicit @mentions in threads we don't own).
	if !msg.IsTopLevel && !msg.IsDM {
		// Reply in a thread we don't own - ignore.
		slog.Debug("router: reply in unowned thread, ignoring", "channel", msg.ChannelID, "thread_ts", threadTS)
		return
	}

	r.handleNewSession(ctx, msg, workdir, identity, threadTS)
}

// tryAutoOnboard attempts to auto-configure a channel when an admin mentions
// the bot in an unconfigured channel with a matching workspace directory.
func (r *Router) tryAutoOnboard(ctx context.Context, msg *slack.InboundMessage) {
	// Check if user is admin.
	isAdmin, err := r.edge.IsUserAdmin(msg.UserID)
	if err != nil {
		slog.Warn("router: auto-onboard: failed to check admin status", "user", msg.UserID, "err", err)
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I got your mention in <#%s> but couldn't verify your permissions. Talk to me here in DMs!",
			msg.ChannelID))
		return
	}
	if !isAdmin {
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I got your mention in <#%s> but I'm not configured for that channel yet. Only workspace admins can onboard new channels — ask an admin or talk to me here in DMs!",
			msg.ChannelID))
		return
	}

	// Look up channel name from Slack API.
	channelName, err := r.edge.GetChannelInfo(msg.ChannelID)
	if err != nil {
		slog.Error("router: auto-onboard: failed to get channel info", "channel", msg.ChannelID, "err", err)
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I got your mention in <#%s> but couldn't look up the channel info. Talk to me here in DMs!",
			msg.ChannelID))
		return
	}

	// Check for matching workspace directory.
	home, _ := os.UserHomeDir()
	workspacePath := filepath.Join(home, "workspace", channelName)
	if info, err := os.Stat(workspacePath); err != nil || !info.IsDir() {
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I got your mention in <#%s> (`#%s`), and you're an admin, but there's no matching workspace directory (`~/workspace/%s`). Create the directory first, then mention me again!",
			msg.ChannelID, channelName, channelName))
		return
	}

	// Build identity from channel name: "cc-connect" -> "CC Connect"
	identity := titleCase(channelName)

	newCh := config.ChannelConfig{
		ID:       msg.ChannelID,
		Name:     channelName,
		Workdir:  workspacePath,
		Identity: identity,
	}

	// Insert into config file.
	if err := config.InsertChannel(r.configPath, newCh); err != nil {
		slog.Error("router: auto-onboard: failed to insert channel", "err", err)
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I got your mention in <#%s> and everything checks out, but I couldn't update my config file. Check the logs!",
			msg.ChannelID))
		return
	}

	// Reload config.
	newCfg, err := config.Load(r.configPath)
	if err != nil {
		slog.Error("router: auto-onboard: config reload failed", "err", err)
		r.edge.DMUser(msg.UserID, fmt.Sprintf(
			"I added <#%s> to my config but couldn't reload. A restart will pick it up!",
			msg.ChannelID))
		return
	}

	r.Reload(newCfg)
	r.edge.ReloadConfig(newCfg.Channels, newCfg.Identities)

	slog.Info("router: auto-onboarded channel",
		"channel_id", msg.ChannelID,
		"channel_name", channelName,
		"workdir", workspacePath,
		"onboarded_by", msg.UserID)

	// Confirm in channel then process the original message.
	r.edge.PostMessage(msg.ChannelID, fmt.Sprintf(
		"✅ Channel onboarded! `#%s` → `~/workspace/%s`\nProcessing your message now...",
		channelName, channelName))

	// Re-dispatch the original message now that the channel is configured.
	r.handleInbound(ctx, msg)
}

// titleCase converts "foo-bar-baz" to "Foo Bar Baz".
func titleCase(s string) string {
	words := strings.Split(s, "-")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// handleNewSession creates a new jcode session and starts processing.
func (r *Router) handleNewSession(ctx context.Context, msg *slack.InboundMessage, workdir string, identity coalesce.Identity, threadTS string) {
	slog.Info("router: creating new session",
		"channel", msg.ChannelID,
		"thread_ts", threadTS,
		"workdir", workdir,
	)

	// Audit the inbound message.
	r.auditSlackMessage(msg, threadTS)

	// Subscribe to a new jcode session (may reuse existing for same workdir).
	sessionID, events, err := r.jcode.Subscribe(ctx, workdir)
	if err != nil {
		slog.Error("router: failed to subscribe to jcode", "err", err, "workdir", workdir)
		r.postError(msg.ChannelID, threadTS, "Agent is temporarily unavailable. Try again in a moment.")
		return
	}

	// Check if this session already has a consumer running (reused workdir).
	existingSession, _ := r.store.GetSessionByJcodeID(sessionID)
	isReused := existingSession != nil

	// Persist session.
	now := time.Now().Unix()
	expiresAt := now + 30*24*3600 // 30 days
	sess := &store.Session{
		ChannelID:    msg.ChannelID,
		ThreadTS:     threadTS,
		JcodeSession: sessionID,
		Workdir:      workdir,
		CreatedAt:    now,
		LastActivity: now,
		Status:       "processing",
		ExpiresAt:    &expiresAt,
	}
	if err := r.store.CreateSession(sess); err != nil {
		slog.Error("router: failed to persist session", "err", err)
	}

	// Create coalescer for this thread.
	friendlyName := extractFriendlyName(sessionID)
	coal := coalesce.NewSessionCoalescer(
		sessionID, friendlyName, msg.ChannelID, threadTS, workdir,
		identity, r.outbound, r.handleImage,
	)
	coal.SetStrictDirectives(r.cfg.Render.StrictDirectiveValidation)
	key := coalescerKey(msg.ChannelID, threadTS)
	r.mu.Lock()
	r.coalescers[key] = coal
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], key)
	r.turnRequester[key] = msg.UserID
	r.mu.Unlock()

	// Add 👀 reaction to acknowledge receipt.
	r.edge.AddReaction(msg.ChannelID, msg.MessageTS, "eyes")

	// If reusing an existing session that is currently processing another turn,
	// queue the message rather than sending immediately. This prevents jcode
	// from mixing the response into the wrong thread's coalescer.
	if isReused && existingSession.Status == "processing" {
		slog.Info("router: session busy, queueing message for new thread",
			"session_id", sessionID,
			"thread_ts", threadTS,
			"existing_thread", existingSession.ThreadTS,
		)
		r.store.EnqueueTurn(&store.TurnQueueItem{
			ChannelID:  msg.ChannelID,
			ThreadTS:   threadTS,
			EnqueuedAt: time.Now().Unix(),
			UserID:     msg.UserID,
			Text:       msg.Text,
		})
		return
	}

	// Send the user's message to jcode.
	var images []jcodeproto.ImagePair
	// TODO: handle file attachments -> images
	if err := r.jcode.SendMessage(ctx, sessionID, msg.Text, images); err != nil {
		slog.Error("router: failed to send message to jcode", "err", err)
		r.postError(msg.ChannelID, threadTS, "Failed to send message to agent: "+err.Error())
		return
	}

	// Only start event consumer if this is a fresh session (not reused).
	// Reused sessions already have an event consumer from recovery or first use.
	if !isReused {
		go r.consumeEvents(ctx, sessionID, events)
	} else {
		slog.Info("router: reusing existing jcode session", "session_id", sessionID, "new_thread", threadTS)
	}
}

// handleContinuation routes a message to an existing session.
func (r *Router) handleContinuation(ctx context.Context, msg *slack.InboundMessage, session *store.Session, threadTS string) {
	// If the message @mentions another user and does NOT mention the bot,
	// treat it as directed at that other user and ignore it. This prevents
	// the bot from responding to e.g. "@alice can you review this?" in an
	// owned thread. DMs are exempt (always process).
	if !msg.IsDM && msg.MentionsOther && !msg.MentionsBot {
		slog.Debug("router: ignoring thread reply mentioning other user",
			"channel", msg.ChannelID, "thread_ts", threadTS, "user", msg.UserID)
		return
	}

	// Audit the inbound message.
	r.auditSlackMessage(msg, threadTS)

	// Check if expired.
	if session.Status == "closed" {
		r.outbound.Enqueue(&outbound.OutboundItem{
			Priority:  3,
			ChannelID: msg.ChannelID,
			ThreadTS:  threadTS,
			Action:    outbound.ActionPostMessage,
			Text:      "This thread has aged out (last activity > 30 days ago). Mention me at the channel root to start fresh.",
		})
		return
	}

	// Update activity timestamp.
	r.store.UpdateSessionActivity(msg.ChannelID, threadTS)

	if session.Status == "idle" {
		// Session is idle: activate it.
		r.store.UpdateSessionStatus(msg.ChannelID, threadTS, "processing")

		// Add 👀 reaction.
		r.edge.AddReaction(msg.ChannelID, msg.MessageTS, "eyes")

		// Send message to jcode. If the session is stale (e.g. jcode restarted),
		// attempt to re-subscribe before giving up.
		var images []jcodeproto.ImagePair
		if err := r.jcode.SendMessage(ctx, session.JcodeSession, msg.Text, images); err != nil {
			slog.Warn("router: send failed, attempting re-subscribe",
				"session_id", session.JcodeSession, "err", err)

			events, subErr := r.jcode.SubscribeExisting(ctx, session.JcodeSession, session.Workdir)
			if subErr != nil {
				// Session is truly gone. Transparently create a new session
				// in the same thread so the user doesn't notice the interruption.
				slog.Info("router: session gone, transparently creating replacement",
					"old_session_id", session.JcodeSession,
					"channel", msg.ChannelID,
					"thread_ts", threadTS)
				r.store.UpdateSessionStatus(msg.ChannelID, threadTS, "idle")
				if err := r.store.DeleteSession(msg.ChannelID, threadTS); err != nil {
					slog.Warn("router: failed to delete stale session before replacement",
						"channel", msg.ChannelID, "thread_ts", threadTS, "err", err)
				}
				r.handleNewSession(ctx, msg, session.Workdir, r.resolveIdentity(msg.ChannelID), threadTS)
				return
			}

			// Re-subscribe succeeded; start event consumer and retry send.
			go r.consumeEvents(ctx, session.JcodeSession, events)
			if err := r.jcode.SendMessage(ctx, session.JcodeSession, msg.Text, images); err != nil {
				// Send still fails after re-subscribe. Create replacement session.
				slog.Warn("router: send failed after re-subscribe, creating replacement",
					"session_id", session.JcodeSession, "err", err)
				r.store.UpdateSessionStatus(msg.ChannelID, threadTS, "idle")
				if err := r.store.DeleteSession(msg.ChannelID, threadTS); err != nil {
					slog.Warn("router: failed to delete session before replacement",
						"channel", msg.ChannelID, "thread_ts", threadTS, "err", err)
				}
				r.handleNewSession(ctx, msg, session.Workdir, r.resolveIdentity(msg.ChannelID), threadTS)
				return
			}
		}

		// Ensure we have a coalescer and event consumer for this session.
		key := coalescerKey(msg.ChannelID, threadTS)
		r.mu.Lock()
		_, hasCoalescer := r.coalescers[key]
		if !hasCoalescer {
			// Re-create coalescer (e.g., after bridge restart).
			_, identity := r.resolveChannel(msg.ChannelID)
			coal := coalesce.NewSessionCoalescer(
				session.JcodeSession, session.FriendlyName,
				msg.ChannelID, threadTS, session.Workdir,
				identity, r.outbound, r.handleImage,
			)
			coal.SetStrictDirectives(r.cfg.Render.StrictDirectiveValidation)
			r.coalescers[key] = coal
		}
		// Ensure event consumer routes to this coalescer for the next turn.
		r.coalescerQueue[session.JcodeSession] = append(r.coalescerQueue[session.JcodeSession], key)
		r.turnRequester[key] = msg.UserID
		r.mu.Unlock()
	} else if session.Status == "processing" {
		// Session is busy: enqueue the message.
		count, _ := r.store.CountTurns(msg.ChannelID, threadTS)
		if count >= r.maxQueuePerSession {
			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  3,
				ChannelID: msg.ChannelID,
				ThreadTS:  threadTS,
				Action:    outbound.ActionPostMessage,
				Text:      fmt.Sprintf("I have a queue of %d messages waiting; further messages will be ignored until I catch up.", r.maxQueuePerSession),
			})
			return
		}

		// Enqueue the turn.
		item := &store.TurnQueueItem{
			ChannelID:  msg.ChannelID,
			ThreadTS:   threadTS,
			EnqueuedAt: time.Now().Unix(),
			UserID:     msg.UserID,
			Text:       msg.Text,
		}
		if err := r.store.EnqueueTurn(item); err != nil {
			slog.Error("router: failed to enqueue turn", "err", err)
			return
		}

		// Add 📋 reaction to acknowledge queuing.
		r.edge.AddReaction(msg.ChannelID, msg.MessageTS, "clipboard")
	}
}

// handleCommand checks for !stop, !cancel, !purge commands.
func (r *Router) handleCommand(ctx context.Context, msg *slack.InboundMessage) bool {
	text := strings.TrimSpace(msg.Text)

	threadTS := msg.ThreadTS
	if msg.IsTopLevel {
		threadTS = msg.MessageTS
	}

	switch {
	case text == "!stop" || text == "!cancel":
		session, err := r.store.GetSession(msg.ChannelID, threadTS)
		if err != nil || session == nil {
			return false
		}
		if session.Status == "processing" {
			if err := r.jcode.Cancel(ctx, session.JcodeSession); err != nil {
				slog.Error("router: cancel failed", "err", err)
			}
			r.store.UpdateSessionStatus(msg.ChannelID, threadTS, "idle")
			r.edge.AddReaction(msg.ChannelID, msg.MessageTS, "octagonal_sign")
		}
		return true

	case text == "!purge":
		session, err := r.store.GetSession(msg.ChannelID, threadTS)
		if err != nil || session == nil {
			return false
		}
		turns, _ := r.store.DrainTurns(msg.ChannelID, threadTS)
		r.edge.AddReaction(msg.ChannelID, msg.MessageTS, "wastebasket")
		if len(turns) > 0 {
			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  3,
				ChannelID: msg.ChannelID,
				ThreadTS:  threadTS,
				Action:    outbound.ActionPostMessage,
				Text:      fmt.Sprintf("Purged %d queued message(s).", len(turns)),
			})
		}
		return true
	}

	return false
}

// ---------------------------------------------------------------------------
// Event consumption from jcode
// ---------------------------------------------------------------------------

// consumeEvents reads from a jcode session event channel and dispatches to coalescer.
func (r *Router) consumeEvents(ctx context.Context, sessionID string, events <-chan *jcodeproto.ServerEvent) {
	slog.Info("router: consumeEvents started", "session_id", sessionID)
	for {
		select {
		case <-ctx.Done():
			slog.Info("router: consumeEvents exiting (ctx done)", "session_id", sessionID)
			return
		case ev, ok := <-events:
			if !ok {
				// Channel closed (session disconnected).
				slog.Info("router: event channel closed", "session_id", sessionID)
				return
			}

			// Look up the active coalescer for this jcode session (front of queue).
			r.mu.RLock()
			queue := r.coalescerQueue[sessionID]
			var key string
			if len(queue) > 0 {
				key = queue[0]
			}
			coal := r.coalescers[key]
			r.mu.RUnlock()

			slog.Debug("router: consumeEvents got event", "session_id", sessionID, "type", ev.Type, "key", key, "has_coal", coal != nil)

			if coal == nil {
				slog.Debug("router: no coalescer for event", "session_id", sessionID, "type", ev.Type)
				continue
			}

			// Handle session event (friendly name).
			if ev.Type == jcodeproto.EventSession {
				var sessEv jcodeproto.SessionEvent
				if json.Unmarshal(ev.Raw, &sessEv) == nil {
					coal.SetFriendlyName(sessEv.SessionID)
				}
				continue
			}

			// Handle history event (extract was_interrupted, then discard).
			if ev.Type == jcodeproto.EventHistory {
				var histEv jcodeproto.HistoryEvent
				if json.Unmarshal(ev.Raw, &histEv) == nil {
					if histEv.WasInterrupted != nil && *histEv.WasInterrupted {
						slog.Info("router: session was interrupted", "session_id", sessionID)
					}
				}
				continue
			}

			// Dispatch to coalescer.
			coal.HandleEvent(ev)

			// Handle turn completion.
			if ev.Type == jcodeproto.EventDone || ev.Type == jcodeproto.EventError || ev.Type == jcodeproto.EventInterrupted {
				// Pop the front of the coalescer queue so subsequent events
				// route to the next waiting thread.
				r.mu.Lock()
				if q := r.coalescerQueue[sessionID]; len(q) > 0 {
					r.coalescerQueue[sessionID] = q[1:]
				}
				r.mu.Unlock()

				r.handleTurnEnd(ctx, sessionID, key, ev.Type)
			}
		}
	}
}

// shouldNotifySuccess returns true if the event type warrants a ✅ success
// notification. Error and interrupted events are handled by the coalescer
// with their own messaging, so sending ✅ would be misleading.
func shouldNotifySuccess(eventType string) bool {
	return eventType == jcodeproto.EventDone
}

// dropNextKeyFromQueue removes the front entry from the coalescerQueue for the
// given sessionID, but only if it matches expectedKey. This is used to clean up
// after a failed SendMessage for a next-thread batch.
func dropNextKeyFromQueue(queue map[string][]string, sessionID, expectedKey string) {
	if q := queue[sessionID]; len(q) > 0 && q[0] == expectedKey {
		queue[sessionID] = q[1:]
	}
}

// isValidLLMThreadID checks that a thread_id string (format "channelID:threadTS")
// matches one of the ThreadContext entries that were provided to the LLM.
func isValidLLMThreadID(threadID string, threads []llmrouter.ThreadContext) bool {
	parts := strings.SplitN(threadID, ":", 2)
	if len(parts) != 2 {
		return false
	}
	channelID, threadTS := parts[0], parts[1]
	for _, tc := range threads {
		if tc.ChannelID == channelID && tc.ThreadTS == threadTS {
			return true
		}
	}
	return false
}

// handleTurnEnd drains the turn queue and either sends the next batch or marks idle.
func (r *Router) handleTurnEnd(ctx context.Context, sessionID, coalKey, eventType string) {
	// Parse channelID and threadTS from the coalescer key.
	channelID, threadTS := parseCoalescerKey(coalKey)
	if channelID == "" {
		slog.Error("router: invalid coalescer key in handleTurnEnd", "key", coalKey)
		return
	}

	// Drain queued turns.
	turns, err := r.store.DrainTurns(channelID, threadTS)
	if err != nil {
		slog.Error("router: drain turns failed", "err", err)
		r.store.UpdateSessionStatus(channelID, threadTS, "idle")
		return
	}

	if len(turns) == 0 {
		r.store.UpdateSessionStatus(channelID, threadTS, "idle")

		// Notify the requesting user that the turn is complete,
		// including the agent's final message text if available.
		// Only send success notification for EventDone; errors and
		// interruptions are already displayed by the coalescer.
		r.mu.Lock()
		requester := r.turnRequester[coalKey]
		delete(r.turnRequester, coalKey)
		coal := r.coalescers[coalKey]
		r.mu.Unlock()

		if requester != "" && shouldNotifySuccess(eventType) {
			notifyText := fmt.Sprintf("<@%s> ✅", requester)
			if coal != nil {
				if finalMsg := coal.FinalMessageText(); finalMsg != "" {
					// Convert markdown to Slack mrkdwn and truncate.
					finalMsg = coalesce.MarkdownToMrkdwn(finalMsg)
					if len(finalMsg) > 300 {
						finalMsg = finalMsg[:300] + "..."
					}
					notifyText = fmt.Sprintf("<@%s> ✅\n\n%s", requester, finalMsg)
				}
			}
			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  2,
				ChannelID: channelID,
				ThreadTS:  threadTS,
				Action:    outbound.ActionPostMessage,
				Text:      notifyText,
			})
		}

		// Check if the next entry in the coalescer queue has pending turns
		// (e.g. a new thread was created while this session was busy).
		r.mu.RLock()
		nextQueue := r.coalescerQueue[sessionID]
		var nextKey string
		if len(nextQueue) > 0 {
			nextKey = nextQueue[0]
		}
		r.mu.RUnlock()

		if nextKey != "" {
			nextCh, nextTS := parseCoalescerKey(nextKey)
			if nextCh != "" {
				nextTurns, _ := r.store.DrainTurns(nextCh, nextTS)
				if len(nextTurns) > 0 {
					var texts []string
					for _, t := range nextTurns {
						texts = append(texts, t.Text)
					}
					combined := strings.Join(texts, "\n\n---\n\n")

					r.store.UpdateSessionStatus(nextCh, nextTS, "processing")
					if err := r.jcode.SendMessage(ctx, sessionID, combined, nil); err != nil {
						slog.Warn("router: failed to send next-thread batch",
							"session_id", sessionID, "err", err)
						for _, t := range nextTurns {
							r.store.EnqueueTurn(t)
						}
						r.store.UpdateSessionStatus(nextCh, nextTS, "idle")
						// Remove the failed nextKey from the queue so it doesn't
						// block future event routing.
						r.mu.Lock()
						dropNextKeyFromQueue(r.coalescerQueue, sessionID, nextKey)
						r.mu.Unlock()
					} else {
						slog.Info("router: sent next-thread batch",
							"session_id", sessionID,
							"thread_ts", nextTS,
							"count", len(nextTurns))
					}
				}
			}
		}
		return
	}

	// Concatenate queued messages.
	var texts []string
	for _, t := range turns {
		texts = append(texts, t.Text)
	}
	combined := strings.Join(texts, "\n\n---\n\n")

	// Send combined message to jcode.
	if err := r.jcode.SendMessage(ctx, sessionID, combined, nil); err != nil {
		slog.Warn("router: failed to send queued messages, will retry on next user message",
			"session_id", sessionID, "err", err)
		// Re-enqueue the turns so they aren't lost.
		for _, t := range turns {
			r.store.EnqueueTurn(t)
		}
		r.store.UpdateSessionStatus(channelID, threadTS, "idle")
		return
	}

	// Push the coalescer key back onto the queue so the next "done" event
	// (after processing this batch) routes to the correct coalescer.
	r.mu.Lock()
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], coalKey)
	r.mu.Unlock()

	r.store.UpdateSessionActivity(channelID, threadTS)
	slog.Info("router: sent queued batch", "session_id", sessionID, "count", len(turns))
}

// ---------------------------------------------------------------------------
// Webhook handling
// ---------------------------------------------------------------------------

func (r *Router) handleWebhook(ctx context.Context, evt *WebhookEvent) {
	slog.Info("router: webhook event", "source", evt.Source, "event_type", evt.EventType)

	// GitHub gets its own routing logic based on repo->channel mapping.
	if evt.Source == "github" {
		r.handleGitHubWebhook(ctx, evt)
		return
	}

	r.mu.RLock()
	routes := r.routes
	r.mu.RUnlock()

	// Find matching route.
	var matchedRoute *config.RouteConfig
	for i := range routes {
		if r.matchRoute(&routes[i], evt) {
			matchedRoute = &routes[i]
			break
		}
	}

	if matchedRoute == nil {
		// No matching route: try LLM router, then fall back.
		r.mu.RLock()
		llm := r.llmRouter
		r.mu.RUnlock()
		if llm != nil {
			r.handleWebhookLLMRouting(ctx, evt)
			return
		}

		// No LLM router: post to fallback channel.
		fallbackID := r.cfg.Ingest.FallbackChannelID
		if fallbackID != "" {
			text := fmt.Sprintf("📨 *Unrouted webhook* (%s/%s)\n```\n%s\n```",
				evt.Source, evt.EventType, truncateJSON(evt.Payload, 500))
			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  3,
				ChannelID: fallbackID,
				Action:    outbound.ActionPostMessage,
				Text:      text,
			})
		}
		r.auditWebhookRouting(evt, "fallback", r.cfg.Ingest.FallbackChannelID)
		return
	}

	destChannelID := matchedRoute.Destination.ChannelID
	if destChannelID == "" {
		slog.Warn("router: route has no destination channel_id", "source", evt.Source)
		return
	}

	// Audit the successful route match.
	r.auditWebhookRouting(evt, "rule", destChannelID)

	// Check for existing correlation.
	correlationField := matchedRoute.Correlation.Field
	if correlationField != "" {
		externalKey := extractField(evt.Payload, correlationField)
		if externalKey != "" {
			corrs, err := r.store.LookupCorrelation(evt.Source, externalKey)
			if err == nil && len(corrs) > 0 {
				// Post to existing correlated thread.
				corr := corrs[0]
				text := r.formatWebhookMessage(evt)
				r.outbound.Enqueue(&outbound.OutboundItem{
					Priority:  3,
					ChannelID: corr.ChannelID,
					ThreadTS:  corr.ThreadTS,
					Action:    outbound.ActionPostMessage,
					Text:      text,
				})
				return
			}
		}
	}

	// No correlation: post to channel root (creates new thread).
	text := r.formatWebhookMessage(evt)
	item := &outbound.OutboundItem{
		Priority:  3,
		ChannelID: destChannelID,
		Action:    outbound.ActionPostMessage,
		Text:      text,
		Username:  "Switchboard",
	}

	// If we have a correlation field, capture the new thread_ts via callback.
	if correlationField != "" {
		externalKey := extractField(evt.Payload, correlationField)
		if externalKey != "" {
			src := evt.Source
			item.OnPosted = func(ts string) {
				ttlDays := matchedRoute.Correlation.TTLDays
				if ttlDays == 0 {
					ttlDays = 7 // default 7 days
				}
				now := time.Now().Unix()
				expiresAt := now + int64(ttlDays)*86400
				corr := &store.ThreadCorrelation{
					Source:      src,
					ExternalKey: externalKey,
					ChannelID:   destChannelID,
					ThreadTS:    ts,
					CreatedAt:   now,
					ExpiresAt:   &expiresAt,
					CreatedBy:   "webhook",
				}
				if err := r.store.UpsertCorrelation(corr); err != nil {
					slog.Error("router: failed to create correlation",
						"source", src, "key", externalKey, "error", err)
				} else {
					slog.Info("router: correlation created",
						"source", src, "key", externalKey, "thread_ts", ts)
				}
			}
		}
	}

	r.outbound.Enqueue(item)
}

// handleGitHubWebhook routes a GitHub webhook using the repo->channel mapping
// from config, formats the message with GitHub-aware rendering, and posts to
// the appropriate channel (or fallback).
func (r *Router) handleGitHubWebhook(ctx context.Context, evt *WebhookEvent) {
	repo := ghRepoName(evt.Payload)
	destChannelID := ""

	// Look up repo in the github.repos mapping.
	if repo != "" && r.cfg.GitHub.Repos != nil {
		destChannelID = r.cfg.GitHub.Repos[repo]
	}

	// Fallback to configured fallback channel.
	if destChannelID == "" {
		destChannelID = r.cfg.Ingest.FallbackChannelID
	}

	if destChannelID == "" {
		slog.Warn("router: no channel for GitHub webhook",
			"repo", repo, "event", evt.EventType)
		return
	}

	slog.Info("router: routing GitHub webhook",
		"repo", repo, "event", evt.EventType,
		"channel", destChannelID)

	// Audit the GitHub routing decision.
	r.auditWebhookRouting(evt, "github_config", destChannelID)

	text := formatGitHubWebhook(evt)

	r.outbound.Enqueue(&outbound.OutboundItem{
		Priority:  3,
		ChannelID: destChannelID,
		Action:    outbound.ActionPostMessage,
		Text:      text,
		Username:  "Switchboard",
	})
}

// matchRoute checks if a webhook event matches a route's criteria.
func (r *Router) matchRoute(route *config.RouteConfig, evt *WebhookEvent) bool {
	if route.Source != evt.Source {
		return false
	}

	for key, value := range route.Match {
		actual := extractFieldString(evt.Payload, key)
		if strings.HasSuffix(key, "_prefix") {
			baseKey := strings.TrimSuffix(key, "_prefix")
			actual = extractFieldString(evt.Payload, baseKey)
			if !strings.HasPrefix(actual, value) {
				return false
			}
		} else {
			if actual != value {
				return false
			}
		}
	}

	return true
}

// formatWebhookMessage renders a webhook event for Slack.
func (r *Router) formatWebhookMessage(evt *WebhookEvent) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📨 *%s* — `%s`\n", evt.Source, evt.EventType))

	// Extract useful summary fields depending on source.
	switch evt.Source {
	case "github":
		if repo, ok := evt.Payload["repository"].(map[string]interface{}); ok {
			if fullName, ok := repo["full_name"].(string); ok {
				sb.WriteString(fmt.Sprintf("Repo: `%s`\n", fullName))
			}
		}
		if action, ok := evt.Payload["action"].(string); ok {
			sb.WriteString(fmt.Sprintf("Action: `%s`\n", action))
		}
	case "temporal":
		if wfID, ok := evt.Payload["workflow_id"].(string); ok {
			sb.WriteString(fmt.Sprintf("Workflow: `%s`\n", wfID))
		}
	default:
		// Generic: show first few fields.
		summary := truncateJSON(evt.Payload, 300)
		if summary != "" {
			sb.WriteString("```\n")
			sb.WriteString(summary)
			sb.WriteString("\n```\n")
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Session recovery (bridge restart)
// ---------------------------------------------------------------------------

func (r *Router) recoverSessions(ctx context.Context) error {
	sessions, err := r.store.ListActiveSessions()
	if err != nil {
		return err
	}

	slog.Info("router: recovering sessions", "count", len(sessions))

	// Track which jcode sessions we've already started consumers for.
	startedConsumers := make(map[string]bool)

	for _, sess := range sessions {
		// Try to re-attach to the jcode session.
		events, err := r.jcode.SubscribeExisting(ctx, sess.JcodeSession, sess.Workdir)
		if err != nil {
			slog.Warn("router: failed to recover session",
				"session_id", sess.JcodeSession,
				"err", err,
			)
			// Mark as idle; user will re-trigger on next message.
			r.store.UpdateSessionStatus(sess.ChannelID, sess.ThreadTS, "idle")
			continue
		}

		// Create coalescer.
		_, identity := r.resolveChannel(sess.ChannelID)
		coal := coalesce.NewSessionCoalescer(
			sess.JcodeSession, sess.FriendlyName,
			sess.ChannelID, sess.ThreadTS, sess.Workdir,
			identity, r.outbound, r.handleImage,
		)
		coal.SetStrictDirectives(r.cfg.Render.StrictDirectiveValidation)

		key := coalescerKey(sess.ChannelID, sess.ThreadTS)
		r.mu.Lock()
		r.coalescers[key] = coal
		// Only add to the event queue if the session was actively processing.
		// Idle sessions will be added when a new message arrives.
		if sess.Status == "processing" {
			r.coalescerQueue[sess.JcodeSession] = append(r.coalescerQueue[sess.JcodeSession], key)
		}
		r.mu.Unlock()

		// Start consuming events (only one consumer per jcode session).
		if !startedConsumers[sess.JcodeSession] {
			startedConsumers[sess.JcodeSession] = true
			go r.consumeEvents(ctx, sess.JcodeSession, events)
		}

		// Mark session as idle (will be activated on next user message).
		r.store.UpdateSessionStatus(sess.ChannelID, sess.ThreadTS, "idle")

		slog.Info("router: recovered session",
			"session_id", sess.JcodeSession,
			"channel", sess.ChannelID,
		)
	}

	return nil
}

// ---------------------------------------------------------------------------
// LLM-based webhook routing
// ---------------------------------------------------------------------------

// handleWebhookLLMRouting routes an unmatched webhook event via the LLM router.
func (r *Router) handleWebhookLLMRouting(ctx context.Context, evt *WebhookEvent) {
	r.mu.RLock()
	llm := r.llmRouter
	fallbackID := r.cfg.Ingest.FallbackChannelID
	r.mu.RUnlock()

	if llm == nil {
		return
	}

	// Build thread context for the LLM.
	threads := r.buildThreadContext()

	// Build webhook summary.
	summary := llmrouter.WebhookSummary{
		Source:    evt.Source,
		EventType: evt.EventType,
		Summary:   truncateJSON(evt.Payload, 500),
	}

	// Call the LLM router.
	decision, err := llm.Route(ctx, summary, threads)
	if err != nil || decision == nil {
		// LLM failed or unavailable: fall back.
		if fallbackID != "" {
			text := fmt.Sprintf("📨 *Unrouted webhook* (%s/%s)\n```\n%s\n```",
				evt.Source, evt.EventType, truncateJSON(evt.Payload, 500))
			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  3,
				ChannelID: fallbackID,
				Action:    outbound.ActionPostMessage,
				Text:      text,
			})
		}
		r.auditWebhookRouting(evt, "fallback", fallbackID)
		return
	}

	// Record the decision.
	decisionRecord := &store.LLMRoutingDecision{
		WebhookInboxID: nil, // set when durable inbox is used
		DecidedAt:      time.Now().Unix(),
		Model:          r.cfg.Routing.LLM.Model,
		ThreadID:       decision.ThreadID,
		Confidence:     decision.Confidence,
		Reasoning:      &decision.Reasoning,
	}

	if llm.MeetsThreshold(decision) && decision.ThreadID != nil {
		// Route to suggested thread.
		parts := strings.SplitN(*decision.ThreadID, ":", 2)
		if len(parts) == 2 {
			channelID, threadTS := parts[0], parts[1]

			// Validate that the thread_id matches one of the threads we
			// actually provided to the LLM. This prevents hallucinated
			// thread IDs from being blindly accepted.
			if !isValidLLMThreadID(*decision.ThreadID, threads) {
				slog.Warn("router: LLM suggested thread_id not in provided list, falling back",
					"thread_id", *decision.ThreadID)
				decisionRecord.PostedTo = "fallback"
				r.postToFallback(evt, fallbackID, decision)
				r.store.InsertLLMDecision(decisionRecord)
				return
			}

			// Validate that the channel is one we actually manage.
			if workdir, _ := r.resolveChannel(channelID); workdir == "" {
				slog.Warn("router: LLM suggested unknown channel, falling back",
					"thread_id", *decision.ThreadID, "channel", channelID)
				decisionRecord.PostedTo = "fallback"
				r.postToFallback(evt, fallbackID, decision)
				r.store.InsertLLMDecision(decisionRecord)
				return
			}

			text := r.formatWebhookMessage(evt)
			text += fmt.Sprintf("\n_routed by AI (confidence %d%%, %s)_", decision.Confidence, decision.Reasoning)

			r.outbound.Enqueue(&outbound.OutboundItem{
				Priority:  3,
				ChannelID: channelID,
				ThreadTS:  threadTS,
				Action:    outbound.ActionPostMessage,
				Text:      text,
			})

			decisionRecord.PostedTo = "suggested"
			r.auditWebhookRouting(evt, "llm", channelID)

			slog.Info("router: LLM routed webhook to thread",
				"source", evt.Source,
				"event_type", evt.EventType,
				"channel", channelID,
				"thread_ts", threadTS,
				"confidence", decision.Confidence,
			)
		} else {
			// Malformed thread_id from LLM: fall back.
			decisionRecord.PostedTo = "fallback"
			r.postToFallback(evt, fallbackID, decision)
		}
	} else {
		// Below threshold: post to fallback with reasoning.
		decisionRecord.PostedTo = "fallback"
		r.postToFallback(evt, fallbackID, decision)
	}

	// Persist the decision.
	if err := r.store.InsertLLMDecision(decisionRecord); err != nil {
		slog.Error("router: failed to persist LLM decision", "error", err)
	}
}

// postToFallback posts an unrouted webhook to the fallback channel with LLM reasoning.
func (r *Router) postToFallback(evt *WebhookEvent, fallbackID string, decision *llmrouter.Decision) {
	if fallbackID == "" {
		return
	}

	text := fmt.Sprintf("📨 *Unrouted webhook* (%s/%s)\n```\n%s\n```",
		evt.Source, evt.EventType, truncateJSON(evt.Payload, 500))

	if decision != nil && decision.Reasoning != "" {
		text += fmt.Sprintf("\n_AI reasoning (confidence %d%%): %s_", decision.Confidence, decision.Reasoning)
	}

	r.outbound.Enqueue(&outbound.OutboundItem{
		Priority:  3,
		ChannelID: fallbackID,
		Action:    outbound.ActionPostMessage,
		Text:      text,
	})

	r.auditWebhookRouting(evt, "fallback_llm", fallbackID)
}

// buildThreadContext gathers recent active threads for the LLM prompt.
// It filters out DM sessions, sorts by last activity (most recent first),
// and limits to include_thread_count (default 30).
func (r *Router) buildThreadContext() []llmrouter.ThreadContext {
	sessions, err := r.store.ListActiveSessions()
	if err != nil {
		slog.Error("router: failed to list active sessions for LLM context", "error", err)
		return nil
	}

	// Sort by LastActivity descending (most recent first).
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastActivity > sessions[j].LastActivity
	})

	var threads []llmrouter.ThreadContext
	for _, sess := range sessions {
		// Filter out DM sessions (channel ID starts with "D").
		if strings.HasPrefix(sess.ChannelID, "D") {
			continue
		}

		channelName := r.edge.ChannelName(sess.ChannelID)
		if channelName == "" {
			channelName = sess.ChannelID
		}

		lastActive := time.Since(time.Unix(sess.LastActivity, 0)).Truncate(time.Minute).String() + " ago"

		threads = append(threads, llmrouter.ThreadContext{
			ChannelID:   sess.ChannelID,
			ChannelName: "#" + channelName,
			ThreadTS:    sess.ThreadTS,
			Topic:       sess.FriendlyName,
			Workdir:     sess.Workdir,
			LastActive:  lastActive,
		})
	}

	// Cap to include_thread_count (default 30).
	limit := r.cfg.Routing.LLM.IncludeThreadCount
	if limit == 0 {
		limit = 30
	}
	if limit < len(threads) {
		threads = threads[:limit]
	}

	return threads
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveChannel looks up the workdir and identity for a channel_id.
func (r *Router) resolveChannel(channelID string) (string, coalesce.Identity) {
	for _, ch := range r.cfg.Channels {
		if ch.ID == channelID {
			return ch.Workdir, coalesce.Identity{
				DisplayName: ch.Identity,
				IconURL:     ch.IconURL,
			}
		}
	}

	// DMs: use default workdir if configured.
	if strings.HasPrefix(channelID, "D") && r.cfg.Bridge.DefaultWorkdir != "" {
		return r.cfg.Bridge.DefaultWorkdir, coalesce.Identity{DisplayName: "Assistant"}
	}

	// Workspace fallback.
	if r.cfg.Bridge.Routing.WorkspaceFallback {
		name := r.edge.ChannelName(channelID)
		if name != "" {
			home, _ := filepath.Abs(filepath.Join("~/workspace", name))
			return home, coalesce.Identity{DisplayName: name + " Worker"}
		}
	}

	return "", coalesce.Identity{}
}

func (r *Router) postError(channelID, threadTS, msg string) {
	r.outbound.Enqueue(&outbound.OutboundItem{
		Priority:  1,
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Action:    outbound.ActionPostMessage,
		Text:      "❌ " + msg,
	})
}

func (r *Router) handleImage(req coalesce.ImageUploadRequest) {
	// TODO: read image from path, validate, upload via outbound queue.
	slog.Info("router: image upload requested", "path", req.Path, "channel", req.ChannelID)
}

// resolveIdentity returns just the identity for a channel (used when workdir is already known).
func (r *Router) resolveIdentity(channelID string) coalesce.Identity {
	_, identity := r.resolveChannel(channelID)
	return identity
}

func (r *Router) closeAllCoalescers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, coal := range r.coalescers {
		coal.Close()
		delete(r.coalescers, key)
	}
}

// NotifyShutdown posts a message to all threads with active turns informing
// the requesting user that the service is restarting.
func (r *Router) NotifyShutdown() {
	r.mu.RLock()
	requesters := make(map[string]string, len(r.turnRequester))
	for k, v := range r.turnRequester {
		requesters[k] = v
	}
	r.mu.RUnlock()

	for coalKey, userID := range requesters {
		channelID, threadTS := parseCoalescerKey(coalKey)
		if channelID == "" {
			continue
		}
		r.outbound.Enqueue(&outbound.OutboundItem{
			Priority:  1,
			ChannelID: channelID,
			ThreadTS:  threadTS,
			Action:    outbound.ActionPostMessage,
			Text:      fmt.Sprintf("<@%s> 🔄 Service restarting - send your message again in a moment to resume.", userID),
		})
	}

	// Give the outbound queue a moment to flush.
	time.Sleep(500 * time.Millisecond)
}

func (r *Router) runMaintenance() {
	cfg := store.MaintenanceConfig{
		AuditRetentionDays:         r.cfg.Bridge.Audit.RetentionDays,
		DoneWebhookRetentionDays:   7,
		FailedWebhookRetentionDays: 30,
		MaxCorrelationRows:         10000,
	}
	if cfg.AuditRetentionDays == 0 {
		cfg.AuditRetentionDays = 30
	}
	if err := r.store.RunMaintenance(cfg); err != nil {
		slog.Error("router: maintenance failed", "err", err)
	}
}

func coalescerKey(channelID, threadTS string) string {
	return channelID + ":" + threadTS
}

func parseCoalescerKey(key string) (channelID, threadTS string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
// extractField extracts a dotted path from a nested map.
func extractField(payload map[string]interface{}, path string) string {
	parts := strings.Split(path, ".")
	current := payload

	for i, part := range parts {
		val, ok := current[part]
		if !ok {
			return ""
		}
		if i == len(parts)-1 {
			if s, ok := val.(string); ok {
				return s
			}
			if n, ok := val.(float64); ok {
				return fmt.Sprintf("%.0f", n)
			}
			return fmt.Sprintf("%v", val)
		}
		next, ok := val.(map[string]interface{})
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

func extractFieldString(payload map[string]interface{}, key string) string {
	return extractField(payload, key)
}

func truncateJSON(payload map[string]interface{}, maxLen int) string {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// extractFriendlyName extracts the animal name from a jcode session ID.
// Session IDs follow the pattern: session_<animal>_<timestamp>_<hash>
func extractFriendlyName(sessionID string) string {
	parts := strings.SplitN(sessionID, "_", 4)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// ---------------------------------------------------------------------------
// Audit helpers
// ---------------------------------------------------------------------------

// sha256Hex returns the hex-encoded SHA-256 of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// auditSlackMessage writes an audit entry for an inbound Slack message.
func (r *Router) auditSlackMessage(msg *slack.InboundMessage, threadTS string) {
	eventType := "message"
	if msg.IsAppMention {
		eventType = "app_mention"
	}

	summary, _ := json.Marshal(map[string]string{
		"channel_id": msg.ChannelID,
		"user_id":    msg.UserID,
		"thread_ts":  threadTS,
	})

	channelID := msg.ChannelID
	entry := &store.AuditEntry{
		TS:                 time.Now().Unix(),
		Source:             "slack",
		EventType:          eventType,
		ChannelID:          &channelID,
		ThreadTS:           &threadTS,
		PayloadSummaryJSON: string(summary),
		PayloadHash:        sha256Hex([]byte(msg.Text)),
	}

	if err := r.store.InsertAudit(entry); err != nil {
		slog.Error("router: audit insert failed", "err", err)
	}
}

// auditWebhookRouting writes an audit entry for a webhook routing decision.
func (r *Router) auditWebhookRouting(evt *WebhookEvent, routedBy string, channelID string) {
	summary, _ := json.Marshal(map[string]string{
		"source":     evt.Source,
		"event_type": evt.EventType,
		"channel_id": channelID,
		"routed_by":  routedBy,
	})

	var chPtr *string
	if channelID != "" {
		chPtr = &channelID
	}

	entry := &store.AuditEntry{
		TS:                 time.Now().Unix(),
		Source:             evt.Source,
		EventType:          evt.EventType,
		ChannelID:          chPtr,
		RoutedBy:           &routedBy,
		PayloadSummaryJSON: string(summary),
		PayloadHash:        sha256Hex(evt.RawBody),
	}

	if err := r.store.InsertAudit(entry); err != nil {
		slog.Error("router: audit insert failed", "err", err)
	}
}
