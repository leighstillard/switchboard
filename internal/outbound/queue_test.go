package outbound

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type mockPoster struct {
	mu      sync.Mutex
	posts   []mockPost
	updates []mockUpdate
	uploads []mockUpload
}

type mockPost struct {
	channelID string
	text      string
	opts      []PostOption
}

type mockUpdate struct {
	channelID string
	ts        string
	text      string
}

type mockUpload struct {
	channelID string
	threadTS  string
	filename  string
}

func (m *mockPoster) PostMessage(channelID, text string, opts ...PostOption) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, mockPost{channelID, text, opts})
	return "new-ts-" + channelID, nil
}

func (m *mockPoster) UpdateMessage(channelID, ts, text string, opts ...PostOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, mockUpdate{channelID, ts, text})
	return nil
}

func (m *mockPoster) UploadFile(channelID, threadTS, filename string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads = append(m.uploads, mockUpload{channelID, threadTS, filename})
	return nil
}

func (m *mockPoster) AddReaction(channelID, ts, emoji string) error {
	return nil
}

func (m *mockPoster) RemoveReaction(channelID, ts, emoji string) error {
	return nil
}

func TestQueueBasicPost(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(&OutboundItem{
		Priority:  3,
		ChannelID: "C001",
		Action:    ActionPostMessage,
		Text:      "hello from queue",
	})

	// Wait for drain.
	time.Sleep(200 * time.Millisecond)

	poster.mu.Lock()
	defer poster.mu.Unlock()
	if len(poster.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(poster.posts))
	}
	if poster.posts[0].text != "hello from queue" {
		t.Errorf("text = %q", poster.posts[0].text)
	}
}

func TestQueueUpdate(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(&OutboundItem{
		Priority:  2,
		ChannelID: "C001",
		Action:    ActionUpdateMessage,
		MessageTS: "orig-ts",
		Text:      "updated text",
	})

	time.Sleep(200 * time.Millisecond)

	poster.mu.Lock()
	defer poster.mu.Unlock()
	if len(poster.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(poster.updates))
	}
	if poster.updates[0].ts != "orig-ts" {
		t.Errorf("ts = %q", poster.updates[0].ts)
	}
}

func TestQueueRoundRobin(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	// Enqueue to multiple channels.
	for i := 0; i < 3; i++ {
		q.Enqueue(&OutboundItem{
			Priority:  3,
			ChannelID: "C_A",
			Action:    ActionPostMessage,
			Text:      "msg-A",
		})
		q.Enqueue(&OutboundItem{
			Priority:  3,
			ChannelID: "C_B",
			Action:    ActionPostMessage,
			Text:      "msg-B",
		})
	}

	// Wait for processing.
	time.Sleep(500 * time.Millisecond)

	poster.mu.Lock()
	defer poster.mu.Unlock()

	// Should have sent from both channels (round-robin fairness).
	aCount, bCount := 0, 0
	for _, p := range poster.posts {
		switch p.channelID {
		case "C_A":
			aCount++
		case "C_B":
			bCount++
		}
	}

	if aCount == 0 || bCount == 0 {
		t.Errorf("expected messages from both channels: A=%d, B=%d", aCount, bCount)
	}
}

func TestQueuePriorityOrdering(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	// Enqueue items with different priorities (don't start drain yet).
	// Use different text to avoid dedup.
	q.Enqueue(&OutboundItem{
		Priority:  4,
		ChannelID: "C001",
		Action:    ActionPostMessage,
		Text:      "low-priority message here",
	})
	q.Enqueue(&OutboundItem{
		Priority:  1,
		ChannelID: "C001",
		Action:    ActionPostMessage,
		Text:      "high-priority message here",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	time.Sleep(1200 * time.Millisecond)

	poster.mu.Lock()
	defer poster.mu.Unlock()

	if len(poster.posts) < 2 {
		t.Fatalf("posts = %d, want >= 2", len(poster.posts))
	}
	// High priority should come first.
	if poster.posts[0].text != "high-priority message here" {
		t.Errorf("first post = %q, want high-priority message here", poster.posts[0].text)
	}
}

func TestQueueContentDedup(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	// Send the same message twice rapidly.
	for i := 0; i < 2; i++ {
		q.Enqueue(&OutboundItem{
			Priority:  3,
			ChannelID: "C001",
			ThreadTS:  "thread-1",
			Action:    ActionPostMessage,
			Text:      "duplicate message",
		})
	}

	time.Sleep(300 * time.Millisecond)

	poster.mu.Lock()
	defer poster.mu.Unlock()

	// Should only send one (dedup within 60s window).
	if len(poster.posts) != 1 {
		t.Errorf("posts = %d, want 1 (dedup)", len(poster.posts))
	}
}

func TestDrainStats(t *testing.T) {
	poster := &mockPoster{}
	q := NewQueue(poster)

	q.Enqueue(&OutboundItem{Priority: 3, ChannelID: "C_X", Action: ActionPostMessage, Text: "a"})
	q.Enqueue(&OutboundItem{Priority: 3, ChannelID: "C_X", Action: ActionPostMessage, Text: "b"})
	q.Enqueue(&OutboundItem{Priority: 3, ChannelID: "C_Y", Action: ActionPostMessage, Text: "c"})

	stats := q.DrainStats()
	if stats["C_X"] != 2 {
		t.Errorf("C_X depth = %d, want 2", stats["C_X"])
	}
	if stats["C_Y"] != 1 {
		t.Errorf("C_Y depth = %d, want 1", stats["C_Y"])
	}
}

func TestTruncateForSlack(t *testing.T) {
	// Short text: no truncation.
	short := "Hello world"
	if got := truncateForSlack(short, 100); got != short {
		t.Errorf("truncateForSlack(%q, 100) = %q, want unchanged", short, got)
	}

	// Long text: truncate at newline.
	var lines string
	for i := 0; i < 100; i++ {
		lines += "This is line number something or other.\n"
	}
	got := truncateForSlack(lines, 200)
	if len(got) > 200 { // must not exceed the requested max (incl. suffix)
		t.Errorf("truncated text too long: %d chars, want <= 200", len(got))
	}
	if !containsSubstring(got, "truncated") {
		t.Error("truncated text should contain truncation notice")
	}
}

func TestIsMsgTooLong(t *testing.T) {
	if isMsgTooLong(nil) {
		t.Error("nil error should not be msg_too_long")
	}
	if !isMsgTooLong(fmt.Errorf("slack: update message: msg_too_long")) {
		t.Error("msg_too_long error should match")
	}
	if isMsgTooLong(fmt.Errorf("slack: rate limited")) {
		t.Error("rate limited error should not match msg_too_long")
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}
