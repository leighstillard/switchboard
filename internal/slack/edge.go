// Package slack implements the Slack edge: receiving events from Slack
// (via Socket Mode) and sending outbound messages.
package slack

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/outbound"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ---------------------------------------------------------------------------
// Inbound types
// ---------------------------------------------------------------------------

// InboundMessage is the normalized representation of a Slack message event
// after eligibility filtering and @mention stripping.
type InboundMessage struct {
	ChannelID    string
	ThreadTS     string // empty if top-level
	MessageTS    string // the message's own timestamp
	UserID       string
	BotID        string // empty if from human
	Text         string // with @mention stripped
	Files        []SlackFile
	IsTopLevel   bool // thread_ts == ts or empty
	IsDM         bool
	IsAppMention bool // true if originated from app_mention event
	MentionsBot  bool // true if the original text contained our @mention
	MentionsOther bool // true if the text contains @mentions of other users
}

// SlackFile describes a file attachment on an inbound message.
type SlackFile struct {
	ID       string
	Name     string
	MimeType string
	Size     int
	URL      string // url_private
}

// InboundHandler is the callback type invoked for each eligible inbound message.
type InboundHandler func(msg *InboundMessage)

// ---------------------------------------------------------------------------
// Edge
// ---------------------------------------------------------------------------

// Edge manages bidirectional communication with Slack via Socket Mode.
type Edge struct {
	cfg        config.SlackConfig
	channels   []config.ChannelConfig
	identities map[string]config.IdentityConfig
	allowlist  map[string]bool // bot IDs allowed through the bot_message filter

	api    *slackapi.Client
	socket *socketmode.Client

	botUserID string // our own bot user ID, populated on connect

	mu          sync.RWMutex
	channelName map[string]string // channelID -> name cache
	channelSet  map[string]bool   // set of configured channel IDs

	onInbound InboundHandler
}

// NewEdge creates a new Slack edge with the given configuration.
// Call SetInboundHandler before Run to receive inbound messages.
func NewEdge(
	cfg config.SlackConfig,
	channels []config.ChannelConfig,
	identities map[string]config.IdentityConfig,
) (*Edge, error) {
	api := slackapi.New(
		cfg.BotToken,
		slackapi.OptionAppLevelToken(cfg.AppToken),
	)

	socket := socketmode.New(
		api,
		socketmode.OptionDebug(false),
	)

	chanSet := make(map[string]bool, len(channels))
	chanNames := make(map[string]string, len(channels))
	for _, ch := range channels {
		chanSet[ch.ID] = true
		if ch.Name != "" {
			chanNames[ch.ID] = ch.Name
		}
	}

	return &Edge{
		cfg:         cfg,
		channels:    channels,
		identities:  identities,
		allowlist:   make(map[string]bool),
		api:         api,
		socket:      socket,
		channelName: chanNames,
		channelSet:  chanSet,
	}, nil
}

// SetInboundHandler registers the callback for dispatching inbound messages.
func (e *Edge) SetInboundHandler(h InboundHandler) {
	e.onInbound = h
}

// SetBotAllowlist configures which external bot IDs are allowed through
// the bot_message subtype filter.
func (e *Edge) SetBotAllowlist(ids []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.allowlist = make(map[string]bool, len(ids))
	for _, id := range ids {
		e.allowlist[id] = true
	}
}

// ReloadConfig updates channels and identities on SIGHUP.
func (e *Edge) ReloadConfig(channels []config.ChannelConfig, identities map[string]config.IdentityConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.channels = channels
	e.identities = identities
	e.channelSet = make(map[string]bool, len(channels))
	for _, ch := range channels {
		e.channelSet[ch.ID] = true
	}
	slog.Info("slack edge: config reloaded", "channels", len(channels), "identities", len(identities))
}

