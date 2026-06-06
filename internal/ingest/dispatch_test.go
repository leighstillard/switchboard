package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/format5/switchboard/internal/config"
)

func TestDispatchEndpoint_Success(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{ListenAddr: "127.0.0.1:0", MaxBodyKB: 1024}
	srv := NewServer(cfg, st)

	var gotChannel, gotPrompt, gotUser string
	srv.SetDispatchHandler(func(ctx context.Context, channelID, prompt, userID string) (string, string, error) {
		gotChannel = channelID
		gotPrompt = prompt
		gotUser = userID
		return "1234567890.123456", "session_test_123", nil
	})

	body, _ := json.Marshal(map[string]string{
		"channel_id": "C0ABC123",
		"prompt":     "deploy to staging",
		"user_id":    "U0USER",
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if gotChannel != "C0ABC123" {
		t.Errorf("channel = %q, want C0ABC123", gotChannel)
	}
	if gotPrompt != "deploy to staging" {
		t.Errorf("prompt = %q, want 'deploy to staging'", gotPrompt)
	}
	if gotUser != "U0USER" {
		t.Errorf("user = %q, want U0USER", gotUser)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "dispatched" {
		t.Errorf("status = %q, want dispatched", resp["status"])
	}
	if resp["thread_ts"] != "1234567890.123456" {
		t.Errorf("thread_ts = %q, want 1234567890.123456", resp["thread_ts"])
	}
	if resp["session_id"] != "session_test_123" {
		t.Errorf("session_id = %q, want session_test_123", resp["session_id"])
	}
}

func TestDispatchEndpoint_NotConfigured(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{ListenAddr: "127.0.0.1:0", MaxBodyKB: 1024}
	srv := NewServer(cfg, st)
	// No dispatch handler set.

	body, _ := json.Marshal(map[string]string{
		"channel_id": "C0ABC123",
		"prompt":     "deploy",
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDispatchEndpoint_MissingFields(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{ListenAddr: "127.0.0.1:0", MaxBodyKB: 1024}
	srv := NewServer(cfg, st)
	srv.SetDispatchHandler(func(ctx context.Context, channelID, prompt, userID string) (string, string, error) {
		return "", "", nil
	})

	body, _ := json.Marshal(map[string]string{
		"channel_id": "C0ABC123",
		// no prompt
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDispatchEndpoint_UnauthorizedWhenSecretConfigured(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{
		ListenAddr: "127.0.0.1:0",
		MaxBodyKB:  1024,
		Sources: map[string]config.SourceConfig{
			"dispatch": {Secret: "0123456789abcdef0123456789abcdef"},
		},
	}
	srv := NewServer(cfg, st)
	srv.SetDispatchHandler(func(ctx context.Context, channelID, prompt, userID string) (string, string, error) {
		return "1234567890.123456", "session_test_123", nil
	})

	body, _ := json.Marshal(map[string]string{
		"channel_id": "C0ABC123",
		"prompt":     "deploy",
	})

	// No signature header => verification must fail.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/dispatch", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestDispatchEndpoint_MethodNotAllowed(t *testing.T) {
	st := testStore(t)
	cfg := config.IngestConfig{ListenAddr: "127.0.0.1:0", MaxBodyKB: 1024}
	srv := NewServer(cfg, st)
	srv.SetDispatchHandler(func(ctx context.Context, channelID, prompt, userID string) (string, string, error) {
		return "", "", nil
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dispatch", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
