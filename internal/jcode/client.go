// Package jcode provides a client for connecting to jcode agent sessions
// via the Unix domain socket protocol.
//
// Architecture: one Unix socket connection per session. The Client manages
// multiple sessionConn instances internally. Each sessionConn has its own
// reader goroutine, keepalive ticker, and event channel.
package jcode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/format5/switchboard/internal/jcodeproto"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	// defaultSocketPath fallback when XDG_RUNTIME_DIR is set but no explicit path.
	defaultSocketName = "jcode.sock"
	// lockFileName is used to prevent double-spawning the daemon.
	lockFileName = "jcode-daemon.lock"

	// Reconnect backoff parameters.
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2

	// keepaliveInterval is the ping interval per connection.
	keepaliveInterval = 30 * time.Second

	// eventChanSize is the buffer size for per-session event channels.
	eventChanSize = 256

	// readerBufSize must accommodate jcode's max 32 MB lines.
	readerBufSize = jcodeproto.MaxLineSize + 4096
)

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client manages connections to the jcode daemon. Each session gets its own
// Unix socket connection (since the protocol does not tag events with
// session_id; all events on a connection belong to the subscribed session).
type Client struct {
	socketPath string
	autoSpawn  bool
	spawnCmd   string

	idGen jcodeproto.AtomicID

	mu       sync.Mutex
	sessions map[string]*sessionConn
	closed   bool
}

// NewClient creates a new jcode client. It does NOT connect immediately;
// connections are established per-session via Subscribe/SubscribeExisting.
func NewClient(socketPath string, autoSpawn bool, spawnCmd string) (*Client, error) {
	if socketPath == "" {
		socketPath = defaultSocketPath()
	}
	return &Client{
		socketPath: socketPath,
		autoSpawn:  autoSpawn,
		spawnCmd:   spawnCmd,
		sessions:   make(map[string]*sessionConn),
	}, nil
}

// defaultSocketPath resolves $XDG_RUNTIME_DIR/jcode.sock.
func defaultSocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, defaultSocketName)
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Subscribe opens a new session in the given workdir. It returns the assigned
// session ID and a channel that will receive all events for that session.
// The channel is closed when the session disconnects permanently or the client
// is closed.
//
// If the workdir already has an active session in the daemon, Subscribe returns
// the existing session (jcode won't create duplicates for the same workdir).
func (c *Client) Subscribe(ctx context.Context, workdir string) (string, <-chan *jcodeproto.ServerEvent, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", nil, errors.New("jcode: client closed")
	}
	// Check if we already have a session for this workdir.
	for _, sc := range c.sessions {
		if sc.workdir == workdir {
			c.mu.Unlock()
			slog.Info("jcode: reusing existing session for workdir", "session_id", sc.sessionID, "workdir", workdir)
			return sc.sessionID, sc.events, nil
		}
	}
	c.mu.Unlock()

	if err := c.ensureDaemon(ctx); err != nil {
		return "", nil, fmt.Errorf("jcode: ensure daemon: %w", err)
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("jcode: dial: %w", err)
	}

	sc := &sessionConn{
		client:  c,
		workdir: workdir,
		conn:    conn,
		reader:  bufio.NewReaderSize(conn, readerBufSize),
		events:  make(chan *jcodeproto.ServerEvent, eventChanSize),
	}
	sc.ctx, sc.cancel = context.WithCancel(context.Background())

	// Send subscribe request.
	reqID := c.idGen.Next()
	req := jcodeproto.NewSubscribe(reqID, workdir)
	if err := sc.writeJSON(req); err != nil {
		conn.Close()
		return "", nil, fmt.Errorf("jcode: send subscribe: %w", err)
	}

	// Wait for the session event that tells us the session_id.
	sessionID, err := sc.waitForSession(ctx)
	if err != nil {
		conn.Close()
		return "", nil, fmt.Errorf("jcode: wait for session: %w", err)
	}

	sc.sessionID = sessionID

	c.mu.Lock()
	c.sessions[sessionID] = sc
	c.mu.Unlock()

	// Start reader and keepalive goroutines.
	go sc.readLoop()
	go sc.keepaliveLoop()

	slog.Info("jcode: subscribed to new session", "session_id", sessionID, "workdir", workdir)
	return sessionID, sc.events, nil
}

