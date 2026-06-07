package claude

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config holds Claude backend configuration.
type Config struct {
	Binary              string
	Model               string
	SettingSources      string            // settings layers to load; default "project,local" (excludes user)
	PermissionPolicy    string            // "allow_all" | "deny_all" | "accept_edits_only"
	AppendSystemPrompt  string            // appended to claude's built-in system prompt
	GracefulStopTimeout time.Duration     // stdin-close → SIGTERM grace; default 30s
	IdleEvictionTimeout time.Duration     // tear down a dormant subprocess after this idle; 0 = never
	ExtraEnv            map[string]string // applied last, overrides inherited env
	ExtraArgs           []string          // appended after our flags
}

// DefaultConfig returns the default Claude backend configuration.
func DefaultConfig() Config {
	return Config{
		Binary:              "claude",
		Model:               "claude-sonnet-4-20250514",
		SettingSources:      "project,local",
		PermissionPolicy:    "allow_all",
		GracefulStopTimeout: 30 * time.Second,
		IdleEvictionTimeout: 30 * time.Minute,
	}
}

func (c *Config) applyDefaults() {
	if c.Binary == "" {
		c.Binary = "claude"
	}
	if c.Model == "" {
		c.Model = "claude-sonnet-4-20250514"
	}
	if c.SettingSources == "" {
		c.SettingSources = "project,local"
	}
	if c.PermissionPolicy == "" {
		c.PermissionPolicy = "allow_all"
	}
	if c.GracefulStopTimeout == 0 {
		c.GracefulStopTimeout = 30 * time.Second
	}
	// IdleEvictionTimeout 0 means "never", so leave it.
}

// initTimeout bounds how long Subscribe waits for the first system/init.
const initTimeout = 60 * time.Second

// ---------------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------------

// Backend implements agent.Backend for the Claude Code CLI, one long-running
// process per session.
type Backend struct {
	cfg     Config
	cmd     commander
	policy  PermissionPolicy
	clk     clock
	baseEnv []string

	mu       sync.Mutex
	sessions map[string]*session
}

// New creates a new Claude Code backend with the given configuration.
func New(cfg Config) *Backend {
	cfg.applyDefaults()
	return &Backend{
		cfg:      cfg,
		cmd:      &realCommander{binary: cfg.Binary},
		policy:   policyForName(cfg.PermissionPolicy),
		clk:      realClock{},
		baseEnv:  buildEnv(cfg.ExtraEnv),
		sessions: make(map[string]*session),
	}
}

// newForTest creates a Backend with injected commander/clock for testing.
func newForTest(cfg Config, cmd commander, clk clock) *Backend {
	b := New(cfg)
	b.cmd = cmd
	b.clk = clk
	return b
}

// buildEnv returns the parent environment with CLAUDECODE stripped (prevents the
// CLI's nested-session detection) and ExtraEnv applied last (overriding any
// inherited var). Everything else passes through — subscription OAuth / keychain
// depend on ambient vars an allow-list would miss.
func buildEnv(extra map[string]string) []string {
	m := make(map[string]string)
	var order []string
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "CLAUDECODE" {
			continue
		}
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = v
	}
	for k, v := range extra {
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// buildArgs constructs the long-running claude CLI flags. resume passes
// --resume <sessionID> (recovery/rehydration); fresh sessions omit it and read
// the assigned id from system/init.
func (b *Backend) buildArgs(sessionID string, resume bool) []string {
	args := []string{
		"--setting-sources", b.cfg.SettingSources,
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--replay-user-messages",
	}
	if b.cfg.Model != "" {
		args = append(args, "--model", b.cfg.Model)
	}
	if b.cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", b.cfg.AppendSystemPrompt)
	}
	if resume && sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, b.cfg.ExtraArgs...)
	return args
}

// ---------------------------------------------------------------------------
// agent.Backend implementation
// ---------------------------------------------------------------------------

// Subscribe spawns a fresh claude process and returns once system/init yields
// the session id.
func (b *Backend) Subscribe(ctx context.Context, workdir string) (string, <-chan agent.Event, error) {
	s := newSession(b, "", workdir)
	id, err := s.startFresh(ctx)
	if err != nil {
		return "", nil, err
	}
	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()
	return id, s.events, nil
}

// SubscribeExisting registers a known session for lazy resume; the process is
// (re)spawned with --resume on the next SendMessage. Idempotent.
func (b *Backend) SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan agent.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[sessionID]; ok {
		return s.events, nil
	}
	s := newSession(b, sessionID, workdir)
	s.resumePending = true
	b.sessions[sessionID] = s
	return s.events, nil
}

// SendMessage writes a user turn to the session's live process, spawning
// (with --resume) first if the process is not running.
func (b *Backend) SendMessage(ctx context.Context, sessionID, content string, images []agent.Image) error {
	b.mu.Lock()
	s := b.sessions[sessionID]
	b.mu.Unlock()
	if s == nil {
		return fmt.Errorf("claude: session %q not found", sessionID)
	}
	return s.send(ctx, content, images)
}

// Cancel aborts the current turn: terminate the process group, emit Interrupted,
// and arm a --resume respawn for the next turn.
func (b *Backend) Cancel(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	s := b.sessions[sessionID]
	b.mu.Unlock()
	if s == nil {
		return fmt.Errorf("claude: session %q not found", sessionID)
	}
	s.cancel()
	return nil
}

// CloseSession permanently tears down ONE session (process group + event channel,
// once) without touching the store or other sessions.
func (b *Backend) CloseSession(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	s := b.sessions[sessionID]
	delete(b.sessions, sessionID)
	b.mu.Unlock()
	if s == nil {
		return nil
	}
	s.teardown()
	return nil
}

// Close tears down the entire backend (all sessions).
func (b *Backend) Close() error {
	b.mu.Lock()
	sessions := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.sessions = make(map[string]*session)
	b.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			s.teardown()
		}(s)
	}
	wg.Wait()
	return nil
}
