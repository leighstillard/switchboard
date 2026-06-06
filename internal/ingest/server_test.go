package ingest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestWebhookGitHub_ValidSignature(t *testing.T) {
	st := testStore(t)
	secret := "test-github-secret"

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"github": {
				Secret:          secret,
				SignatureHeader: "X-Hub-Signature-256",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"action":"opened","pull_request":{"number":42}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("X-GitHub-Delivery", "delivery-123")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusAccepted, w.Body.String())
	}
}

func TestWebhookGitHub_InvalidSignature(t *testing.T) {
	st := testStore(t)

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"github": {
				Secret:          "real-secret",
				SignatureHeader: "X-Hub-Signature-256",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"action":"opened"}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	req.Header.Set("X-GitHub-Delivery", "delivery-456")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestWebhookSwitchboard_ValidSignature(t *testing.T) {
	st := testStore(t)
	secret := "temporal-secret"

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"temporal": {
				Secret:          secret,
				SignatureHeader: "X-Switchboard-Signature",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"workflow_id":"wf-123","event_type":"started"}`)
	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())

	message := []byte(timestamp + "." + string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(message)
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook/temporal", bytes.NewReader(body))
	req.Header.Set("X-Switchboard-Signature", signature)
	req.Header.Set("X-Switchboard-Timestamp", timestamp)
	req.Header.Set("X-Switchboard-Idempotency-Key", "idem-789")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusAccepted, w.Body.String())
	}
}

func TestWebhookDeduplication(t *testing.T) {
	st := testStore(t)

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"github": {
				Secret:          "",
				SignatureHeader: "X-Hub-Signature-256",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"action":"closed"}`)

	// Send twice with same delivery ID.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", "same-delivery-id")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Errorf("attempt %d: status = %d, want %d", i, w.Code, http.StatusAccepted)
		}
	}
}

func TestWebhookPayloadTooLarge(t *testing.T) {
	st := testStore(t)

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1, // 1KB limit
		Sources: map[string]config.SourceConfig{
			"github": {Secret: ""},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := make([]byte, 2048) // 2KB
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "large-payload")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestCorrelate_Unauthenticated(t *testing.T) {
	st := testStore(t)

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"api": {
				Secret:          "correlate-secret-abcdef",
				SignatureHeader: "X-Switchboard-Signature",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"source":"temporal","external_key":"wf-1","channel_id":"C1","thread_ts":"1.1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/correlate", bytes.NewReader(body))
	// No signature headers -> must be rejected.

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestCorrelate_AuthenticatedSucceeds(t *testing.T) {
	st := testStore(t)
	secret := "correlate-secret-abcdef"

	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"api": {
				Secret:          secret,
				SignatureHeader: "X-Switchboard-Signature",
			},
		},
	}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	body := []byte(`{"source":"temporal","external_key":"wf-1","channel_id":"C1","thread_ts":"1.1"}`)
	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())

	message := []byte(timestamp + "." + string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(message)
	signature := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/correlate", bytes.NewReader(body))
	req.Header.Set("X-Switchboard-Signature", signature)
	req.Header.Set("X-Switchboard-Timestamp", timestamp)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}
}

func TestHealthEndpoint(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{ListenAddr: "127.0.0.1:0"}

	srv := NewServer(cfg, st)
	handler := srv.srv.Handler

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHeaderScrubbing(t *testing.T) {
	h := http.Header{
		"Content-Type":    {"application/json"},
		"Authorization":   {"Bearer secret-token"},
		"X-Custom-Secret": {"hidden"},
		"X-Normal-Header": {"visible"},
	}

	scrubbed := scrubHeaders(h)

	if scrubbed.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be preserved")
	}
	if scrubbed.Get("Authorization") != "[REDACTED]" {
		t.Errorf("Authorization = %q, want [REDACTED]", scrubbed.Get("Authorization"))
	}
	if scrubbed.Get("X-Custom-Secret") != "[REDACTED]" {
		t.Errorf("X-Custom-Secret = %q, want [REDACTED]", scrubbed.Get("X-Custom-Secret"))
	}
	if scrubbed.Get("X-Normal-Header") != "visible" {
		t.Error("X-Normal-Header should be preserved")
	}
}
