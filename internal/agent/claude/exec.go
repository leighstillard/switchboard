package claude

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// ---------------------------------------------------------------------------
// Exec seam: one long-running claude subprocess per session, injectable for tests
// ---------------------------------------------------------------------------

// proc is one spawned claude process. It is driven over stdin (turns +
// permission responses) and stdout (events), with a bounded stderr surfaced on
// abnormal exit. Terminate/Kill act on the whole process group (see proc_unix).
type proc interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Wait blocks until the process exits, returning the (bounded) captured
	// stderr and the exit error (nil on clean exit).
	Wait() (stderr string, err error)
	// Terminate sends SIGTERM to the process group (graceful).
	Terminate() error
	// Kill sends SIGKILL to the process group (last resort).
	Kill() error
	Pid() int
}

// commander spawns processes; the real implementation runs the claude binary,
// tests inject a fake.
type commander interface {
	Start(ctx context.Context, args []string, workdir string, env []string) (proc, error)
}

// maxStderrBytes bounds the captured stderr so a chatty/looping child cannot
// grow memory without limit; we keep the most recent bytes (errors trail).
const maxStderrBytes = 16 * 1024

// boundedBuffer is an io.Writer that retains only the last maxBytes written.
type boundedBuffer struct {
	mu       sync.Mutex
	buf      []byte
	maxBytes int
}

func newBoundedBuffer(maxBytes int) *boundedBuffer {
	return &boundedBuffer{maxBytes: maxBytes}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.maxBytes {
		b.buf = b.buf[len(b.buf)-b.maxBytes:]
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// ---------------------------------------------------------------------------
// Real implementation
// ---------------------------------------------------------------------------

type realCommander struct {
	binary string
}

func (r *realCommander) Start(ctx context.Context, args []string, workdir string, env []string) (proc, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = workdir
	cmd.Env = env
	prepareCmdForKill(cmd) // setpgid so the whole tree can be group-killed

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude: stdout pipe: %w", err)
	}
	stderr := newBoundedBuffer(maxStderrBytes)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: start: %w", err)
	}

	return &realProc{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type realProc struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   *boundedBuffer
	waitOnce sync.Once
	waitErr  error
}

func (p *realProc) Stdin() io.WriteCloser { return p.stdin }
func (p *realProc) Stdout() io.Reader     { return p.stdout }

func (p *realProc) Wait() (string, error) {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.stderr.String(), p.waitErr
}

func (p *realProc) Terminate() error { return signalProcessGroup(p.cmd, sigTerm) }
func (p *realProc) Kill() error      { return forceKillCmd(p.cmd) }

func (p *realProc) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