// Run starts the Slack Socket Mode event loop. Blocks until ctx is cancelled.
func (e *Edge) Run(ctx context.Context) {
	// Resolve our own bot user ID for self-message filtering.
	if resp, err := e.api.AuthTest(); err == nil {
		e.botUserID = resp.UserID
		slog.Info("slack edge: authenticated", "bot_user_id", e.botUserID)
	} else {
		slog.Error("slack edge: auth test failed", "error", err)
	}

	go e.handleEvents(ctx)

	slog.Info("slack edge started (socket mode)")
	if err := e.socket.RunContext(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Error("slack edge: socket mode error", "error", err)
		}
	}
	slog.Info("slack edge stopped")
}

// handleEvents processes events from the Socket Mode client.
func (e *Edge) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-e.socket.Events:
			if !ok {
				return
			}
			e.routeEvent(evt)
		}
	}
}

// routeEvent dispatches a Socket Mode event to the appropriate handler.
func (e *Edge) routeEvent(evt socketmode.Event) {
	slog.Debug("slack edge: received event", "type", string(evt.Type))

	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		e.socket.Ack(*evt.Request)
		evtAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			slog.Debug("slack edge: events API data type mismatch", "data_type", fmt.Sprintf("%T", evt.Data))
			return
		}
		e.handleEventsAPI(evtAPI)
	case socketmode.EventTypeConnecting:
		slog.Debug("slack edge: connecting")
	case socketmode.EventTypeConnected:
		slog.Info("slack edge: connected")
	case socketmode.EventTypeConnectionError:
		slog.Warn("slack edge: connection error")
	default:
		// Ignore interactive messages, slash commands, etc. for now.
	}
}

// handleEventsAPI processes Events API callbacks.
func (e *Edge) handleEventsAPI(evt slackevents.EventsAPIEvent) {
	switch evt.Type {
	case slackevents.CallbackEvent:
		e.handleInnerEvent(evt.InnerEvent)
	default:
		slog.Debug("slack edge: unhandled events api type", "type", evt.Type)
	}
}

// handleInnerEvent dispatches the inner event by type.
func (e *Edge) handleInnerEvent(inner slackevents.EventsAPIInnerEvent) {
	slog.Debug("slack edge: inner event", "type", inner.Type)

	switch ev := inner.Data.(type) {
	case *slackevents.MessageEvent:
		e.handleMessage(ev)
	case *slackevents.AppMentionEvent:
		e.handleAppMention(ev)
	case *slackevents.ChannelRenameEvent:
		e.handleChannelRename(ev)
	default:
		slog.Debug("slack edge: unhandled inner event", "type", inner.Type)
	}
}

// ---------------------------------------------------------------------------
// Message eligibility filter (spec S9)
// ---------------------------------------------------------------------------

func (e *Edge) handleMessage(ev *slackevents.MessageEvent) {
	slog.Debug("slack edge: message event",
		"channel", ev.Channel, "user", ev.User, "subtype", ev.SubType,
		"bot_id", ev.BotID, "text_len", len(ev.Text))

	// Drop subtypes we never process.
	switch ev.SubType {
	case "message_changed", "message_deleted":
		return
	}

	// Drop bot_message from non-allowlisted bots.
	if ev.SubType == "bot_message" {
		e.mu.RLock()
		allowed := e.allowlist[ev.BotID]
		e.mu.RUnlock()
		if !allowed {
			return
		}
	}

	// Drop self-messages.
	if ev.User == e.botUserID {
		return
	}

	channelID := ev.Channel
	threadTS := ev.ThreadTimeStamp
	messageTS := ev.TimeStamp
	isDM := (ev.ChannelType == slackevents.ChannelTypeIM)
	isTopLevel := (threadTS == "" || threadTS == messageTS)

	// Check channel mapping.
	if !isDM && !e.hasChannel(channelID) {
		return
	}

	// Top-level in channel: let app_mention handler deal with these.
	// This avoids double-dispatching when both message and app_mention events fire.
	if !isDM && isTopLevel {
		return
	}

	// Reply in owned thread: always process (no mention needed).
	// DM to bot: always process.

	mentionsBot := strings.Contains(ev.Text, "<@"+e.botUserID+">")
	mentionsOther := hasMentionOtherThan(ev.Text, e.botUserID)
	text := e.stripBotMention(ev.Text)

	// Extract files from the Message field (populated by custom UnmarshalJSON).
	var files []SlackFile
	if ev.Message != nil {
		for _, f := range ev.Message.Files {
			files = append(files, SlackFile{
				ID:       f.ID,
				Name:     f.Name,
				MimeType: f.Mimetype,
				Size:     f.Size,
				URL:      f.URLPrivate,
			})
		}
	}

	msg := &InboundMessage{
		ChannelID:     channelID,
		ThreadTS:      threadTS,
		MessageTS:     messageTS,
		UserID:        ev.User,
		BotID:         ev.BotID,
		Text:          text,
		Files:         files,
		IsTopLevel:    isTopLevel,
		IsDM:          isDM,
		MentionsBot:   mentionsBot,
		MentionsOther: mentionsOther,
	}

	e.dispatch(msg)
}