// SubscribeExisting reconnects to an existing session by ID.
// workdir is stored for deduplication (Subscribe reuses sessions with matching workdir).
func (c *Client) SubscribeExisting(ctx context.Context, targetSessionID, workdir string) (<-chan *jcodeproto.ServerEvent, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("jcode: client closed")
	}
	// Check if we already have this session connected.
	if sc, ok := c.sessions[targetSessionID]; ok {
		c.mu.Unlock()
		slog.Info("jcode: reusing existing connection for session", "session_id", targetSessionID)
		return sc.events, nil
	}
	c.mu.Unlock()

	if err := c.ensureDaemon(ctx); err != nil {
		return nil, fmt.Errorf("jcode: ensure daemon: %w", err)
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("jcode: dial: %w", err)
	}

	sc := &sessionConn{
		client:    c,
		sessionID: targetSessionID,
		workdir:   workdir,
		conn:      conn,
		reader:    bufio.NewReaderSize(conn, readerBufSize),
		events:    make(chan *jcodeproto.ServerEvent, eventChanSize),
	}
	sc.ctx, sc.cancel = context.WithCancel(context.Background())

	reqID := c.idGen.Next()
	req := jcodeproto.NewSubscribeResume(reqID, targetSessionID, false)
	if err := sc.writeJSON(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("jcode: send subscribe: %w", err)
	}

	// Wait for session confirmation.
	gotID, err := sc.waitForSession(ctx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("jcode: wait for session: %w", err)
	}
	if gotID != targetSessionID {
		slog.Warn("jcode: session ID mismatch", "expected", targetSessionID, "got", gotID)
		sc.sessionID = gotID
	}

	c.mu.Lock()
	c.sessions[sc.sessionID] = sc
	c.mu.Unlock()

	go sc.readLoop()
	go sc.keepaliveLoop()

	slog.Info("jcode: subscribed to existing session", "session_id", sc.sessionID)
	return sc.events, nil
}

// SendMessage sends a user message to the specified session.
func (c *Client) SendMessage(ctx context.Context, sessionID string, content string, images []jcodeproto.ImagePair) error {
	sc, err := c.getSession(sessionID)
	if err != nil {
		return err
	}

	reqID := c.idGen.Next()
	req := jcodeproto.NewMessage(reqID, content, nil)
	if len(images) > 0 {
		req.Images = images
	}

	if err := sc.writeJSON(req); err != nil {
		return fmt.Errorf("jcode: send message: %w", err)
	}
	slog.Debug("jcode: sent message", "session_id", sessionID, "req_id", reqID, "len", len(content))
	return nil
}

// Cancel aborts the current generation turn for the specified session.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	sc, err := c.getSession(sessionID)
	if err != nil {
		return err
	}

	reqID := c.idGen.Next()
	req := jcodeproto.NewCancel(reqID)

	if err := sc.writeJSON(req); err != nil {
		return fmt.Errorf("jcode: send cancel: %w", err)
	}
	slog.Debug("jcode: sent cancel", "session_id", sessionID, "req_id", reqID)
	return nil
}

// Close shuts down all session connections and marks the client as closed.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sessions := make([]*sessionConn, 0, len(c.sessions))
	for _, sc := range c.sessions {
		sessions = append(sessions, sc)
	}
	c.sessions = nil
	c.mu.Unlock()

	for _, sc := range sessions {
		sc.close()
	}
	slog.Info("jcode: client closed", "sessions_closed", len(sessions))
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (c *Client) getSession(sessionID string) (*sessionConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("jcode: client closed")
	}
	sc, ok := c.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("jcode: session %q not found", sessionID)
	}
	return sc, nil
}

func (c *Client) removeSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, sessionID)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ensureDaemon checks if the socket exists and spawns jcode if needed.
func (c *Client) ensureDaemon(ctx context.Context) error {
	// Try to stat the socket first.
	if _, err := os.Stat(c.socketPath); err == nil {
		// Socket file exists; try a quick connection to verify liveness.
		conn, err := net.DialTimeout("unix", c.socketPath, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		slog.Warn("jcode: socket exists but not connectable, will spawn", "err", err)
	}

	if !c.autoSpawn {
		return fmt.Errorf("daemon not running at %s and auto_spawn disabled", c.socketPath)
	}

	return c.spawnDaemon(ctx)
}

// spawnDaemon starts `jcode serve` with setsid, guarded by flock to prevent
// double-spawning.
func (c *Client) spawnDaemon(ctx context.Context) error {
	runtimeDir := filepath.Dir(c.socketPath)
	lockPath := filepath.Join(runtimeDir, lockFileName)

	// Acquire advisory lock (non-blocking).
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()

	err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		// Another process is spawning the daemon; wait and retry connect.
		slog.Info("jcode: another process is spawning daemon, waiting")
		return c.waitForSocket(ctx, 10*time.Second)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	spawnCmd := c.spawnCmd
	if spawnCmd == "" {
		spawnCmd = "jcode serve"
	}

	slog.Info("jcode: spawning daemon", "cmd", spawnCmd)

	// Use setsid so the daemon outlives this process.
	cmd := exec.CommandContext(ctx, "setsid", "sh", "-c", spawnCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}

	// Detach - don't wait for the daemon process.
	go cmd.Wait()

	// Wait for socket to become connectable.
	return c.waitForSocket(ctx, 15*time.Second)
}

// waitForSocket polls until the socket is connectable or timeout.
func (c *Client) waitForSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := net.DialTimeout("unix", c.socketPath, time.Second)
		if err == nil {
			conn.Close()
			slog.Info("jcode: daemon is ready")
			return nil
		}
		time.Sleep(interval)
		if interval < time.Second {
			interval *= 2
		}
	}
	return fmt.Errorf("daemon did not become ready within %v", timeout)
}

