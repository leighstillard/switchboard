package claude

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// ---------------------------------------------------------------------------
// Fake exec seam
// ---------------------------------------------------------------------------

type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	closed  bool
	onClose func()
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) Close() error {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	onClose := b.onClose
	b.mu.Unlock()
	if !already && onClose != nil {
		onClose()
	}
	return nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeProc struct {
	stdoutR  *io.PipeReader
	stdoutW  *io.PipeWriter
	stdin    *syncBuffer
	waitCh   chan struct{}
	exitOnce sync.Once
	stderr   string
	exitErr  error

	mu        sync.Mutex
	termCalls int
	killCalls int
}

func newFakeProc() *fakeProc {
	r, w := io.Pipe()
	p := &fakeProc{stdoutR: r, stdoutW: w, stdin: &syncBuffer{}, waitCh: make(chan struct{})}
	p.stdin.onClose = p.exit // closing stdin (clean teardown) exits the process, like claude on EOF
	return p
}

func (p *fakeProc) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProc) Stdout() io.Reader     { return p.stdoutR }
func (p *fakeProc) Wait() (string, error) { <-p.waitCh; return p.stderr, p.exitErr }
func (p *fakeProc) Pid() int              { return 4242 }

func (p *fakeProc) Terminate() error {
	p.mu.Lock()
	p.termCalls++
	p.mu.Unlock()
	p.exit()
	return nil
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killCalls++
	p.mu.Unlock()
	p.exit()
	return nil
}

func (p *fakeProc) terms() int { p.mu.Lock(); defer p.mu.Unlock(); return p.termCalls }

func (p *fakeProc) feed(lines ...string) {
	for _, l := range lines {
		_, _ = io.WriteString(p.stdoutW, l+"\n")
	}
}

func (p *fakeProc) exitWith(stderr string, err error) {
	p.stderr = stderr
	p.exitErr = err
	p.exit()
}

func (p *fakeProc) exit() {
	p.exitOnce.Do(func() {
		_ = p.stdoutW.Close()
		close(p.waitCh)
	})
}

type startCall struct {
	args    []string
	workdir string
	env     []string
}

type fakeCommander struct {
	mu      sync.Mutex
	calls   []startCall
	newProc chan *fakeProc
}

func newFakeCommander() *fakeCommander {
	return &fakeCommander{newProc: make(chan *fakeProc, 16)}
}

func (c *fakeCommander) Start(_ context.Context, args []string, workdir string, env []string) (proc, error) {
	p := newFakeProc()
	c.mu.Lock()
	c.calls = append(c.calls, startCall{args: args, workdir: workdir, env: env})
	c.mu.Unlock()
	c.newProc <- p
	return p, nil
}

func (c *fakeCommander) waitProc(t *testing.T) *fakeProc {
	t.Helper()
	select {
	case p := <-c.newProc:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a spawned process")
		return nil
	}
}

func (c *fakeCommander) callCount() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.calls) }
func (c *fakeCommander) lastArgs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1].args
}
func (c *fakeCommander) lastEnv() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1].env
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const model = "claude-sonnet-4-20250514"

func initLine(id string) string {
	return `{"type":"system","subtype":"init","session_id":"` + id + `","model":"` + model + `"}`
}

func testBackend(t *testing.T) (*Backend, *fakeCommander, *fakeClock) {
	t.Helper()
	fc := newFakeCommander()
	clk := newFakeClock()
	return newForTest(DefaultConfig(), fc, clk), fc, clk
}

// subscribe registers a session (no process spawned yet — claude is lazy).
func subscribe(t *testing.T, b *Backend) (string, <-chan agent.Event) {
	t.Helper()
	id, ev, err := b.Subscribe(context.Background(), "/wd")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return id, ev
}

// firstTurn sends the first message (triggering the lazy spawn) and returns the
// spawned process.
func firstTurn(t *testing.T, b *Backend, fc *fakeCommander, id, content string) *fakeProc {
	t.Helper()
	if err := b.SendMessage(context.Background(), id, content, nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	return fc.waitProc(t)
}

func waitFor(t *testing.T, events <-chan agent.Event, typ agent.EventType) agent.Event {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("channel closed before %v", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %v", typ)
		}
	}
}

