// Package jcode provides a client for connecting to jcode agent sessions
// via the Unix domain socket protocol.
package jcode

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/format5/switchboard/internal/config"
)

// Client manages the connection to the jcode daemon socket.
type Client struct {
	cfg    config.JcodeConfig
	mu     sync.Mutex
	closed bool
}

// NewClient creates a jcode client. If auto_spawn is enabled and no socket
// is found, it will spawn the jcode daemon.
func NewClient(cfg config.JcodeConfig) (*Client, error) {
	c := &Client{cfg: cfg}

	if cfg.AutoSpawn && cfg.SocketPath == "" {
		slog.Info("jcode auto-spawn enabled, will connect on demand")
	}

	return c, nil
}

// SendMessage sends a message to a jcode session.
func (c *Client) SendMessage(ctx context.Context, sessionID string, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("jcode: client closed")
	}

	// TODO: implement actual socket protocol communication
	slog.Debug("jcode send", "session", sessionID, "len", len(message))
	return nil
}

// SpawnSession starts a new jcode session for the given working directory.
func (c *Client) SpawnSession(ctx context.Context, workdir string) (string, error) {
	if c.cfg.AutoSpawn && c.cfg.SpawnCommand != "" {
		parts := strings.Fields(c.cfg.SpawnCommand)
		cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
		cmd.Dir = workdir
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("jcode: spawn: %w", err)
		}
	}

	// TODO: implement session creation via socket protocol
	return "", nil
}

// Close shuts down the jcode client connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
