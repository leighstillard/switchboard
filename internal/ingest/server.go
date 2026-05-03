// Package ingest provides a webhook ingestion HTTP server that receives
// events from external sources (GitHub, Temporal, cron, etc.) and verifies
// their HMAC signatures.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/format5/switchboard/internal/config"
)

// Event represents an ingested webhook event.
type Event struct {
	Source    string
	EventType string
	Payload   []byte
	Headers   http.Header
	Timestamp time.Time
}

// Server is the webhook ingestion HTTP server.
type Server struct {
	cfg     config.IngestConfig
	srv     *http.Server
	eventCh chan Event
}

// NewServer creates a new ingest server.
func NewServer(cfg config.IngestConfig) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		eventCh: make(chan Event, 256),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/{source}", s.handleWebhook)
	mux.HandleFunc("/health", s.handleHealth)

	s.srv = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxHeaderBytes: cfg.MaxBodyKB * 1024,
	}

	return s, nil
}

// Run starts the HTTP server. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
	}()

	slog.Info("ingest server listening", "addr", s.cfg.ListenAddr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("ingest server error", "error", err)
	}
}

// Events returns the channel of ingested events for the router to consume.
func (s *Server) Events() <-chan Event {
	return s.eventCh
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if source == "" {
		http.Error(w, "missing source", http.StatusBadRequest)
		return
	}

	srcCfg, ok := s.cfg.Sources[source]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown source: %s", source), http.StatusNotFound)
		return
	}

	// TODO: verify HMAC signature using srcCfg.Secret and srcCfg.SignatureHeader
	_ = srcCfg

	// TODO: read body (respecting MaxBodyKB), parse event, push to eventCh
	slog.Debug("webhook received", "source", source)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
