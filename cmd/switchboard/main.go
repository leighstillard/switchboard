// Switchboard bridges Slack channels to jcode agent sessions with webhook
// ingestion, message coalescing, and intelligent routing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/ingest"
	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/outbound"
	"github.com/format5/switchboard/internal/router"
	"github.com/format5/switchboard/internal/slack"
	"github.com/format5/switchboard/internal/store"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config file")
	debug := flag.Bool("debug", false, "enable debug logging (overrides SWITCHBOARD_LOG_LEVEL)")
	flag.Parse()

	// Set up structured JSON logging.
	level := parseLogLevel(os.Getenv("SWITCHBOARD_LOG_LEVEL"))
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err, "path", *configPath)
		os.Exit(1)
	}
	slog.Info("config loaded", "path", *configPath, "bridge_name", cfg.Bridge.Name)

	// Initialize components in dependency order.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Store (SQLite)
	st, err := store.New(cfg.Bridge.DataDir)
	if err != nil {
		slog.Error("failed to initialize store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// 2. jcode client
	jc, err := jcode.NewClient(cfg.Jcode.SocketPath, cfg.Jcode.AutoSpawn, cfg.Jcode.SpawnCommand)
	if err != nil {
		slog.Error("failed to initialize jcode client", "error", err)
		os.Exit(1)
	}
	defer jc.Close()

	// 3. Slack edge
	edge, err := slack.NewEdge(cfg.Slack, cfg.Channels, cfg.Identities)
	if err != nil {
		slog.Error("failed to initialize slack edge", "error", err)
		os.Exit(1)
	}
	edge.SetBotAllowlist(cfg.Bridge.BotAllowlist)

	// 4. Outbound queue (backed by Slack edge)
	out := outbound.NewQueue(edge)

	// 5. Ingest server
	ing := ingest.NewServer(cfg.Ingest, st)

	// 6. Router (wires everything together)
	rt := router.New(cfg, st, jc, edge, out)

	// Wire ingest -> router.
	ing.SetHandler(func(item *store.WebhookInboxItem) {
		// Parse the webhook body and dispatch to router.
		rt.EnqueueWebhook(webhookFromInbox(item))
	})

	// Start all components.
	go edge.Run(ctx)
	go out.Run(ctx)
	go func() {
		if err := ing.Run(ctx); err != nil {
			slog.Error("ingest server error", "error", err)
		}
	}()
	go func() {
		if err := rt.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("router error", "error", err)
		}
	}()

	slog.Info("switchboard started",
		"listen_addr", cfg.Ingest.ListenAddr,
		"channels", len(cfg.Channels),
		"routes", len(cfg.Routes),
	)

	// Signal handling: SIGHUP for config reload, SIGINT/SIGTERM for shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			slog.Info("SIGHUP received, reloading config")
			newCfg, err := config.Load(*configPath)
			if err != nil {
				slog.Error("config reload failed", "error", err)
				continue
			}
			rt.Reload(newCfg.Routes)
			slog.Info("config reloaded successfully")
		case syscall.SIGINT, syscall.SIGTERM:
			slog.Info("shutdown signal received", "signal", sig.String())
			cancel()
			slog.Info("switchboard stopped")
			return
		}
	}
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return fmt.Sprintf("%s/.config/switchboard/config.toml", home)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// webhookFromInbox converts a persisted webhook inbox item to a router WebhookEvent.
func webhookFromInbox(item *store.WebhookInboxItem) *router.WebhookEvent {
	evt := &router.WebhookEvent{
		Source:      item.Source,
		RawBody:     item.BodyBlob,
		Idempotency: item.IdempotencyKey,
	}

	// Try to parse body as JSON to extract event_type and payload.
	var payload map[string]interface{}
	if err := json.Unmarshal(item.BodyBlob, &payload); err == nil {
		evt.Payload = payload
		// Try to extract event_type from common fields.
		if et, ok := payload["event_type"].(string); ok {
			evt.EventType = et
		} else if et, ok := payload["action"].(string); ok {
			evt.EventType = et
		}
	}

	return evt
}
