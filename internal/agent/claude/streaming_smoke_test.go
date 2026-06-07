//go:build claude_smoke

package claude

import (
	"context"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/agent"
)

// TestStreamingTextAndTools characterises the real persistent-mode stream end to
// end: a text prompt yields non-empty assistant text, and a tool prompt yields
// the ToolStart→ToolExec→ToolDone lifecycle — all from full `assistant` messages
// (no stream_event deltas exist in this mode; the translator drops them anyway).
func TestStreamingTextAndTools(t *testing.T) {
	requireSubscription(t)

	ctx := context.Background()
	b := New(DefaultConfig())
	defer b.Close()

	id, events, err := b.Subscribe(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Text turn.
	if err := b.SendMessage(ctx, id, "Reply with exactly: PONG", nil); err != nil {
		t.Fatalf("SendMessage(text): %v", err)
	}
	text, ok := collectTurn(t, events, 90*time.Second)
	if !ok || text == "" {
		t.Fatalf("text turn: ok=%v text=%q (empty-response regression)", ok, text)
	}

	// Tool turn: provoke a Bash call and assert the tool lifecycle arrives.
	if err := b.SendMessage(ctx, id, "Use the Bash tool to run: echo smoketest", nil); err != nil {
		t.Fatalf("SendMessage(tool): %v", err)
	}
	var sawToolStart, sawToolDone, sawDone bool
	deadline := time.After(120 * time.Second)
loop:
	for {
		select {
		case ev, okc := <-events:
			if !okc {
				t.Fatal("channel closed mid tool turn")
			}
			switch ev.Type {
			case agent.EventToolStart:
				sawToolStart = true
			case agent.EventToolDone:
				sawToolDone = true
			case agent.EventTurnDone, agent.EventTurnError:
				sawDone = true
				break loop
			}
		case <-deadline:
			t.Fatal("timed out on tool turn")
		}
	}
	if !sawToolStart || !sawToolDone {
		t.Errorf("tool lifecycle incomplete: start=%v done=%v", sawToolStart, sawToolDone)
	}
	_ = sawDone
}