func (e *Edge) handleAppMention(ev *slackevents.AppMentionEvent) {
	slog.Debug("slack edge: app_mention received",
		"channel", ev.Channel, "user", ev.User, "text", ev.Text)

	threadTS := ev.ThreadTimeStamp
	messageTS := ev.TimeStamp
	isTopLevel := (threadTS == "" || threadTS == messageTS)

	text := e.stripBotMention(ev.Text)

	var files []SlackFile
	for _, f := range ev.Files {
		files = append(files, SlackFile{
			ID:       f.ID,
			Name:     f.Name,
			MimeType: f.Mimetype,
			Size:     f.Size,
			URL:      f.URLPrivate,
		})
	}

	msg := &InboundMessage{
		ChannelID:     ev.Channel,
		ThreadTS:      threadTS,
		MessageTS:     messageTS,
		UserID:        ev.User,
		BotID:         ev.BotID,
		Text:          text,
		Files:         files,
		IsTopLevel:    isTopLevel,
		IsDM:          false,
		IsAppMention:  true,
		MentionsBot:   true, // by definition: it's an app_mention event
		MentionsOther: hasMentionOtherThan(ev.Text, e.botUserID),
	}

	e.dispatch(msg)
}

// ---------------------------------------------------------------------------
// Channel rename
// ---------------------------------------------------------------------------

func (e *Edge) handleChannelRename(ev *slackevents.ChannelRenameEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.channelName[ev.Channel.ID] = ev.Channel.Name
	slog.Info("slack edge: channel renamed", "id", ev.Channel.ID, "name", ev.Channel.Name)
}

// ---------------------------------------------------------------------------
// Outbound API (satisfies outbound.SlackPoster)
// ---------------------------------------------------------------------------

// PostMessage sends a new message to a Slack channel.
func (e *Edge) PostMessage(channelID, text string, opts ...outbound.PostOption) (string, error) {
	msgOpts := []slackapi.MsgOption{
		slackapi.MsgOptionText(text, false),
	}

	for _, o := range opts {
		if o.ThreadTS != "" {
			msgOpts = append(msgOpts, slackapi.MsgOptionTS(o.ThreadTS))
		}
		if o.Username != "" {
			msgOpts = append(msgOpts, slackapi.MsgOptionUsername(o.Username))
		}
		if o.IconURL != "" {
			msgOpts = append(msgOpts, slackapi.MsgOptionIconURL(o.IconURL))
		}
		if len(o.Blocks) > 0 {
			blocks := buildBlocks(o.Blocks)
			msgOpts = append(msgOpts, slackapi.MsgOptionBlocks(blocks...))
		}
	}

	_, ts, err := e.api.PostMessage(channelID, msgOpts...)
	if err != nil {
		return "", fmt.Errorf("slack: post message: %w", err)
	}
	return ts, nil
}

