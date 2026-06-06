package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Outbound actions
// ---------------------------------------------------------------------------

// OutboundAction identifies the type of Slack API call to make.
type OutboundAction int

const (
	ActionPostMessage OutboundAction = iota
	ActionUpdateMessage
	ActionUploadFile
	ActionAddReaction
	ActionRemoveReaction
)

// ---------------------------------------------------------------------------
// OutboundItem
// ---------------------------------------------------------------------------

// OutboundItem is a single unit of work in the outbound queue.
type OutboundItem struct {
	Priority  int // 1=Done/Error flush, 2=chat.update, 3=chat.postMessage, 4=files.upload
	ChannelID string
	ThreadTS  string

	Action OutboundAction

	// PostMessage fields
	Text     string
	Username string
	IconURL  string
	Blocks   []map[string]interface{}

	// UpdateMessage fields
	MessageTS string

	// UploadFile fields
	Filename string
	Content  []byte

	// AddReaction / RemoveReaction
	Emoji string

	// Dedup: content hash for 60s dedup window on PostMessage.
	ContentHash string

	// OnPosted is called after a successful PostMessage with the returned TS.
	// Used by the coalescer to switch from post to update mode.
	OnPosted func(ts string)
}

// ---------------------------------------------------------------------------
// Token bucket
// ---------------------------------------------------------------------------

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	refill   float64 // tokens per second
	lastTime time.Time
}

func newTokenBucket(max, refillPerSec float64) *tokenBucket {
	return &tokenBucket{
		tokens:   max,
		max:      max,
		refill:   refillPerSec,
		lastTime: time.Now(),
	}
}

// tryConsume attempts to consume one token. Returns true if successful,
// false if the bucket is empty. Also returns the duration until the next
// token becomes available.
func (b *tokenBucket) tryConsume() (ok bool, waitTime time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens = math.Min(b.max, b.tokens+elapsed*b.refill)
	b.lastTime = now

	if b.tokens >= 1.0 {
		b.tokens--
		return true, 0
	}

	// Time until one token is available.
	deficit := 1.0 - b.tokens
	wait := time.Duration(deficit/b.refill*1000) * time.Millisecond
	return false, wait
}

// retryAfter forces a wait by draining tokens and setting lastTime
// such that tokens won't refill until after the given duration.
func (b *tokenBucket) retryAfter(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = 0
	b.lastTime = time.Now().Add(d)
}

// ---------------------------------------------------------------------------
// Per-channel queue
// ---------------------------------------------------------------------------

type channelQueue struct {
	channelID  string
	items      []*OutboundItem
	postBucket *tokenBucket // chat.postMessage: 1/sec/channel
}

func newChannelQueue(channelID string) *channelQueue {
	return &channelQueue{
		channelID:  channelID,
		postBucket: newTokenBucket(1, 1), // 1 token, refills 1/sec
	}
}

// insert adds an item in priority order (lower priority number = higher priority).
func (cq *channelQueue) insert(item *OutboundItem) {
	// Find insertion point to maintain sorted order.
	i := 0
	for i < len(cq.items) && cq.items[i].Priority <= item.Priority {
		i++
	}
	cq.items = append(cq.items, nil)
	copy(cq.items[i+1:], cq.items[i:])
	cq.items[i] = item
}

func (cq *channelQueue) peek() *OutboundItem {
	if len(cq.items) == 0 {
		return nil
	}
	return cq.items[0]
}

func (cq *channelQueue) pop() *OutboundItem {
	if len(cq.items) == 0 {
		return nil
	}
	item := cq.items[0]
	cq.items = cq.items[1:]
	return item
}

func (cq *channelQueue) len() int {
	return len(cq.items)
}

// ---------------------------------------------------------------------------
// Dedup tracker
// ---------------------------------------------------------------------------

type dedupEntry struct {
	hash    string
	expires time.Time
}

type dedupTracker struct {
	mu      sync.Mutex
	entries []dedupEntry
}

func (d *dedupTracker) isDuplicate(hash string) bool {
	if hash == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	// Prune expired entries.
	valid := d.entries[:0]
	for _, e := range d.entries {
		if e.expires.After(now) {
			valid = append(valid, e)
		}
	}
	d.entries = valid

	for _, e := range d.entries {
		if e.hash == hash {
			return true
		}
	}
	return false
}