func expectClosed(t *testing.T, events <-chan agent.Event) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed")
		}
	}
}

func waitForStdin(t *testing.T, p *fakeProc, substr string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(p.stdin.String(), substr) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("stdin never contained %q; got: %s", substr, p.stdin.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func argHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSubscribeIsLazyNoSpawn(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, _ := subscribe(t, b)
	if fc.callCount() != 0 {
		t.Errorf("Subscribe must not spawn a process; got %d", fc.callCount())
	}
	if len(id) != 36 {
		t.Errorf("expected a generated uuid session id, got %q", id)
	}
	_ = b.Close()
}

func TestFreshSpawnArgs(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, _ := subscribe(t, b)
	firstTurn(t, b, fc, id, "hi")
	args := fc.lastArgs()

	if v, _ := argValue(args, "--setting-sources"); v != "project,local" {
		t.Errorf("--setting-sources = %q, want project,local", v)
	}
	if v, _ := argValue(args, "--session-id"); v != id {
		t.Errorf("fresh spawn --session-id = %q, want %q", v, id)
	}
	if argHasFlag(args, "--resume") {
		t.Error("fresh spawn must not have --resume")
	}
	for _, f := range []string{"--permission-prompt-tool", "--input-format", "--replay-user-messages", "--verbose"} {
		if !argHasFlag(args, f) {
			t.Errorf("missing %q", f)
		}
	}
	for _, forbidden := range []string{"--bare", "--include-partial-messages", "--permission-mode", "-p", "--print"} {
		if argHasFlag(args, forbidden) {
			t.Errorf("argv must not contain %q: %v", forbidden, args)
		}
	}
	_ = b.Close()
}

func TestEnvStripsCLAUDECODEKeepsRest(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("SWITCHBOARD_SENTINEL", "keepme")
	b := newForTest(DefaultConfig(), newFakeCommander(), newFakeClock())
	fc := b.cmd.(*fakeCommander)
	id, _ := subscribe(t, b)
	firstTurn(t, b, fc, id, "hi")

	var sawSentinel, sawClaudeCode bool
	for _, e := range fc.lastEnv() {
		if e == "SWITCHBOARD_SENTINEL=keepme" {
			sawSentinel = true
		}
		if strings.HasPrefix(e, "CLAUDECODE=") {
			sawClaudeCode = true
		}
	}
	if !sawSentinel {
		t.Error("sentinel env var should pass through")
	}
	if sawClaudeCode {
		t.Error("CLAUDECODE must be stripped")
	}
	_ = b.Close()
}

func TestExtraEnvOverrides(t *testing.T) {
	t.Setenv("OVERRIDE_ME", "old")
	cfg := DefaultConfig()
	cfg.ExtraEnv = map[string]string{"OVERRIDE_ME": "new"}
	b := newForTest(cfg, newFakeCommander(), newFakeClock())
	fc := b.cmd.(*fakeCommander)
	id, _ := subscribe(t, b)
	firstTurn(t, b, fc, id, "hi")

	count := 0
	for _, e := range fc.lastEnv() {
		if strings.HasPrefix(e, "OVERRIDE_ME=") {
			count++
			if e != "OVERRIDE_ME=new" {
				t.Errorf("override = %q, want new", e)
			}
		}
	}
	if count != 1 {
		t.Errorf("OVERRIDE_ME appears %d times, want 1 (deduped)", count)
	}
	_ = b.Close()
}

func TestSendMessageWritesUserLineAndText(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hello there")
	waitForStdin(t, p, `"type":"user"`)
	waitForStdin(t, p, "hello there")

	p.feed(
		initLine(id),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`,
		`{"type":"result","subtype":"success"}`,
	)
	ready := waitFor(t, events, agent.EventSessionReady)
	if ready.SessionID != id {
		t.Errorf("SessionReady id = %q, want %q", ready.SessionID, id)
	}
	if txt := waitFor(t, events, agent.EventTextDelta); txt.Text != "hi back" {
		t.Errorf("text = %q", txt.Text)
	}
	waitFor(t, events, agent.EventTurnDone)
	_ = b.Close()
}

func TestControlRequestAllowAll(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, _ := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "do it")
	p.feed(`{"type":"control_request","request_id":"req_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	waitForStdin(t, p, `"type":"control_response"`)
	s := p.stdin.String()
	if !strings.Contains(s, `"request_id":"req_1"`) || !strings.Contains(s, `"behavior":"allow"`) {
		t.Errorf("control_response missing allow/request_id: %s", s)
	}
	if !strings.Contains(s, `"updatedInput"`) {
		t.Errorf("allow response must carry updatedInput: %s", s)
	}
	_ = b.Close()
}

func TestControlRequestDenyAll(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PermissionPolicy = "deny_all"
	b := newForTest(cfg, newFakeCommander(), newFakeClock())
	fc := b.cmd.(*fakeCommander)
	id, _ := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "do it")
	p.feed(`{"type":"control_request","request_id":"req_2","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`)
	waitForStdin(t, p, `"type":"control_response"`)
	s := p.stdin.String()
	if !strings.Contains(s, `"behavior":"deny"`) || !strings.Contains(s, `"message"`) {
		t.Errorf("deny response must carry a message: %s", s)
	}
	_ = b.Close()
}

func TestCrashMidTurnEmitsInterruptedThenResumes(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p1 := firstTurn(t, b, fc, id, "long task")
	waitForStdin(t, p1, "long task")
	p1.feed(
		initLine(id), // healthy
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}`,
	)
	waitFor(t, events, agent.EventTextDelta)
	p1.exitWith("boom", nil) // crash, no result

	waitFor(t, events, agent.EventInterrupted)

	// Channel stays open; next turn respawns with --resume.
	if err := b.SendMessage(context.Background(), id, "retry", nil); err != nil {
		t.Fatalf("SendMessage after crash: %v", err)
	}
	fc.waitProc(t)
	if v, _ := argValue(fc.lastArgs(), "--resume"); v != id {
		t.Errorf("respawn must --resume %q, got %q (args %v)", id, v, fc.lastArgs())
	}
	_ = b.Close()
}

func TestResumeFailureSurfacesTurnErrorAndClosesChannel(t *testing.T) {
	b, fc, _ := testBackend(t)
	events, err := b.SubscribeExisting(context.Background(), "old-sess", "/wd")
	if err != nil {
		t.Fatalf("SubscribeExisting: %v", err)
	}
	if fc.callCount() != 0 {
		t.Errorf("SubscribeExisting must be lazy; got %d spawns", fc.callCount())
	}

	if err := b.SendMessage(context.Background(), "old-sess", "continue", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	p := fc.waitProc(t)
	if v, _ := argValue(fc.lastArgs(), "--resume"); v != "old-sess" {
		t.Errorf("lazy resume must --resume old-sess, got %q", v)
	}
	p.exitWith("No conversation found with session ID old-sess", nil) // exits before init

	if te := waitFor(t, events, agent.EventTurnError); te.ErrorMessage == "" {
		t.Error("TurnError must carry a message")
	}
	expectClosed(t, events)
}

func TestCancelTerminatesAndInterrupts(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "task")
	waitForStdin(t, p, "task")

	if err := b.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, events, agent.EventInterrupted)
	if p.terms() == 0 {
		t.Error("Cancel must terminate the process group")
	}
	_ = b.Close()
}

func TestCloseClosesStdinFirstNoSignalOnCleanExit(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "task")
	waitForStdin(t, p, "task")

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p.terms() != 0 {
		t.Errorf("clean exit (stdin close) must not require SIGTERM; terms=%d", p.terms())
	}
	expectClosed(t, events)
}

func TestCloseSessionIsolatesOneSession(t *testing.T) {
	b, fc, _ := testBackend(t)
	idA, evA := subscribe(t, b)
	firstTurn(t, b, fc, idA, "a")
	idB, evB := subscribe(t, b)
	pB := firstTurn(t, b, fc, idB, "b")

	if err := b.CloseSession(context.Background(), idA); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	expectClosed(t, evA)

	// Session B still works and its channel stays open.
	if err := b.SendMessage(context.Background(), idB, "still here", nil); err != nil {
		t.Fatalf("session B SendMessage: %v", err)
	}
	waitForStdin(t, pB, "still here")
	select {
	case ev, ok := <-evB:
		if !ok {
			t.Error("session B channel must stay open")
		}
		_ = ev
	default:
	}
	_ = b.Close()
}

func TestIdleEvictionTearsDownProcessKeepsSession(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IdleEvictionTimeout = 10 * time.Minute
	clk := newFakeClock()
	b := newForTest(cfg, newFakeCommander(), clk)
	fc := b.cmd.(*fakeCommander)

	id, events := subscribe(t, b)
	p1 := firstTurn(t, b, fc, id, "task")
	p1.feed(initLine(id), `{"type":"result","subtype":"success"}`)
	waitFor(t, events, agent.EventTurnDone) // terminal → idle timer armed

	time.Sleep(20 * time.Millisecond)
	clk.Advance(11 * time.Minute)

	deadline := time.After(2 * time.Second)
	for p1.terms() == 0 {
		select {
		case <-deadline:
			t.Fatal("idle eviction did not terminate the process")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case _, ok := <-events:
		if !ok {
			t.Error("idle eviction must NOT close the channel")
		}
	default:
	}

	if err := b.SendMessage(context.Background(), id, "again", nil); err != nil {
		t.Fatalf("SendMessage post-eviction: %v", err)
	}
	fc.waitProc(t)
	if v, _ := argValue(fc.lastArgs(), "--resume"); v != id {
		t.Errorf("post-eviction respawn must --resume %q, got %q", id, v)
	}
	_ = b.Close()
}

func TestIdleEvictionNeverKillsInFlight(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IdleEvictionTimeout = 10 * time.Minute
	clk := newFakeClock()
	b := newForTest(cfg, newFakeCommander(), clk)
	fc := b.cmd.(*fakeCommander)

	id, _ := subscribe(t, b)
	p1 := firstTurn(t, b, fc, id, "long build")
	waitForStdin(t, p1, "long build") // in-flight, no terminal event

	clk.Advance(30 * time.Minute)
	time.Sleep(20 * time.Millisecond)

	if p1.terms() != 0 {
		t.Errorf("in-flight turn must never be evicted; terms=%d", p1.terms())
	}
	_ = b.Close()
}

func TestSubscribeExistingIdempotent(t *testing.T) {
	b, _, _ := testBackend(t)
	ev1, err := b.SubscribeExisting(context.Background(), "s", "/wd")
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := b.SubscribeExisting(context.Background(), "s", "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if ev1 != ev2 {
		t.Error("SubscribeExisting must be idempotent (same channel)")
	}
	_ = b.Close()
}

func TestSendMessageUnknownSession(t *testing.T) {
	b, _, _ := testBackend(t)
	if err := b.SendMessage(context.Background(), "nope", "hi", nil); err == nil {
		t.Error("expected error for unknown session")
	}
}

// TestMalformedInitSurfacesProtocolError verifies the stage-2a compatibility
// probe is wired: a first system line that is not a well-formed init yields an
// explicit "(init)" TurnError and closes the channel (not a silent empty turn).
func TestMalformedInitSurfacesProtocolError(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hi")
	p.feed(`{"type":"system","subtype":"init","model":"m"}`) // missing session_id
	te := waitFor(t, events, agent.EventTurnError)
	if !strings.Contains(te.ErrorMessage, "init") {
		t.Errorf("want init protocol error, got %q", te.ErrorMessage)
	}
	expectClosed(t, events)
}

// TestPreInitNoiseThenInit verifies benign events before system/init don't skip
// the init gate: rate_limit_event + control_request are handled/skipped, then a
// valid init unblocks normal turn output.
func TestPreInitNoiseThenInit(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hi")
	p.feed(
		`{"type":"rate_limit_event","remaining":100}`,
		`{"type":"control_request","request_id":"pre","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`,
		initLine(id),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"after noise"}]}}`,
		`{"type":"result","subtype":"success"}`,
	)
	// The pre-init control_request was still answered.
	waitForStdin(t, p, `"request_id":"pre"`)
	if ready := waitFor(t, events, agent.EventSessionReady); ready.SessionID != id {
		t.Errorf("SessionReady id = %q", ready.SessionID)
	}
	if txt := waitFor(t, events, agent.EventTextDelta); txt.Text != "after noise" {
		t.Errorf("text = %q", txt.Text)
	}
	waitFor(t, events, agent.EventTurnDone)
	_ = b.Close()
}

// TestHookStartedBeforeInitSkipped: system/hook_started arriving before
// system/init must be silently skipped (not treated as a protocol violation).
func TestHookStartedBeforeInitSkipped(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hi")
	p.feed(
		`{"type":"system","subtype":"hook_started","hook_id":"h1"}`,
		initLine(id),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success"}`,
	)
	if ready := waitFor(t, events, agent.EventSessionReady); ready.SessionID != id {
		t.Errorf("SessionReady id = %q", ready.SessionID)
	}
	if txt := waitFor(t, events, agent.EventTextDelta); txt.Text != "hello" {
		t.Errorf("text = %q", txt.Text)
	}
	waitFor(t, events, agent.EventTurnDone)
	_ = b.Close()
}

