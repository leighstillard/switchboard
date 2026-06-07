//go:build claude_smoke

// Package-level smoke tests that drive the REAL claude CLI under the operator's
// subscription. Excluded from the default test pass; run before tagging a
// release:  go test -tags claude_smoke ./internal/agent/claude/
package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// collectTurn reads events until a terminal event, returning the concatenated
// assistant text and whether the turn ended cleanly.
func collectTurn(t *testing.T, events <-chan agent.Event, timeout time.Duration) (string, bool) {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event channel closed mid-turn")
			}
			switch ev.Type {
			case agent.EventTextDelta:
				sb.WriteString(ev.Text)
			case agent.EventTurnDone:
				return sb.String(), true
			case agent.EventTurnError, agent.EventInterrupted:
				return sb.String(), false
			}
		case <-deadline:
			t.Fatalf("timed out waiting for turn end; partial text: %q", sb.String())
		}
	}
}

func requireSubscription(t *testing.T) {
	t.Helper()
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Skip("ANTHROPIC_API_KEY is set; this test must prove subscription OAuth, not API-key billing")
	}
}

// TestSubscriptionMultiTurnAndHandover is the load-bearing merge gate: it proves
// the persistent process keeps context across turns under subscription OAuth
// (the empty-response bug is gone), and that a restart/handover resumes the SAME
// conversation via --resume.
func TestSubscriptionMultiTurnAndHandover(t *testing.T) {
	requireSubscription(t)

	wd := t.TempDir()
	ctx := context.Background()

	b := New(DefaultConfig())
	defer b.Close()

	id, events, err := b.Subscribe(ctx, wd)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Logf("session id: %s", id)

	// Turn 1: establish context.
	if err := b.SendMessage(ctx, id, "Remember the number 42. Reply with just: OK", nil); err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	text1, ok := collectTurn(t, events, 90*time.Second)
	if !ok {
		t.Fatalf("turn 1 did not complete cleanly")
	}
	if strings.TrimSpace(text1) == "" {
		t.Fatal("turn 1 produced EMPTY text — OAuth/empty-response regression")
	}

	// Turn 2: prove context carried across turns in the same process.
	if err := b.SendMessage(ctx, id, "What number did I ask you to remember? Reply with just the number.", nil); err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	text2, ok := collectTurn(t, events, 90*time.Second)
	if !ok {
		t.Fatalf("turn 2 did not complete cleanly")
	}
	if !strings.Contains(text2, "42") {
		t.Fatalf("turn 2 lost multi-turn context; want 42, got %q", text2)
	}

	// Handover/restart: a fresh backend resumes the SAME session via --resume.
	b2 := New(DefaultConfig())
	defer b2.Close()
	events2, err := b2.SubscribeExisting(ctx, id, wd)
	if err != nil {
		t.Fatalf("SubscribeExisting: %v", err)
	}
	if err := b2.SendMessage(ctx, id, "What number again? Reply with just the number.", nil); err != nil {
		t.Fatalf("SendMessage 3 (resumed): %v", err)
	}
	text3, ok := collectTurn(t, events2, 90*time.Second)
	if !ok {
		t.Fatalf("resumed turn did not complete cleanly")
	}
	if !strings.Contains(text3, "42") {
		t.Fatalf("--resume lost the conversation; want 42, got %q", text3)
	}
}