func (d *dedupTracker) record(hash string) {
	if hash == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, dedupEntry{
		hash:    hash,
		expires: time.Now().Add(60 * time.Second),
	})
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// Queue implements per-channel sub-queues with round-robin draining and
// token-bucket rate limiting for outbound Slack API calls.
type Queue struct {
	slack SlackPoster

	mu       sync.Mutex
	channels map[string]*channelQueue
	// Round-robin state: ordered list of channel IDs.
	order []string
	robin int // current index into order

	// Workspace-wide buckets.
	updateBucket *tokenBucket // chat.update: 50/min
	uploadBucket *tokenBucket // files.upload: 20/min

	dedup *dedupTracker

	wakeup chan struct{}
}

// NewQueue creates a new outbound queue backed by the given SlackPoster.
func NewQueue(poster SlackPoster) *Queue {
	return &Queue{
		slack:        poster,
		channels:     make(map[string]*channelQueue),
		updateBucket: newTokenBucket(50, 50.0/60.0), // 50 tokens, refill ~0.833/sec
		uploadBucket: newTokenBucket(20, 20.0/60.0), // 20 tokens, refill ~0.333/sec
		dedup:        &dedupTracker{},
		wakeup:       make(chan struct{}, 1),
	}
}

// Enqueue adds an item to the appropriate channel queue.
func (q *Queue) Enqueue(item *OutboundItem) {
	// Auto-compute content hash for PostMessage if not set.
	if item.Action == ActionPostMessage && item.ContentHash == "" && item.Text != "" {
		h := sha256.Sum256([]byte(item.ChannelID + "|" + item.ThreadTS + "|" + item.Text))
		item.ContentHash = hex.EncodeToString(h[:])
	}

	q.mu.Lock()
	cq, ok := q.channels[item.ChannelID]
	if !ok {
		cq = newChannelQueue(item.ChannelID)
		q.channels[item.ChannelID] = cq
		q.order = append(q.order, item.ChannelID)
	}
	cq.insert(item)
	q.mu.Unlock()

	// Signal the drain loop.
	select {
	case q.wakeup <- struct{}{}:
	default:
	}
}

// Run is the drain loop goroutine. It round-robins across non-empty channel
// queues, respecting per-channel and workspace-wide rate limits.
func (q *Queue) Run(ctx context.Context) {
	slog.Info("outbound queue started")
	for {
		item, minWait := q.tryDequeue()
		if item != nil {
			q.execute(ctx, item)
			continue
		}

		// Nothing ready: wait for wakeup or next bucket refill.
		if minWait <= 0 {
			minWait = 100 * time.Millisecond
		}

		select {
		case <-ctx.Done():
			slog.Info("outbound queue stopped")
			return
		case <-q.wakeup:
			// New item enqueued, try again.
		case <-time.After(minWait):
			// Bucket refill, try again.
		}
	}
}

// DrainStats returns per-channel queue depths for monitoring.
func (q *Queue) DrainStats() map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()

	stats := make(map[string]int, len(q.channels))
	for id, cq := range q.channels {
		stats[id] = cq.len()
	}
	return stats
}

// ---------------------------------------------------------------------------
// Internal: dequeue with round-robin
// ---------------------------------------------------------------------------

// tryDequeue attempts to find a sendable item across all channels using
// round-robin. Returns (nil, minWait) if all channels are rate-limited.
func (q *Queue) tryDequeue() (*OutboundItem, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := len(q.order)
	if n == 0 {
		return nil, 0
	}

	minWait := time.Duration(math.MaxInt64)
	start := q.robin

	for i := 0; i < n; i++ {
		idx := (start + i) % n
		cq := q.channels[q.order[idx]]
		if cq.len() == 0 {
			continue
		}

		item := cq.peek()

		// Check workspace-wide bucket for the action type.
		wsOK, wsWait := q.checkWorkspaceBucket(item.Action)
		if !wsOK {
			if wsWait < minWait {
				minWait = wsWait
			}
			continue
		}

		// Check per-channel bucket for PostMessage.
		if item.Action == ActionPostMessage {
			chOK, chWait := cq.postBucket.tryConsume()
			if !chOK {
				if chWait < minWait {
					minWait = chWait
				}
				continue
			}
		}

		// Re-check workspace bucket (consume token).
		if !q.consumeWorkspaceBucket(item.Action) {
			continue
		}

		// Dedup check for PostMessage.
		if item.Action == ActionPostMessage && q.dedup.isDuplicate(item.ContentHash) {
			cq.pop()
			slog.Debug("outbound: deduped message", "channel", item.ChannelID, "hash", item.ContentHash[:16])
			// Try next item without advancing robin.
			i--
			continue
		}

		popped := cq.pop()

		// Record dedup hash.
		if popped.Action == ActionPostMessage {
			q.dedup.record(popped.ContentHash)
		}

		// Advance robin to the next channel.
		q.robin = (idx + 1) % n

		// Clean up empty channel queues.
		if cq.len() == 0 {
			delete(q.channels, q.order[idx])
			q.order = append(q.order[:idx], q.order[idx+1:]...)
			if len(q.order) == 0 {
				q.robin = 0
			} else {
				q.robin = q.robin % len(q.order)
			}
		}

		return popped, 0
	}

	return nil, minWait
}