// TestOutputBeforeInitRejected: assistant/result before any init is a protocol
// violation → explicit TurnError + channel close.
func TestOutputBeforeInitRejected(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hi")
	p.feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"premature"}]}}`)
	te := waitFor(t, events, agent.EventTurnError)
	if !strings.Contains(te.ErrorMessage, "init") {
		t.Errorf("want init protocol error, got %q", te.ErrorMessage)
	}
	expectClosed(t, events)
}

// TestMissingInitRejected: a process that produces only pre-init noise and then
// exits without ever emitting init surfaces a TurnError (not a silent success).
func TestMissingInitRejected(t *testing.T) {
	b, fc, _ := testBackend(t)
	id, events := subscribe(t, b)
	p := firstTurn(t, b, fc, id, "hi")
	p.feed(`{"type":"rate_limit_event","remaining":100}`)
	p.exitWith("exited before init", nil)
	te := waitFor(t, events, agent.EventTurnError)
	if te.ErrorMessage == "" {
		t.Error("missing init must surface a non-empty TurnError")
	}
	expectClosed(t, events)
}

// TestTerminalEventNeverDropped fills the event buffer, then emits a terminal
// event from another goroutine; once the consumer drains, the terminal event
// must still arrive (terminal events are never dropped).
func TestTerminalEventNeverDropped(t *testing.T) {
	b, _, _ := testBackend(t)
	id, events := subscribe(t, b)
	b.mu.Lock()
	s := b.sessions[id]
	b.mu.Unlock()

	// Saturate the buffer with non-terminal events (none drained yet).
	for i := 0; i < cap(s.events)+5; i++ {
		s.emit(agent.Event{Type: agent.EventTextDelta, Text: "x"})
	}

	emitted := make(chan struct{})
	go func() {
		s.emit(agent.Event{Type: agent.EventTurnDone}) // must block, not drop
		close(emitted)
	}()

	// Drain until the terminal event appears.
	sawDone := false
	deadline := time.After(2 * time.Second)
	for !sawDone {
		select {
		case ev := <-events:
			if ev.Type == agent.EventTurnDone {
				sawDone = true
			}
		case <-deadline:
			t.Fatal("TurnDone was dropped on a full channel")
		}
	}
	<-emitted
	_ = b.Close()
}

// TestNextTurnAfterTerminalNotEvicted guards the ordering fix: terminal idle
// bookkeeping runs BEFORE the terminal event is published, so a turn dispatched
// immediately after TurnDone is not clobbered and evicted.
func TestNextTurnAfterTerminalNotEvicted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IdleEvictionTimeout = 10 * time.Minute
	clk := newFakeClock()
	b := newForTest(cfg, newFakeCommander(), clk)
	fc := b.cmd.(*fakeCommander)

	id, events := subscribe(t, b)
	p1 := firstTurn(t, b, fc, id, "t1")
	p1.feed(initLine(id), `{"type":"result","subtype":"success"}`)
	waitFor(t, events, agent.EventTurnDone)

	// Dispatch the next turn immediately (as the router would on TurnDone).
	if err := b.SendMessage(context.Background(), id, "t2", nil); err != nil {
		t.Fatalf("SendMessage t2: %v", err)
	}
	// The new turn is in-flight; advancing past the idle timeout must NOT evict.
	clk.Advance(11 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if p1.terms() != 0 {
		t.Error("next in-flight turn was evicted (terminal bookkeeping raced the consumer)")
	}
	_ = b.Close()
}
