// Switchboard bridges Slack channels to jcode agent sessions with webhook
// ingestion, message coalescing, and intelligent routing.
package main

import (
	"context"
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
	"github.com/format5/switchboard/internal/router"
	"github.com/format5/switchboard/internal/slack"
	"github.com/format5/switchboard/internal/store"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config file")
	flag.Parse()

	// Set up structured JSON logging.
	level := parseLogLevel(os.Getenv("SWITCHBOARD_LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err, "path", *configPath)
		os.Exit(1)
	}
	slog.Info("config loaded", "path", *configPath, "bridge_name", cfg.Bridge.Name)

	// Initialize components in order.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Store (SQLite)
	st, err := store.New(cfg.Bridge.DataDir)
	if err != nil {
		slog.Error("failed to initialize store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// 2. Jcode client
	jc, err := jcode.NewClient(cfg.Jcode.SocketPath, cfg.Jcode.AutoSpawn, cfg.Jcode.SpawnCommand)
	if err != nil {
		slog.Error("failed to initialize jcode client", "error", err)
		os.Exit(1)
	}
	defer jc.Close()

	// 3. Slack edge
	se, err := slack.NewEdge(cfg.Slack, cfg.Channels, cfg.Identities)
	if err != nil {
		slog.Error("failed to initialize slack edge", "error", err)
		os.Exit(1)
	}

	// 4. Ingest server
	ing, err := ingest.NewServer(cfg.Ingest)
	if err != nil {
		slog.Error("failed to initialize ingest server", "error", err)
		os.Exit(1)
	}

	// 5. Router
	rt := router.New(cfg.Routes, se, jc, st, ing)

	// Start all components.
	go se.Run(ctx)
	go ing.Run(ctx)
	go rt.Run(ctx)

	slog.Info("switchboard started", "listen_addr", cfg.Ingest.ListenAddr)

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
