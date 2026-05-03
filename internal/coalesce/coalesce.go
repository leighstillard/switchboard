// Package coalesce implements per-session message coalescing to batch
// rapid-fire messages before sending them to jcode sessions.
package coalesce

import (
	"sync"
	"time"
)

// Coalescer batches messages for a session within a configurable window.
type Coalescer struct {
	window   time.Duration
	mu       sync.Mutex
	buffers  map[string]*buffer
	flushFn  func(sessionID string, messages []string)
}

type buffer struct {
	messages []string
	timer    *time.Timer
}

// New creates a new Coalescer with the given flush window and callback.
func New(window time.Duration, flushFn func(sessionID string, messages []string)) *Coalescer {
	return &Coalescer{
		window:  window,
		buffers: make(map[string]*buffer),
		flushFn: flushFn,
	}
}

// Add appends a message to the session's buffer, resetting the flush timer.
func (c *Coalescer) Add(sessionID, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	buf, ok := c.buffers[sessionID]
	if !ok {
		buf = &buffer{}
		c.buffers[sessionID] = buf
	}

	buf.messages = append(buf.messages, message)

	if buf.timer != nil {
		buf.timer.Stop()
	}

	buf.timer = time.AfterFunc(c.window, func() {
		c.flush(sessionID)
	})
}

func (c *Coalescer) flush(sessionID string) {
	c.mu.Lock()
	buf, ok := c.buffers[sessionID]
	if !ok {
		c.mu.Unlock()
		return
	}
	messages := buf.messages
	delete(c.buffers, sessionID)
	c.mu.Unlock()

	if len(messages) > 0 && c.flushFn != nil {
		c.flushFn(sessionID, messages)
	}
}
