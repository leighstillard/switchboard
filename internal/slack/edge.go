// Package slack implements the Slack edge: receiving events from Slack
// (via Socket Mode) and sending outbound messages.
package slack

import (
	"context"
	"log/slog"
	"sync"

	"github.com/format5/switchboard/internal/config"
)

// Edge manages bidirectional communication with Slack.
type Edge struct {
	cfg        config.SlackConfig
	channels   []config.ChannelConfig
	identities map[string]config.IdentityConfig
	mu         sync.RWMutex
}

// NewEdge creates a new Slack edge with the given configuration.
func NewEdge(cfg config.SlackConfig, channels []config.ChannelConfig, identities map[string]config.IdentityConfig) (*Edge, error) {
	return &Edge{
		cfg:        cfg,
		channels:   channels,
		identities: identities,
	}, nil
}

// Run starts the Slack event loop (Socket Mode). Blocks until ctx is cancelled.
func (e *Edge) Run(ctx context.Context) {
	slog.Info("slack edge started")
	<-ctx.Done()
	slog.Info("slack edge stopped")
}

// SendMessage sends a message to a Slack channel with an optional identity override.
func (e *Edge) SendMessage(ctx context.Context, channelID, text, identity string) error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// TODO: implement Slack API calls
	slog.Debug("slack send", "channel", channelID, "identity", identity, "len", len(text))
	return nil
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
