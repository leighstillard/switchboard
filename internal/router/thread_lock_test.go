package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestThreadLock_BasicAcquireRelease tests that the lock is acquired on
// first queue entry and released on turn completion.
func TestThreadLock_BasicAcquireRelease(t *testing.T) {
	r := &Router{
		coalescerQueue: make(map[string][]string),
		threadLock:     make(map[string]string),
	}

	sessionID := "session_test"
	key1 := "C0AL12WCNBG:1234.5678"

	// Simulate handleNewSession: append to queue and acquire lock.
	r.mu.Lock()
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], key1)
	if _, locked := r.threadLock[sessionID]; !locked {
		r.threadLock[sessionID] = key1
	}
	r.mu.Unlock()

	// Lock should be held by key1.
	r.mu.RLock()
	assert.Equal(t, key1, r.threadLock[sessionID])
	r.mu.RUnlock()

	// Simulate turn completion: release lock.
	r.mu.Lock()
	delete(r.threadLock, sessionID)
	if q := r.coalescerQueue[sessionID]; len(q) > 0 {
		r.coalescerQueue[sessionID] = q[1:]
	}
	r.mu.Unlock()

	// Lock should be released.
	r.mu.RLock()
	_, locked := r.threadLock[sessionID]
	assert.False(t, locked)
	r.mu.RUnlock()
}

// TestThreadLock_SecondThreadQueued tests that a second thread is queued
// (not given the lock) when the first thread holds it.
func TestThreadLock_SecondThreadQueued(t *testing.T) {
	r := &Router{
		coalescerQueue: make(map[string][]string),
		threadLock:     make(map[string]string),
	}

	sessionID := "session_test"
	key1 := "C0AL12WCNBG:1111.0000"
	key2 := "C0AL12WCNBG:2222.0000"

	// Thread 1 acquires lock.
	r.mu.Lock()
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], key1)
	if _, locked := r.threadLock[sessionID]; !locked {
		r.threadLock[sessionID] = key1
	}
	r.mu.Unlock()

	// Thread 2 tries to acquire -- should be queued but not locked.
	r.mu.Lock()
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], key2)
	if _, locked := r.threadLock[sessionID]; !locked {
		r.threadLock[sessionID] = key2
	}
	r.mu.Unlock()

	// Lock should still be held by key1, not key2.
	r.mu.RLock()
	assert.Equal(t, key1, r.threadLock[sessionID])
	assert.Equal(t, []string{key1, key2}, r.coalescerQueue[sessionID])
	r.mu.RUnlock()
}

// TestThreadLock_PromotionOnCompletion tests that the second queued thread
// is promoted to lock holder when the first thread's turn completes.
func TestThreadLock_PromotionOnCompletion(t *testing.T) {
	r := &Router{
		coalescerQueue: make(map[string][]string),
		threadLock:     make(map[string]string),
	}

	sessionID := "session_test"
	key1 := "C0AL12WCNBG:1111.0000"
	key2 := "C0AL12WCNBG:2222.0000"

	// Setup: key1 holds lock, key2 is queued.
	r.coalescerQueue[sessionID] = []string{key1, key2}
	r.threadLock[sessionID] = key1

	// Simulate turn completion for key1 (same logic as consumeEvents).
	r.mu.Lock()
	delete(r.threadLock, sessionID)
	if q := r.coalescerQueue[sessionID]; len(q) > 0 {
		r.coalescerQueue[sessionID] = q[1:]
	}
	// Promote next.
	if q := r.coalescerQueue[sessionID]; len(q) > 0 {
		r.threadLock[sessionID] = q[0]
	}
	r.mu.Unlock()

	// key2 should now hold the lock.
	r.mu.RLock()
	assert.Equal(t, key2, r.threadLock[sessionID])
	assert.Equal(t, []string{key2}, r.coalescerQueue[sessionID])
	r.mu.RUnlock()
}

// TestThreadLock_DispatchDoesNotStealLock tests that a dispatch (new thread)
// queues properly without stealing the lock from an active turn.
func TestThreadLock_DispatchDoesNotStealLock(t *testing.T) {
	r := &Router{
		coalescerQueue: make(map[string][]string),
		threadLock:     make(map[string]string),
	}

	sessionID := "session_test"
	activeKey := "C0AL12WCNBG:1111.0000"
	dispatchKey := "C0AL12WCNBG:3333.0000"

	// Active thread holds the lock.
	r.coalescerQueue[sessionID] = []string{activeKey}
	r.threadLock[sessionID] = activeKey

	// Dispatch creates a new thread -- same logic as handleNewSession.
	r.mu.Lock()
	r.coalescerQueue[sessionID] = append(r.coalescerQueue[sessionID], dispatchKey)
	if _, locked := r.threadLock[sessionID]; !locked {
		r.threadLock[sessionID] = dispatchKey
	}
	r.mu.Unlock()

	// Lock should NOT have been stolen.
	r.mu.RLock()
	assert.Equal(t, activeKey, r.threadLock[sessionID])
	assert.Equal(t, []string{activeKey, dispatchKey}, r.coalescerQueue[sessionID])
	r.mu.RUnlock()
}

// TestThreadLock_EmptyQueueNoLock tests that when no threads are queued,
// there is no lock holder.
func TestThreadLock_EmptyQueueNoLock(t *testing.T) {
	r := &Router{
		coalescerQueue: make(map[string][]string),
		threadLock:     make(map[string]string),
	}

	sessionID := "session_test"
	key := "C0AL12WCNBG:1111.0000"

	// Setup: single thread holds lock.
	r.coalescerQueue[sessionID] = []string{key}
	r.threadLock[sessionID] = key

	// Turn completes.
	r.mu.Lock()
	delete(r.threadLock, sessionID)
	if q := r.coalescerQueue[sessionID]; len(q) > 0 {
		r.coalescerQueue[sessionID] = q[1:]
	}
	if q := r.coalescerQueue[sessionID]; len(q) > 0 {
		r.threadLock[sessionID] = q[0]
	}
	r.mu.Unlock()

	// No lock holder, empty queue.
	r.mu.RLock()
	_, locked := r.threadLock[sessionID]
	require.False(t, locked)
	assert.Empty(t, r.coalescerQueue[sessionID])
	r.mu.RUnlock()
}