// ---------------------------------------------------------------------------
// sessionConn - per-session connection
// ---------------------------------------------------------------------------

type sessionConn struct {
	client    *Client
	sessionID string
	workdir   string

	conn   net.Conn
	reader *bufio.Reader

	writeMu sync.Mutex // serialize writes to the socket
	events  chan *jcodeproto.ServerEvent

	ctx    context.Context
	cancel context.CancelFunc

	closedOnce sync.Once
}

// writeJSON marshals v and writes it as a single NDJSON line.
func (sc *sessionConn) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()

	if sc.conn == nil {
		return errors.New("connection closed")
	}
	// Set a generous write deadline.
	sc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = sc.conn.Write(data)
	return err
}

// waitForSession reads events until we get a "swarm_status" event containing
// the session_id, or the "done" event for our subscribe request.
// The session_id is extracted from the first member in swarm_status.
func (sc *sessionConn) waitForSession(ctx context.Context) (string, error) {
	// Give the daemon up to 30s to respond.
	deadline := time.Now().Add(30 * time.Second)
	sc.conn.SetReadDeadline(deadline)
	defer sc.conn.SetReadDeadline(time.Time{})

	var sessionID string

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		line, err := sc.reader.ReadBytes('\n')
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}

		evType, raw, err := jcodeproto.ParseServerEvent(line)
		if err != nil {
			slog.Warn("jcode: parse error during session wait", "err", err)
			continue
		}

		slog.Debug("jcode: waitForSession event", "type", evType, "raw_len", len(raw))

		switch evType {
		case jcodeproto.EventSwarmStatus:
			var swarmEv jcodeproto.SwarmStatusEvent
			if err := json.Unmarshal(raw, &swarmEv); err != nil {
				slog.Warn("jcode: failed to parse swarm_status", "err", err)
				continue
			}
			if len(swarmEv.Members) > 0 {
				sessionID = swarmEv.Members[0].SessionID
				slog.Debug("jcode: got session from swarm_status",
					"session_id", sessionID,
					"friendly_name", swarmEv.Members[0].FriendlyName,
				)
			}

		case jcodeproto.EventHistory:
			// Resume flow: history event contains the session_id.
			// Only extract session_id (the event can be very large).
			var partial struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(raw, &partial); err == nil && partial.SessionID != "" {
				sessionID = partial.SessionID
				slog.Debug("jcode: got session from history event", "session_id", sessionID)
			}

		case jcodeproto.EventSession:
			// Legacy path: some versions may still emit "session" events.
			var sessEv jcodeproto.SessionEvent
			if err := json.Unmarshal(raw, &sessEv); err == nil && sessEv.SessionID != "" {
				sessionID = sessEv.SessionID
			}

		case jcodeproto.EventDone:
			// Subscribe is complete. Return whatever session_id we found.
			if sessionID != "" {
				return sessionID, nil
			}
			return "", fmt.Errorf("subscribe completed but no session_id received")

		case jcodeproto.EventError:
			var errEv jcodeproto.ErrorEvent
			if json.Unmarshal(raw, &errEv) == nil {
				return "", fmt.Errorf("server error: %s", errEv.Message)
			}
			return "", fmt.Errorf("server error (unparseable)")

		case jcodeproto.EventAck:
			// Expected; continue waiting for swarm_status and done.
			continue

		default:
			// Buffer other events that arrive before subscribe completes.
			ev := &jcodeproto.ServerEvent{Type: evType, Raw: raw}
			select {
			case sc.events <- ev:
			default:
				slog.Warn("jcode: event channel full during session wait, dropping", "type", evType)
			}
		}
	}
}