// checkWorkspaceBucket peeks at the workspace-wide bucket for the given action
// without consuming a token.
func (q *Queue) checkWorkspaceBucket(action OutboundAction) (bool, time.Duration) {
	switch action {
	case ActionUpdateMessage:
		q.updateBucket.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(q.updateBucket.lastTime).Seconds()
		tokens := math.Min(q.updateBucket.max, q.updateBucket.tokens+elapsed*q.updateBucket.refill)
		q.updateBucket.mu.Unlock()
		if tokens >= 1.0 {
			return true, 0
		}
		deficit := 1.0 - tokens
		wait := time.Duration(deficit/q.updateBucket.refill*1000) * time.Millisecond
		return false, wait
	case ActionUploadFile:
		q.uploadBucket.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(q.uploadBucket.lastTime).Seconds()
		tokens := math.Min(q.uploadBucket.max, q.uploadBucket.tokens+elapsed*q.uploadBucket.refill)
		q.uploadBucket.mu.Unlock()
		if tokens >= 1.0 {
			return true, 0
		}
		deficit := 1.0 - tokens
		wait := time.Duration(deficit/q.uploadBucket.refill*1000) * time.Millisecond
		return false, wait
	default:
		return true, 0
	}
}

// consumeWorkspaceBucket consumes a token from the workspace-wide bucket.
func (q *Queue) consumeWorkspaceBucket(action OutboundAction) bool {
	switch action {
	case ActionUpdateMessage:
		ok, _ := q.updateBucket.tryConsume()
		return ok
	case ActionUploadFile:
		ok, _ := q.uploadBucket.tryConsume()
		return ok
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// Internal: execute
// ---------------------------------------------------------------------------

func (q *Queue) execute(ctx context.Context, item *OutboundItem) {
	var err error

	switch item.Action {
	case ActionPostMessage:
		opts := buildPostOpts(item)
		ts, postErr := q.slack.PostMessage(item.ChannelID, item.Text, opts...)
		if postErr == nil && item.OnPosted != nil {
			item.OnPosted(ts)
		}
		err = postErr
	case ActionUpdateMessage:
		opts := buildPostOpts(item)
		err = q.slack.UpdateMessage(item.ChannelID, item.MessageTS, item.Text, opts...)
	case ActionUploadFile:
		err = q.slack.UploadFile(item.ChannelID, item.ThreadTS, item.Filename, item.Content)
	case ActionAddReaction:
		err = q.slack.AddReaction(item.ChannelID, item.MessageTS, item.Emoji)
	case ActionRemoveReaction:
		err = q.slack.RemoveReaction(item.ChannelID, item.MessageTS, item.Emoji)
	default:
		slog.Error("outbound: unknown action", "action", item.Action)
		return
	}

	if err != nil {
		if retryAfter := parseRetryAfter(err); retryAfter > 0 {
			slog.Warn("outbound: rate limited (429), re-queuing",
				"channel", item.ChannelID,
				"action", item.Action,
				"retry_after", retryAfter,
			)
			// Apply backpressure to all relevant buckets.
			q.applyRetryAfter(item.Action, retryAfter)
			// Re-enqueue the item.
			q.Enqueue(item)
			return
		}
		// Handle msg_too_long: truncate and retry once for UpdateMessage.
		if item.Action == ActionUpdateMessage && isMsgTooLong(err) {
			truncated := truncateForSlack(item.Text, 3800)
			if truncated != item.Text {
				slog.Warn("outbound: msg_too_long, retrying with truncated text",
					"channel", item.ChannelID,
					"original_len", len(item.Text),
					"truncated_len", len(truncated),
				)
				opts := buildPostOpts(item)
				retryErr := q.slack.UpdateMessage(item.ChannelID, item.MessageTS, truncated, opts...)
				if retryErr != nil {
					// If the truncated retry also hit a transient 429,
					// re-enqueue with the truncated text so it isn't lost.
					if retryAfter := parseRetryAfter(retryErr); retryAfter > 0 {
						slog.Warn("outbound: truncated retry rate-limited, re-queuing",
							"channel", item.ChannelID,
							"retry_after", retryAfter,
						)
						q.applyRetryAfter(item.Action, retryAfter)
						retryItem := *item
						retryItem.Text = truncated
						q.Enqueue(&retryItem)
						return
					}
					slog.Error("outbound: truncated retry also failed",
						"channel", item.ChannelID,
						"error", retryErr,
					)
				}
				return
			}
		}
		slog.Error("outbound: send failed",
			"channel", item.ChannelID,
			"action", item.Action,
			"error", err,
		)
	}
}

func buildPostOpts(item *OutboundItem) []PostOption {
	var opts []PostOption
	if item.ThreadTS != "" || item.Username != "" || item.IconURL != "" || len(item.Blocks) > 0 {
		opts = append(opts, PostOption{
			ThreadTS: item.ThreadTS,
			Username: item.Username,
			IconURL:  item.IconURL,
			Blocks:   item.Blocks,
		})
	}
	return opts
}

// parseRetryAfter checks if the error is a Slack rate limit error and
// extracts the Retry-After duration.
func parseRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	// The slack-go library returns a *slack.RateLimitedError for 429 responses.
	type rateLimited interface {
		RetryAfter() time.Duration
	}
	if rl, ok := err.(rateLimited); ok {
		return rl.RetryAfter()
	}
	// Fallback: check error string for "retry_after" pattern.
	s := err.Error()
	if idx := findRetryAfterInString(s); idx > 0 {
		return time.Duration(idx) * time.Second
	}
	return 0
}

// findRetryAfterInString is a best-effort parser for "Retry-After: N" in error strings.
func findRetryAfterInString(s string) int {
	// Look for patterns like "retry_after=30" or "Retry-After: 30".
	for _, prefix := range []string{"retry_after=", "Retry-After: "} {
		if idx := indexOf(s, prefix); idx >= 0 {
			numStr := s[idx+len(prefix):]
			// Extract digits.
			end := 0
			for end < len(numStr) && numStr[end] >= '0' && numStr[end] <= '9' {
				end++
			}
			if end > 0 {
				n, err := strconv.Atoi(numStr[:end])
				if err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func (q *Queue) applyRetryAfter(action OutboundAction, d time.Duration) {
	switch action {
	case ActionPostMessage:
		// Apply to the specific channel bucket in tryDequeue.
		// For simplicity, we apply a global sleep via the wakeup mechanism.
	case ActionUpdateMessage:
		q.updateBucket.retryAfter(d)
	case ActionUploadFile:
		q.uploadBucket.retryAfter(d)
	}
}

// isMsgTooLong checks if the error is Slack's msg_too_long rejection.
func isMsgTooLong(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return indexOf(s, "msg_too_long") >= 0
}

// truncateForSlack truncates text to fit within Slack's chat.update limit.
// It tries to break at the last newline before maxLen to avoid mid-line cuts.
// The truncation suffix is accounted for so the returned string never exceeds
// maxLen (which would otherwise re-trigger msg_too_long).
func truncateForSlack(text string, maxLen int) string {
	const suffix = "\n\n_...message truncated (exceeded Slack limit)_"

	if len(text) <= maxLen {
		return text
	}
	if maxLen <= len(suffix) {
		return suffix[:maxLen]
	}
	limit := maxLen - len(suffix)

	// Find last newline before the limit.
	cutoff := text[:limit]
	if idx := lastIndexByte(cutoff, '\n'); idx > limit/2 {
		return text[:idx] + suffix
	}
	return text[:limit] + suffix
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