// UpdateMessage edits an existing Slack message.
func (e *Edge) UpdateMessage(channelID, ts, text string, opts ...outbound.PostOption) error {
	msgOpts := []slackapi.MsgOption{
		slackapi.MsgOptionText(text, false),
	}

	for _, o := range opts {
		if len(o.Blocks) > 0 {
			blocks := buildBlocks(o.Blocks)
			msgOpts = append(msgOpts, slackapi.MsgOptionBlocks(blocks...))
		}
	}

	_, _, _, err := e.api.UpdateMessage(channelID, ts, msgOpts...)
	if err != nil {
		return fmt.Errorf("slack: update message: %w", err)
	}
	return nil
}

// UploadFile uploads a file to a Slack channel/thread.
func (e *Edge) UploadFile(channelID, threadTS, filename string, content []byte) error {
	params := slackapi.UploadFileParameters{
		Filename:       filename,
		Reader:         bytes.NewReader(content),
		FileSize:       len(content),
		Channel:        channelID,
		ThreadTimestamp: threadTS,
	}

	_, err := e.api.UploadFile(params)
	if err != nil {
		return fmt.Errorf("slack: upload file: %w", err)
	}
	return nil
}

// AddReaction adds an emoji reaction to a message.
func (e *Edge) AddReaction(channelID, ts, emoji string) error {
	ref := slackapi.NewRefToMessage(channelID, ts)
	if err := e.api.AddReaction(emoji, ref); err != nil {
		return fmt.Errorf("slack: add reaction: %w", err)
	}
	return nil
}

// RemoveReaction removes an emoji reaction from a message.
func (e *Edge) RemoveReaction(channelID, ts, emoji string) error {
	ref := slackapi.NewRefToMessage(channelID, ts)
	if err := e.api.RemoveReaction(emoji, ref); err != nil {
		return fmt.Errorf("slack: remove reaction: %w", err)
	}
	return nil
}

// DMUser opens a DM conversation with the given user ID and sends a message.
// Returns the DM channel ID and message timestamp on success.
func (e *Edge) DMUser(userID, text string) (string, string, error) {
	// Open (or retrieve) a DM channel with this user.
	ch, _, _, err := e.api.OpenConversation(&slackapi.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return "", "", fmt.Errorf("slack: open DM conversation: %w", err)
	}

	channelID := ch.ID
	_, ts, err := e.api.PostMessage(channelID, slackapi.MsgOptionText(text, false))
	if err != nil {
		return channelID, "", fmt.Errorf("slack: post DM: %w", err)
	}
	return channelID, ts, nil
}

// GetChannelInfo retrieves channel name from the Slack API.
func (e *Edge) GetChannelInfo(channelID string) (string, error) {
	ch, err := e.api.GetConversationInfo(&slackapi.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return "", fmt.Errorf("slack: get channel info: %w", err)
	}
	return ch.Name, nil
}

// IsUserAdmin checks if a user is a Slack workspace admin or owner.
func (e *Edge) IsUserAdmin(userID string) (bool, error) {
	user, err := e.api.GetUserInfo(userID)
	if err != nil {
		return false, fmt.Errorf("slack: get user info: %w", err)
	}
	return user.IsAdmin || user.IsOwner, nil
}

// ChannelForWorkdir returns the channel config for a given working directory.
func (e *Edge) ChannelForWorkdir(workdir string) *config.ChannelConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for i := range e.channels {
		if e.channels[i].Workdir == workdir {
			return &e.channels[i]
		}
	}
	return nil
}