// readLoop continuously reads events from the connection and dispatches them.
func (sc *sessionConn) readLoop() {
	defer sc.handleDisconnect()

	for {
		select {
		case <-sc.ctx.Done():
			return
		default:
		}

		// No read deadline - keepalive pings detect dead connections.
		line, err := sc.reader.ReadBytes('\n')
		if err != nil {
			if sc.ctx.Err() != nil {
				return // intentional shutdown
			}
			slog.Warn("jcode: read error", "session_id", sc.sessionID, "err", err)
			return
		}

		evType, raw, err := jcodeproto.ParseServerEvent(line)
		if err != nil {
			slog.Debug("jcode: unparseable event line", "session_id", sc.sessionID, "err", err)
			continue
		}

		// Handle reloading event: triggers reconnect.
		if evType == jcodeproto.EventReloading {
			slog.Info("jcode: daemon reloading, will reconnect", "session_id", sc.sessionID)
			return // handleDisconnect will attempt reconnect
		}

		ev := &jcodeproto.ServerEvent{Type: evType, Raw: raw}
		select {
		case sc.events <- ev:
		case <-sc.ctx.Done():
			return
		default:
			slog.Warn("jcode: event channel full, dropping event", "session_id", sc.sessionID, "type", evType)
		}
	}
}

// keepaliveLoop sends periodic pings to detect dead connections.
func (sc *sessionConn) keepaliveLoop() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sc.ctx.Done():
			return
		case <-ticker.C:
			reqID := sc.client.idGen.Next()
			if err := sc.writeJSON(jcodeproto.NewPing(reqID)); err != nil {
				slog.Warn("jcode: keepalive ping failed", "session_id", sc.sessionID, "err", err)
				// The read loop will detect the broken connection.
				return
			}
		}
	}
}

// handleDisconnect is called when the read loop exits unexpectedly.
// It attempts exponential backoff reconnection.
func (sc *sessionConn) handleDisconnect() {
	// Check if this is an intentional close.
	if sc.ctx.Err() != nil {
		sc.closeEvents()
		return
	}

	slog.Info("jcode: session disconnected, attempting reconnect", "session_id", sc.sessionID)

	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		select {
		case <-sc.ctx.Done():
			sc.closeEvents()
			return
		case <-time.After(backoff):
		}

		if err := sc.reconnect(); err != nil {
			slog.Warn("jcode: reconnect failed",
				"session_id", sc.sessionID,
				"attempt", attempt,
				"backoff", backoff,
				"err", err,
			)
			backoff = time.Duration(float64(backoff) * backoffFactor)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		slog.Info("jcode: reconnected", "session_id", sc.sessionID, "attempt", attempt)
		// Restart the read loop (keepalive is restarted here too).
		go sc.readLoop()
		go sc.keepaliveLoop()
		return
	}
}

// reconnect creates a new connection and re-subscribes to the session.
func (sc *sessionConn) reconnect() error {
	// Ensure daemon is running.
	ctx, cancel := context.WithTimeout(sc.ctx, 30*time.Second)
	defer cancel()

	if err := sc.client.ensureDaemon(ctx); err != nil {
		return err
	}

	conn, err := sc.client.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// Replace connection state.
	if sc.conn != nil {
		sc.conn.Close()
	}
	sc.conn = conn
	sc.reader = bufio.NewReaderSize(conn, readerBufSize)

	// Re-subscribe.
	reqID := sc.client.idGen.Next()
	req := jcodeproto.NewSubscribeResume(reqID, sc.sessionID, true)
	if err := sc.writeJSON(req); err != nil {
		conn.Close()
		return fmt.Errorf("send subscribe: %w", err)
	}

	// Wait for session confirmation.
	gotID, err := sc.waitForSession(ctx)
	if err != nil {
		conn.Close()
		return fmt.Errorf("wait for session: %w", err)
	}
	if gotID != sc.sessionID {
		slog.Warn("jcode: session ID changed on reconnect", "old", sc.sessionID, "new", gotID)
		// Update in client map.
		sc.client.mu.Lock()
		delete(sc.client.sessions, sc.sessionID)
		sc.sessionID = gotID
		sc.client.sessions[gotID] = sc
		sc.client.mu.Unlock()
	}

	return nil
}

// close terminates the session connection.
func (sc *sessionConn) close() {
	sc.cancel()
	if sc.conn != nil {
		sc.conn.Close()
	}
	sc.closeEvents()
	sc.client.removeSession(sc.sessionID)
}

// closeEvents closes the event channel exactly once.
func (sc *sessionConn) closeEvents() {
	sc.closedOnce.Do(func() {
		close(sc.events)
	})
}
