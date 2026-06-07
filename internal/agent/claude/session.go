package claude

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// session manages one long-running claude process and its event stream. The
// process is spawned lazily on the first turn (claude emits nothing until it
// receives a message) and respawned (with --resume) transparently on crash,
// cancel, or idle eviction. The event channel survives respawns and is closed
// exactly once, on CloseSession/Close or an unrecoverable resume failure.
type session struct {
	backend *Backend
	id      string
	workdir string

	events chan agent.Event
	trans  *translator

	// stdinMu serializes ALL writes to the child stdin — user turns AND
	// permission control_responses.
	stdinMu sync.Mutex

	// emitMu guards the event channel send/close so they never race.
	emitMu       sync.Mutex
	eventsClosed bool

	mu         sync.Mutex
	proc       proc
	readGen    int  // bumped on every spawn/teardown so stale readLoops are ignored
	freshSpawn bool // true until the first spawn: use --session-id, then --resume
	inFlight   bool
	closed     bool
	fatal      bool
	idleTimer  timer
}

func newSession(b *Backend, id, workdir string) *session {
	return &session{
		backend: b,
		id:      id,
		workdir: workdir,
		events:  make(chan agent.Event, 64),
		trans:   newTranslator(),
	}
}

// ---------------------------------------------------------------------------
// Spawn
// ---------------------------------------------------------------------------

// spawnLocked starts the subprocess and its read loop. Caller holds s.mu. The
// first spawn of a fresh session passes --session-id <id>; every later spawn
// (and any resumed session) passes --resume <id>.
func (s *session) spawnLocked(ctx context.Context) error {
	resume := !s.freshSpawn
	args := s.backend.buildArgs(s.id, resume)
	p, err := s.backend.cmd.Start(ctx, args, s.workdir, s.backend.baseEnv)
	if err != nil {
		return err
	}
	s.proc = p
	s.freshSpawn = false
	s.readGen++
	gen := s.readGen
	go s.readLoop(p, gen)
	return nil
}

// ---------------------------------------------------------------------------
// Read loop + protocol
// ---------------------------------------------------------------------------

func (s *session) readLoop(p proc, gen int) {
	scanner := bufio.NewScanner(p.Stdout())
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	healthy := false // produced system/init or a completed turn

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if peekType(line) == "control_request" {
			s.handleControlRequest(p, line)
			continue
		}
		for _, ev := range s.trans.translateLine(line) {
			if ev.Type == agent.EventSessionReady {
				healthy = true
			}
			if isTerminal(ev.Type) {
				healthy = true
			}
			s.emit(ev)
			if isTerminal(ev.Type) {
				s.onTerminal()
			}
		}
	}

	stderr, werr := p.Wait()
	s.onProcExit(gen, healthy, stderr, werr)
}