// ChannelName returns the cached name for a channel ID.
func (e *Edge) ChannelName(channelID string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.channelName[channelID]
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mentionRe matches <@UXXXXXXXX> patterns (unused but kept for reference).
var _ = regexp.MustCompile(`<@[A-Z0-9]+>`)

func (e *Edge) textMentionsBot(text string) bool {
	if e.botUserID == "" {
		return false
	}
	return strings.Contains(text, "<@"+e.botUserID+">")
}

func (e *Edge) stripBotMention(text string) string {
	if e.botUserID == "" {
		return strings.TrimSpace(text)
	}
	mention := "<@" + e.botUserID + ">"
	text = strings.ReplaceAll(text, mention, "")
	// Strip Slack MCP "Sent using" suffix (e.g. "*Sent using* <@UXXXXXXX>").
	if idx := strings.Index(text, "*Sent using*"); idx >= 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

// userMentionRe matches Slack user mentions like <@U12345ABC>.
var userMentionRe = regexp.MustCompile(`<@(U[A-Z0-9]+)>`)

// hasMentionOtherThan returns true if the text contains an @mention of any
// user other than the specified one (typically the bot's own user ID).
func hasMentionOtherThan(text, excludeUserID string) bool {
	matches := userMentionRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if m[1] != excludeUserID {
			return true
		}
	}
	return false
}

func (e *Edge) hasChannel(channelID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.channelSet[channelID]
}

func (e *Edge) dispatch(msg *InboundMessage) {
	if e.onInbound != nil {
		e.onInbound(msg)
	} else {
		slog.Warn("slack edge: inbound message dropped (no handler)", "channel", msg.ChannelID)
	}
}

// buildBlocks converts raw block maps to slack.Block values.
func buildBlocks(raw []map[string]interface{}) []slackapi.Block {
	blocks := make([]slackapi.Block, 0, len(raw))
	for _, b := range raw {
		blockType, _ := b["type"].(string)
		switch blockType {
		case "header":
			textObj, _ := b["text"].(map[string]interface{})
			txt, _ := textObj["text"].(string)
			blocks = append(blocks, slackapi.NewHeaderBlock(
				slackapi.NewTextBlockObject(slackapi.PlainTextType, txt, true, false),
			))
		case "section":
			textObj, _ := b["text"].(map[string]interface{})
			txt, _ := textObj["text"].(string)
			textType := slackapi.MarkdownType
			if tp, ok := textObj["type"].(string); ok && tp == "plain_text" {
				textType = slackapi.PlainTextType
			}
			blocks = append(blocks, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(textType, txt, false, false),
				nil, nil,
			))
		case "context":
			var elements []slackapi.MixedElement
			switch elems := b["elements"].(type) {
			case []map[string]interface{}:
				for _, elem := range elems {
					txt, _ := elem["text"].(string)
					elemType, _ := elem["type"].(string)
					if elemType == "mrkdwn" {
						elements = append(elements, slackapi.NewTextBlockObject(slackapi.MarkdownType, txt, false, false))
					} else {
						elements = append(elements, slackapi.NewTextBlockObject(slackapi.PlainTextType, txt, false, false))
					}
				}
			case []interface{}:
				for _, e := range elems {
					if elem, ok := e.(map[string]interface{}); ok {
						txt, _ := elem["text"].(string)
						elemType, _ := elem["type"].(string)
						if elemType == "mrkdwn" {
							elements = append(elements, slackapi.NewTextBlockObject(slackapi.MarkdownType, txt, false, false))
						} else {
							elements = append(elements, slackapi.NewTextBlockObject(slackapi.PlainTextType, txt, false, false))
						}
					}
				}
			}
			blocks = append(blocks, slackapi.NewContextBlock("", elements...))
		case "divider":
			blocks = append(blocks, slackapi.NewDividerBlock())
		default:
			slog.Warn("buildBlocks: unsupported block type", "type", blockType)
			blocks = append(blocks, slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType,
					fmt.Sprintf("(unsupported block type: %s)", blockType), false, false),
				nil, nil,
			))
		}
	}
	return blocks
}

// SendMessage is a legacy convenience method for backward compatibility
// with the router. It posts a message with an optional identity override.
func (e *Edge) SendMessage(ctx context.Context, channelID, text, identity string) error {
	var opts []outbound.PostOption
	if identity != "" {
		e.mu.RLock()
		ident, ok := e.identities[identity]
		e.mu.RUnlock()
		if ok {
			opts = append(opts, outbound.PostOption{
				Username: ident.DisplayName,
				IconURL:  ident.IconURL,
			})
		}
	}
	_, err := e.PostMessage(channelID, text, opts...)
	return err
}
