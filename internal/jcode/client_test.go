package jcode

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/jcodeproto"
)

// TestSubscribeRetryOnNoSessionID verifies that Subscribe retries when the
// daemon returns done without a session_id (race condition).
func TestSubscribeRetryOnNoSessionID(t *testing.T) {
	// Create a fake Unix socket server that simulates the race condition
	// on the first attempt and succeeds on the second.
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "jcode.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	attempt := 0
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempt++
			go handleFakeConn(t, conn, attempt)
		}
	}()

	// Create a client pointing at our fake socket (autoSpawn=false so it
	// won't try to start a real daemon).
	client := &Client{
		socketPath: sockPath,
		autoSpawn:  false,
		sessions:   make(map[string]*sessionConn),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessionID, events, err := client.Subscribe(ctx, "/tmp/test-workdir")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	if sessionID != "session_test_123" {
		t.Errorf("expected session_test_123, got %s", sessionID)
	}
	if events == nil {
		t.Error("expected non-nil events channel")
	}

	// Verify it took 2 attempts (first failed with race, second succeeded).
	if attempt != 2 {
		t.Errorf("expected 2 attempts, got %d", attempt)
	}

	// Close client to stop background goroutines.
	client.Close()
}

// TestSubscribeSucceedsFirstAttempt verifies the happy path (no retry needed).
func TestSubscribeSucceedsFirstAttempt(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "jcode.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Keep serving connections (the readLoop will reconnect after server closes).
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				reader := bufio.NewReader(c)
				_, err := reader.ReadBytes('\n')
				if err != nil {
					c.Close()
					return
				}
				// Send swarm_status with session_id then done.
				writeEvent(c, jcodeproto.EventAck, map[string]any{"id": 1})
				writeEvent(c, jcodeproto.EventSwarmStatus, map[string]any{
					"members": []map[string]any{
						{
							"session_id":    "session_test_456",
							"friendly_name": "test",
							"status":        "ready",
							"working_dir":   "/tmp/test-workdir-2",
						},
					},
				})
				writeEvent(c, jcodeproto.EventDone, map[string]any{"id": 1})
				// Keep alive for the readLoop.
				time.Sleep(200 * time.Millisecond)
				c.Close()
			}(conn)
		}
	}()

	client := &Client{
		socketPath: sockPath,
		autoSpawn:  false,
		sessions:   make(map[string]*sessionConn),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessionID, _, err := client.Subscribe(ctx, "/tmp/test-workdir-2")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	if sessionID != "session_test_456" {
		t.Errorf("expected session_test_456, got %s", sessionID)
	}

	// Close client to stop background goroutines.
	client.Close()
	// Give goroutines time to clean up.
	time.Sleep(50 * time.Millisecond)
}

// handleFakeConn simulates the jcode daemon's response.
func handleFakeConn(t *testing.T, conn net.Conn, attempt int) {
	t.Helper()
	defer conn.Close()

	reader := bufio.NewReader(conn)
	// Read the subscribe request.
	_, err := reader.ReadBytes('\n')
	if err != nil {
		t.Logf("read subscribe request: %v", err)
		return
	}

	if attempt == 1 {
		// First attempt: simulate race - send ack then done without session_id.
		writeEvent(conn, jcodeproto.EventAck, map[string]any{"id": 1})
		writeEvent(conn, jcodeproto.EventDone, map[string]any{"id": 1})
	} else {
		// Second attempt: send swarm_status with session_id then done.
		writeEvent(conn, jcodeproto.EventAck, map[string]any{"id": 2})
		writeEvent(conn, jcodeproto.EventSwarmStatus, map[string]any{
			"members": []map[string]any{
				{
					"session_id":    "session_test_123",
					"friendly_name": "test",
					"status":        "ready",
					"working_dir":   "/tmp/test-workdir",
				},
			},
		})
		writeEvent(conn, jcodeproto.EventDone, map[string]any{"id": 2})
		// Keep connection alive briefly for read loop.
		time.Sleep(100 * time.Millisecond)
	}
}

func writeEvent(conn net.Conn, evType string, payload map[string]any) {
	payload["type"] = evType
	data, _ := json.Marshal(payload)
	conn.Write(append(data, '\n'))
}

// handleFakeConnKeepAlive subscribes successfully then blocks until the client
// closes the connection (so the session stays alive until CloseSession).
func handleFakeConnKeepAlive(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadBytes('\n'); err != nil {
		return
	}
	writeEvent(conn, jcodeproto.EventAck, map[string]any{"id": 1})
	writeEvent(conn, jcodeproto.EventSwarmStatus, map[string]any{
		"members": []map[string]any{
			{"session_id": "session_test_123", "friendly_name": "test", "status": "ready", "working_dir": "/tmp/wd"},
		},
	})
	writeEvent(conn, jcodeproto.EventDone, map[string]any{"id": 1})
	// Block until the client closes the connection.
	_, _ = reader.ReadBytes('\n')
}

func TestCloseSessionRemovesOnlyThatSession(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "jcode.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleFakeConnKeepAlive(t, conn)
		}
	}()

	client := &Client{socketPath: sockPath, autoSpawn: false, sessions: make(map[string]*sessionConn)}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, events, err := client.Subscribe(ctx, "/tmp/wd")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := client.getSession(id); err != nil {
		t.Fatalf("session should be registered: %v", err)
	}

	if err := client.CloseSession(id); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, err := client.getSession(id); err == nil {
		t.Error("session should be removed after CloseSession")
	}

	// The session's event channel must close.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("events channel not closed after CloseSession")
		}
	}
}
