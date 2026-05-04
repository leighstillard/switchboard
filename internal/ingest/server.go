// Package ingest implements the HTTP webhook ingest server. It receives
// webhooks from GitHub, Temporal, cron, and generic sources, verifies
// HMAC signatures, persists them durably (at-least-once delivery), and
// hands them to the router for processing.
package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/store"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// WebhookHandler is called for each successfully ingested webhook.
type WebhookHandler func(item *store.WebhookInboxItem)

// Server is the HTTP webhook ingest server.
type Server struct {
	cfg     config.IngestConfig
	store   *store.Store
	handler WebhookHandler

	srv      *http.Server
	listener net.Listener

	// Test injection: when set, POST /test/inject routes a simulated
	// Slack message through the router. Only enabled in debug mode.
	testInjectHandler func(channelID, threadTS, userID, text string) string

	// Per-source rate limiters.
	mu       sync.Mutex
	limiters map[string]*rateLimiter
}

// NewServer creates an ingest server.
func NewServer(cfg config.IngestConfig, st *store.Store) *Server {
	s := &Server{
		cfg:      cfg,
		store:    st,
		limiters: make(map[string]*rateLimiter),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", s.handleWebhook("github"))
	mux.HandleFunc("/webhook/temporal", s.handleWebhook("temporal"))
	mux.HandleFunc("/webhook/cron", s.handleWebhook("cron"))
	mux.HandleFunc("/webhook/generic", s.handleWebhookGeneric)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/test/inject", s.handleTestInject)

	s.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

// SetHandler registers the callback for processed webhooks.
func (s *Server) SetHandler(h WebhookHandler) {
	s.handler = h
}

// SetTestInjectHandler registers a callback for the test injection endpoint.
// This should only be called in debug mode.
func (s *Server) SetTestInjectHandler(h func(channelID, threadTS, userID, text string) string) {
	s.testInjectHandler = h
}

// Run starts the HTTP server and webhook worker. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	addr := s.cfg.ListenAddr
	if addr == "" {
		addr = "127.0.0.1:8765"
	}

	var err error
	s.listener, err = net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("ingest: listen: %w", err)
	}

	slog.Info("ingest: HTTP server started", "addr", addr)

	// Start the webhook worker that drains the inbox.
	go s.webhookWorker(ctx)

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal.
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
		slog.Info("ingest: HTTP server stopped")
		return nil
	case err := <-errCh:
		return fmt.Errorf("ingest: serve: %w", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) handleWebhook(source string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Rate limiting.
		if !s.checkRateLimit(source) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}

		// Read body.
		maxBody := int64(s.cfg.MaxBodyKB) * 1024
		if maxBody <= 0 {
			maxBody = 1024 * 1024 // default 1MB
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if int64(len(body)) > maxBody {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}

		// HMAC verification.
		if !s.verifySignature(source, r, body) {
			slog.Warn("ingest: HMAC verification failed", "source", source, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Compute idempotency key.
		idempotencyKey := s.extractIdempotencyKey(source, r)
		if idempotencyKey == "" {
			http.Error(w, "missing idempotency key", http.StatusBadRequest)
			return
		}

		// Scrub headers for persistence.
		scrubbedHeaders := scrubHeaders(r.Header)

		// Persist to webhook_inbox.
		item := &store.WebhookInboxItem{
			ReceivedAt:     time.Now().Unix(),
			Source:         source,
			IdempotencyKey: idempotencyKey,
			HeadersJSON:    headersToJSON(scrubbedHeaders),
			BodyBlob:       body,
			Status:         "pending",
			Attempts:       0,
		}

		inserted, err := s.store.InsertWebhook(item)
		if err != nil {
			slog.Error("ingest: persist failed", "source", source, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if !inserted {
			// Duplicate (idempotency key already exists) - that's fine.
			slog.Debug("ingest: duplicate webhook ignored", "source", source, "key", idempotencyKey)
		}

		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted"}`))
	}
}

func (s *Server) handleWebhookGeneric(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		http.Error(w, "missing source query parameter", http.StatusBadRequest)
		return
	}
	s.handleWebhook(source)(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// handleTestInject accepts simulated Slack messages for e2e testing.
// POST /test/inject with JSON body: {"channel_id", "thread_ts", "user_id", "text"}
// Only works when SetTestInjectHandler has been called (debug mode).
func (s *Server) handleTestInject(w http.ResponseWriter, r *http.Request) {
	if s.testInjectHandler == nil {
		http.Error(w, "test injection not enabled (start with --debug)", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
		UserID    string `json:"user_id"`
		Text      string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || req.Text == "" {
		http.Error(w, "channel_id and text are required", http.StatusBadRequest)
		return
	}
	if req.UserID == "" {
		req.UserID = "U_E2E_TEST"
	}

	slog.Debug("ingest: test inject", "channel", req.ChannelID, "user", req.UserID, "text_len", len(req.Text))
	messageTS := s.testInjectHandler(req.ChannelID, req.ThreadTS, req.UserID, req.Text)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "injected", "message_ts": messageTS})
}

// ---------------------------------------------------------------------------
// HMAC verification
// ---------------------------------------------------------------------------

func (s *Server) verifySignature(source string, r *http.Request, body []byte) bool {
	srcCfg, ok := s.cfg.Sources[source]
	if !ok || srcCfg.Secret == "" {
		// No secret configured: skip verification (dev mode).
		slog.Debug("ingest: no secret configured, skipping HMAC", "source", source)
		return true
	}

	sigHeader := srcCfg.SignatureHeader
	if sigHeader == "" {
		sigHeader = "X-Switchboard-Signature"
	}

	signature := r.Header.Get(sigHeader)
	if signature == "" {
		return false
	}

	// GitHub-style: "sha256=<hex>"
	if source == "github" {
		return verifyGitHubSignature(srcCfg.Secret, body, signature)
	}

	// Switchboard-style: HMAC-SHA256 of "<timestamp>.<body>"
	timestamp := r.Header.Get("X-Switchboard-Timestamp")
	if timestamp == "" {
		return false
	}

	// Check timestamp freshness (5 minute window).
	ts, err := parseTimestamp(timestamp)
	if err != nil || time.Since(ts) > 5*time.Minute || ts.After(time.Now().Add(1*time.Minute)) {
		slog.Debug("ingest: timestamp out of range", "source", source, "ts", timestamp)
		return false
	}

	message := []byte(timestamp + "." + string(body))
	mac := hmac.New(sha256.New, []byte(srcCfg.Secret))
	mac.Write(message)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

func verifyGitHubSignature(secret string, body []byte, signature string) bool {
	// GitHub sends "sha256=<hex>"
	parts := strings.SplitN(signature, "=", 2)
	if len(parts) != 2 || parts[0] != "sha256" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(parts[1]))
}

func parseTimestamp(s string) (time.Time, error) {
	// Try Unix milliseconds first, then seconds.
	if len(s) > 10 {
		// Likely milliseconds.
		var ms int64
		_, err := fmt.Sscanf(s, "%d", &ms)
		if err == nil {
			return time.UnixMilli(ms), nil
		}
	}
	var sec int64
	_, err := fmt.Sscanf(s, "%d", &sec)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

// ---------------------------------------------------------------------------
// Idempotency key extraction
// ---------------------------------------------------------------------------

func (s *Server) extractIdempotencyKey(source string, r *http.Request) string {
	switch source {
	case "github":
		// GitHub provides X-GitHub-Delivery as unique delivery ID.
		return r.Header.Get("X-GitHub-Delivery")
	case "temporal":
		// Compose from headers or body fields.
		key := r.Header.Get("X-Switchboard-Idempotency-Key")
		if key != "" {
			return key
		}
		// Fallback: use request timestamp + source.
		return fmt.Sprintf("%s-%d", source, time.Now().UnixNano())
	case "cron":
		// Required header.
		return r.Header.Get("X-Switchboard-Idempotency-Key")
	default:
		return r.Header.Get("X-Switchboard-Idempotency-Key")
	}
}

// ---------------------------------------------------------------------------
// Header scrubbing
// ---------------------------------------------------------------------------

// sensitivePattern matches headers that should be scrubbed.
var sensitiveHeaders = map[string]bool{
	"authorization": true,
	"cookie":        true,
	"set-cookie":    true,
}

func isSensitiveHeader(key string) bool {
	lower := strings.ToLower(key)
	if sensitiveHeaders[lower] {
		return true
	}
	// Match patterns: X-*-Token, X-*-Secret, X-*-Signature, *secret*, *token*, *password*, *key*
	for _, pattern := range []string{"secret", "token", "password", "key", "signature"} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func scrubHeaders(h http.Header) http.Header {
	scrubbed := make(http.Header)
	for key, values := range h {
		if isSensitiveHeader(key) {
			scrubbed.Set(key, "[REDACTED]")
		} else {
			scrubbed[key] = values
		}
	}
	return scrubbed
}

func headersToJSON(h http.Header) string {
	// Flatten multi-valued headers to single string, then marshal.
	flat := make(map[string]string, len(h))
	for key, values := range h {
		flat[key] = strings.Join(values, ", ")
	}
	data, err := json.Marshal(flat)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// Webhook worker (drains inbox)
// ---------------------------------------------------------------------------

func (s *Server) webhookWorker(ctx context.Context) {
	slog.Info("ingest: webhook worker started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("ingest: webhook worker stopped")
			return
		default:
		}

		// Try to claim a pending webhook.
		item, err := s.store.ClaimPendingWebhook("")
		if err != nil {
			slog.Error("ingest: claim webhook failed", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if item == nil {
			// No pending work; poll interval.
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
				continue
			}
		}

		// Process the webhook.
		if err := s.processWebhook(ctx, item); err != nil {
			maxAttempts := s.cfg.MaxAttempts
			if maxAttempts <= 0 {
				maxAttempts = 5
			}
			if item.Attempts >= maxAttempts {
				errMsg := err.Error()
				s.store.MarkWebhookFailed(item.ID, errMsg)
				slog.Error("ingest: webhook permanently failed",
					"id", item.ID, "source", item.Source, "err", err)
			} else {
				// Leave as processing; it'll be retried on next claim cycle.
				errMsg := err.Error()
				s.store.MarkWebhookFailed(item.ID, errMsg)
				slog.Warn("ingest: webhook processing failed, will retry",
					"id", item.ID, "source", item.Source, "attempt", item.Attempts, "err", err)
			}
			continue
		}

		s.store.MarkWebhookDone(item.ID)
	}
}

func (s *Server) processWebhook(ctx context.Context, item *store.WebhookInboxItem) error {
	if s.handler != nil {
		s.handler(item)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rate limiting (per-source)
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	refill   float64
	lastTime time.Time
}

func (s *Server) checkRateLimit(source string) bool {
	s.mu.Lock()
	rl, ok := s.limiters[source]
	if !ok {
		// Default: 100 requests/minute per source.
		rl = &rateLimiter{
			tokens:   100,
			max:      100,
			refill:   100.0 / 60.0,
			lastTime: time.Now(),
		}
		s.limiters[source] = rl
	}
	s.mu.Unlock()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.tokens = min(rl.max, rl.tokens+elapsed*rl.refill)
	rl.lastTime = now

	if rl.tokens >= 1.0 {
		rl.tokens--
		return true
	}
	return false
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
