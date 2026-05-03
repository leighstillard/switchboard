// Package outbound provides a rate-limited send queue for outbound messages
// to Slack and other destinations.
package outbound

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Message represents an outbound message in the queue.
type Message struct {
	ChannelID string
	Text      string
	Identity  string
	Priority  int
}

// Queue is a rate-limited outbound message queue.
type Queue struct {
	ch       chan Message
	rate     time.Duration
	mu       sync.Mutex
	sendFn   func(ctx context.Context, msg Message) error
}

// NewQueue creates a new rate-limited outbound queue.
func NewQueue(ratePerSec int, sendFn func(ctx context.Context, msg Message) error) *Queue {
	rate := time.Second / time.Duration(ratePerSec)
	return &Queue{
		ch:     make(chan Message, 1024),
		rate:   rate,
		sendFn: sendFn,
	}
}

// Enqueue adds a message to the outbound queue.
func (q *Queue) Enqueue(msg Message) {
	q.ch <- msg
}

// Run processes the outbound queue with rate limiting. Blocks until ctx is cancelled.
func (q *Queue) Run(ctx context.Context) {
	ticker := time.NewTicker(q.rate)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-q.ch:
			<-ticker.C
			if err := q.sendFn(ctx, msg); err != nil {
				slog.Error("outbound send failed", "channel", msg.ChannelID, "error", err)
			}
		}
	}
}
