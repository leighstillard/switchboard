package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"

	"github.com/format5/switchboard/internal/agent"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Exec seam: allows tests to inject canned output without invoking real binary
// ---------------------------------------------------------------------------

// commander abstracts process execution for testability.
type commander interface {
	// Start launches the process and returns its stdout reader.
	// The process should be killed when the returned cancel func is called.
	Start(ctx context.Context, args []string, workdir string) (stdout io.ReadCloser, cancel func(), pid int, err error)
}

// realCommander executes the actual Claude binary.
type realCommander struct {
	binary string
}

func (r *realCommander) Start(ctx context.Context, args []string, workdir string) (io.ReadCloser, func(), int, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = workdir
	// Set process group so we can signal the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, 0, fmt.Errorf("start claude: %w", err)
	}

	cancel := func() {
		// Send SIGINT to process group.
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		}
	}

	// Wait for process in background to clean up resources.
	go cmd.Wait()

	return stdout, cancel, cmd.Process.Pid, nil
}

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

type session struct {
	id      string
	workdir string
	model   string

	events chan agent.Event
	trans  *translator

	// mu guards process-related fields.
	mu         sync.Mutex
	cancelProc func() // cancels the running process, if any
	turnCount  int     // incremented on each SendMessage
	closed     bool
}

// ---------------------------------------------------------------------------
// Backend
// ---------------------------------------------------------------------------

// Config holds Claude backend configuration.
type Config struct {
	Binary             string
	PermissionMode     string
	Model              string
	AppendSystemPrompt string
	ExtraArgs          []string
}

// DefaultConfig returns the default Claude backend configuration.
func DefaultConfig() Config {
	return Config{
		Binary:         "claude",
		PermissionMode: "bypassPermissions",
		Model:          "claude-sonnet-4-20250514",
	}
}

// Backend implements agent.Backend for Claude Code CLI.
type Backend struct {
	cfg Config
	cmd commander

	mu       sync.Mutex
	sessions map[string]*session
}

// New creates a new Claude Code backend with the given configuration.
func New(cfg Config) *Backend {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-20250514"
	}

	return &Backend{
		cfg:      cfg,
		cmd:      &realCommander{binary: cfg.Binary},
		sessions: make(map[string]*session),
	}
}

// newForTest creates a Backend with a custom commander for testing.
func newForTest(cfg Config, cmd commander) *Backend {
	b := New(cfg)
	b.cmd = cmd
	return b
}

func (b *Backend) Subscribe(ctx context.Context, workdir string) (string, <-chan agent.Event, error) {
	id := uuid.New().String()

	s := &session{
		id:      id,
		workdir: workdir,
		model:   b.cfg.Model,
		events:  make(chan agent.Event, 64),
		trans:   newTranslator(),
	}

	b.mu.Lock()
	b.sessions[id] = s
	b.mu.Unlock()

	return id, s.events, nil
}

func (b *Backend) SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan agent.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if s, ok := b.sessions[sessionID]; ok {
		return s.events, nil
	}

	// Create session state for an existing Claude session.
	// Process will spawn with --resume on next SendMessage.
	s := &session{
		id:        sessionID,
		workdir:   workdir,
		model:     b.cfg.Model,
		events:    make(chan agent.Event, 64),
		trans:     newTranslator(),
		turnCount: 1, // Mark as resumed so --resume is used.
	}
	b.sessions[sessionID] = s

	return s.events, nil
}

func (b *Backend) SendMessage(ctx context.Context, sessionID, content string, images []agent.Image) error {
	b.mu.Lock()
	s, ok := b.sessions[sessionID]
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("claude: session %q not found", sessionID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("claude: session %q is closed", sessionID)
	}

	isFirstTurn := s.turnCount == 0
	s.turnCount++
	s.mu.Unlock()

	// Build CLI args.
	args := b.buildArgs(sessionID, content, isFirstTurn)

	// Start process.
	stdout, cancelProc, _, err := b.cmd.Start(ctx, args, s.workdir)
	if err != nil {
		return fmt.Errorf("claude: start process: %w", err)
	}

	s.mu.Lock()
	s.cancelProc = cancelProc
	s.mu.Unlock()

	// Read stdout in background and translate events.
	go b.readAndTranslate(s, stdout)

	return nil
}

func (b *Backend) Cancel(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	s, ok := b.sessions[sessionID]
	b.mu.Unlock()

	if !ok {
		return fmt.Errorf("claude: session %q not found", sessionID)
	}

	s.mu.Lock()
	cancel := s.cancelProc
	s.cancelProc = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Emit interrupted event.
	select {
	case s.events <- agent.Event{Type: agent.EventInterrupted}:
	default:
	}

	return nil
}

func (b *Backend) Close() error {
	b.mu.Lock()
	sessions := make(map[string]*session, len(b.sessions))
	for k, v := range b.sessions {
		sessions[k] = v
	}
	b.mu.Unlock()

	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		if s.cancelProc != nil {
			s.cancelProc()
			s.cancelProc = nil
		}
		s.mu.Unlock()
		close(s.events)
	}

	b.mu.Lock()
	b.sessions = make(map[string]*session)
	b.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

// buildArgs constructs the Claude CLI arguments for a turn.
func (b *Backend) buildArgs(sessionID, content string, isFirstTurn bool) []string {
	args := []string{
		"-p", content,
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--model", b.cfg.Model,
		"--permission-mode", b.cfg.PermissionMode,
	}

	if isFirstTurn {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}

	if b.cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", b.cfg.AppendSystemPrompt)
	}

	args = append(args, b.cfg.ExtraArgs...)

	return args
}

// readAndTranslate reads stdout line by line, translates to agent.Events,
// and sends them to the session's event channel.
func (b *Backend) readAndTranslate(s *session, stdout io.ReadCloser) {
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	// Claude can emit large JSON lines; increase the buffer.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	sawResult := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		events := s.trans.translateLine(line)
		for _, ev := range events {
			if ev.Type == agent.EventTurnDone || ev.Type == agent.EventTurnError {
				sawResult = true
			}
			select {
			case s.events <- ev:
			default:
				slog.Warn("claude: event channel full, dropping event",
					"session_id", s.id, "type", ev.Type)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Debug("claude: scanner error", "session_id", s.id, "err", err)
	}

	// If process exited without a result event, emit TurnError.
	if !sawResult {
		select {
		case s.events <- agent.Event{
			Type:         agent.EventTurnError,
			ErrorMessage: "claude process exited without result",
		}:
		default:
		}
	}

	// Clear the cancel function since the process is done.
	s.mu.Lock()
	s.cancelProc = nil
	s.mu.Unlock()
}