// handleControlRequest applies the permission policy and writes a
// control_response back to stdin (serialized via stdinMu).
func (s *session) handleControlRequest(p proc, line []byte) {
	var req struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string         `json:"subtype"`
			ToolName string         `json:"tool_name"`
			Input    map[string]any `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	if req.Request.Subtype != "can_use_tool" {
		return
	}

	res := s.backend.policy.Decide(req.Request.ToolName, req.Request.Input)
	var inner map[string]any
	if res.Behavior == "allow" {
		ui := res.UpdatedInput
		if ui == nil {
			ui = map[string]any{}
		}
		inner = map[string]any{"behavior": "allow", "updatedInput": ui}
	} else {
		msg := res.Message
		if msg == "" {
			msg = defaultDenyMessage
		}
		inner = map[string]any{"behavior": "deny", "message": msg}
	}
	resp := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": req.RequestID,
			"response":   inner,
		},
	}
	if err := s.writeLine(p, resp); err != nil {
		slog.Debug("claude: write control_response failed", "session_id", s.id, "err", err)
	}
}

// onProcExit handles unexpected subprocess exit. Superseded (cancel/evict/
// teardown/respawn bumped the gen) or closed sessions are ignored. `healthy` is
// true if the process produced system/init or a completed turn before exiting.
func (s *session) onProcExit(gen int, healthy bool, stderr string, werr error) {
	s.mu.Lock()
	if s.closed || gen != s.readGen {
		s.mu.Unlock()
		return
	}
	wasInFlight := s.inFlight
	s.proc = nil
	s.inFlight = false
	s.stopIdleTimerLocked()
	// A turn that exited before the process ever became healthy means the
	// spawn/--resume itself failed (e.g. claude has no record of the session).
	resumeFail := wasInFlight && !healthy
	if resumeFail {
		s.fatal = true
	}
	s.mu.Unlock()
	_ = werr

	switch {
	case resumeFail:
		s.emit(agent.Event{
			Type:         agent.EventTurnError,
			ErrorMessage: "claude session unrecoverable: " + tail(stderr, 500),
		})
		s.markClosed()
		s.closeEvents()
	case wasInFlight:
		// Crash mid-turn: flush the coalescer and let the next user message
		// respawn with --resume (the incomplete turn's output is lost).
		s.emit(agent.Event{Type: agent.EventInterrupted})
	}
}

// ---------------------------------------------------------------------------
// Send
// ---------------------------------------------------------------------------

func (s *session) send(ctx context.Context, content string, images []agent.Image) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errClosed
	}
	if s.fatal {
		s.mu.Unlock()
		s.emit(agent.Event{Type: agent.EventTurnError, ErrorMessage: "claude session unrecoverable"})
		return errFatal
	}
	s.inFlight = true
	s.stopIdleTimerLocked()
	if s.proc == nil {
		if err := s.spawnLocked(ctx); err != nil {
			s.inFlight = false
			s.mu.Unlock()
			s.emit(agent.Event{Type: agent.EventTurnError, ErrorMessage: "claude spawn failed: " + err.Error()})
			return err
		}
	}
	p := s.proc
	s.mu.Unlock()

	if err := s.writeLine(p, userMessage(content, images)); err != nil {
		// The process likely died between spawn and write; onProcExit will
		// surface the authoritative TurnError.
		slog.Debug("claude: write user message failed", "session_id", s.id, "err", err)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cancel / eviction / teardown
// ---------------------------------------------------------------------------

func (s *session) cancel() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	p := s.proc
	s.proc = nil
	s.readGen++ // supersede the current readLoop so it won't respawn/Interrupt
	s.inFlight = false
	s.stopIdleTimerLocked()
	s.mu.Unlock()

	if p != nil {
		_ = p.Terminate()
		go killAfterGrace(p, s.backend.cfg.GracefulStopTimeout)
	}
	s.emit(agent.Event{Type: agent.EventInterrupted})
}

// evict tears down the dormant subprocess but keeps the session id resumable.
// Never evicts an in-flight turn.
func (s *session) evict() {
	s.mu.Lock()
	if s.closed || s.inFlight {
		s.mu.Unlock()
		return
	}
	p := s.proc
	s.proc = nil
	s.readGen++
	s.mu.Unlock()

	if p != nil {
		_ = p.Terminate()
		go killAfterGrace(p, s.backend.cfg.GracefulStopTimeout)
	}
	// Event channel intentionally NOT closed — the session resumes on demand.
}

// teardown permanently stops the session: close stdin for a clean exit, escalate
// to SIGTERM/SIGKILL on the group, and close the event channel exactly once.
func (s *session) teardown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.closeEvents() // ensure channel closed even on repeat call
		return
	}
	s.closed = true
	p := s.proc
	s.proc = nil
	s.readGen++
	s.stopIdleTimerLocked()
	s.mu.Unlock()

	if p != nil {
		s.stdinMu.Lock()
		_ = p.Stdin().Close()
		s.stdinMu.Unlock()

		done := make(chan struct{})
		go func() { p.Wait(); close(done) }()
		grace := s.backend.cfg.GracefulStopTimeout
		select {
		case <-done:
		case <-time.After(grace):
			_ = p.Terminate()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = p.Kill()
			}
		}
	}
	s.closeEvents()
}

func killAfterGrace(p proc, grace time.Duration) {
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		_ = p.Kill()
	}
}

// ---------------------------------------------------------------------------
// Idle timer
// ---------------------------------------------------------------------------

func (s *session) onTerminal() {
	s.mu.Lock()
	s.inFlight = false
	s.startIdleTimerLocked()
	s.mu.Unlock()
}

func (s *session) startIdleTimerLocked() {
	s.stopIdleTimerLocked()
	if s.backend.cfg.IdleEvictionTimeout <= 0 || s.closed {
		return
	}
	s.idleTimer = s.backend.clk.AfterFunc(s.backend.cfg.IdleEvictionTimeout, s.evict)
}

func (s *session) stopIdleTimerLocked() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *session) emit(ev agent.Event) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- ev:
	default:
		slog.Warn("claude: event channel full, dropping event", "session_id", s.id, "type", ev.Type)
	}
}

func (s *session) closeEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		return
	}
	s.eventsClosed = true
	close(s.events)
}

func (s *session) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.stopIdleTimerLocked()
	s.mu.Unlock()
}

func (s *session) writeLine(p proc, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	_, err = p.Stdin().Write(append(data, '\n'))
	return err
}

func userMessage(content string, images []agent.Image) map[string]any {
	if len(images) == 0 {
		return map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": content},
		}
	}
	parts := make([]map[string]any, 0, len(images)+1)
	for _, img := range images {
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		parts = append(parts, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mt,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	parts = append(parts, map[string]any{"type": "text", "text": content})
	return map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": parts},
	}
}

func peekType(line []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line, &e)
	return e.Type
}

func isTerminal(t agent.EventType) bool {
	return t == agent.EventTurnDone || t == agent.EventTurnError || t == agent.EventInterrupted
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
