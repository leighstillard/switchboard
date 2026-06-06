//go:build integration

// Package integration contains tests that require a live jcode daemon.
// Run with: go test -tags integration ./test/integration/ -v -timeout 120s
package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/jcodeproto"
	"github.com/format5/switchboard/test/testutil"
)

// testWorkdir is a safe directory for creating test sessions.
const testWorkdir = "/tmp/switchboard-integration-test"

func newTestClient(t *testing.T) *jcode.Client {
	t.Helper()
	c, err := jcode.NewClient(testutil.JcodeSocketPath(), false, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// subscribeWithRetry attempts Subscribe up to 3 times with backoff.
// This handles the case where the daemon is still cleaning up sessions
// from previous test runs.
func subscribeWithRetry(t *testing.T, ctx context.Context, workdir string) (*jcode.Client, string, <-chan *jcodeproto.ServerEvent) {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			t.Logf("Retrying Subscribe (attempt %d)...", attempt+1)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		c, err := jcode.NewClient(testutil.JcodeSocketPath(), false, "")
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		sessionID, events, err := c.Subscribe(ctx, workdir)
		if err == nil {
			t.Cleanup(func() { c.Close() })
			return c, sessionID, events
		}
		lastErr = err
		c.Close()
	}
	t.Fatalf("Subscribe failed after 3 attempts: %v", lastErr)
	return nil, "", nil
}

// TestJcodeClient runs all jcode client integration tests sequentially
// to avoid overwhelming the daemon with concurrent session creation.
func TestJcodeClient(t *testing.T) {
	// Shared session state across subtests.
	var sharedSessionID string

	t.Run("Subscribe_NewSession", func(t *testing.T) {
		c := newTestClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sessionID, events, err := c.Subscribe(ctx, testWorkdir)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		if sessionID == "" {
			t.Fatal("Subscribe returned empty session_id")
		}
		t.Logf("Created session: %s", sessionID)
		sharedSessionID = sessionID

		if events == nil {
			t.Fatal("Subscribe returned nil events channel")
		}
	})

	time.Sleep(500 * time.Millisecond)

	t.Run("SubscribeExisting_Resume", func(t *testing.T) {
		if sharedSessionID == "" {
			t.Skip("no session from previous test")
		}

		c := newTestClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		events, _, err := c.SubscribeExisting(ctx, sharedSessionID, testWorkdir)
		if err != nil {
			t.Fatalf("SubscribeExisting: %v", err)
		}

		if events == nil {
			t.Fatal("SubscribeExisting returned nil events channel")
		}
		t.Logf("Resumed session: %s", sharedSessionID)
	})

	time.Sleep(500 * time.Millisecond)

	t.Run("SendMessage_ReceiveEvents", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		c, sessionID, events := subscribeWithRetry(t, ctx, testWorkdir)
		_ = c
		t.Logf("Session: %s", sessionID)

		// Send a trivial message that should produce a quick response.
		err := c.SendMessage(ctx, sessionID, "Respond with exactly: PONG", nil)
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		// Collect events until we get a "done" event or timeout.
		var (
			gotTextDelta bool
			gotDone      bool
			textContent  string
			eventTypes   []string
		)

		deadline := time.After(40 * time.Second)
		for !gotDone {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatal("events channel closed before done")
				}
				eventTypes = append(eventTypes, ev.Type)
				switch ev.Type {
				case jcodeproto.EventTextDelta:
					var e jcodeproto.TextDeltaEvent
					if json.Unmarshal(ev.Raw, &e) == nil {
						textContent += e.Text
						gotTextDelta = true
					}
				case jcodeproto.EventDone:
					gotDone = true
				case jcodeproto.EventError:
					var e jcodeproto.ErrorEvent
					json.Unmarshal(ev.Raw, &e)
					t.Fatalf("Received error event: %s", e.Message)
				}
			case <-deadline:
				t.Fatalf("Timed out waiting for done event. Got types: %v", eventTypes)
			}
		}

		if !gotTextDelta {
			t.Error("Expected at least one text_delta event")
		}
		t.Logf("Received %d events, types: %v", len(eventTypes), uniqueTypes(eventTypes))
		t.Logf("Response text (first 200 chars): %.200s", textContent)
	})

	// Longer pause before Cancel - the daemon needs time to release the
	// previous session's socket resources.
	time.Sleep(2 * time.Second)

	t.Run("Cancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		c, sessionID, events := subscribeWithRetry(t, ctx, testWorkdir)
		t.Logf("Session: %s", sessionID)

		// Send a message that will trigger a longer response.
		err := c.SendMessage(ctx, sessionID, "Write a 500-word essay about testing software.", nil)
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		// Wait briefly for streaming to begin, then cancel.
		time.Sleep(2 * time.Second)

		err = c.Cancel(ctx, sessionID)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		t.Log("Cancel sent")

		// We should eventually get either an interrupted or done event.
		var gotTerminal bool
		deadline := time.After(15 * time.Second)
		for !gotTerminal {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatal("events channel closed without terminal event")
				}
				switch ev.Type {
				case jcodeproto.EventInterrupted:
					t.Log("Got interrupted event")
					gotTerminal = true
				case jcodeproto.EventDone:
					t.Log("Got done event (response may have finished before cancel)")
					gotTerminal = true
				case jcodeproto.EventError:
					var e jcodeproto.ErrorEvent
					json.Unmarshal(ev.Raw, &e)
					t.Logf("Got error event: %s", e.Message)
					gotTerminal = true
				}
			case <-deadline:
				t.Fatal("Timed out waiting for terminal event after cancel")
			}
		}
	})

	time.Sleep(2 * time.Second)

	t.Run("Keepalive_Connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		c, sessionID, events := subscribeWithRetry(t, ctx, testWorkdir)
		_ = c
		t.Logf("Session: %s", sessionID)

		// Wait 5 seconds (idle).
		t.Log("Waiting 5s idle...")
		time.Sleep(5 * time.Second)

		// Send a message - should still work.
		err := c.SendMessage(ctx, sessionID, "Respond with exactly: ALIVE", nil)
		if err != nil {
			t.Fatalf("SendMessage after idle: %v", err)
		}

		// Wait for done.
		deadline := time.After(30 * time.Second)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatal("events channel closed")
				}
				if ev.Type == jcodeproto.EventDone {
					t.Log("Got done event after idle period - connection healthy")
					return
				}
			case <-deadline:
				t.Fatal("Timed out waiting for response after idle")
			}
		}
	})
}

// uniqueTypes deduplicates a slice of strings.
func uniqueTypes(types []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, t := range types {
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	return result
}
