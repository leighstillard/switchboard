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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/cron"
	"github.com/format5/switchboard/internal/ingest"
	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/outbound"
	"github.com/format5/switchboard/internal/render"
	"github.com/format5/switchboard/internal/router"
	"github.com/format5/switchboard/internal/slack"
	"github.com/format5/switchboard/internal/store"
)

// Set at build time via -ldflags.
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

func main() {
	// Handle subcommands before flag parsing.
	if len(os.Args) > 1 && os.Args[1] == "cron" {
		os.Exit(runCronCLI(os.Args[2:]))
	}

	configPath := flag.String("config", defaultConfigPath(), "path to config file")
	debug := flag.Bool("debug", false, "enable debug logging (overrides SWITCHBOARD_LOG_LEVEL)")
	showVersion := flag.Bool("version", false, "print version and exit")
	validateConfig := flag.Bool("validate-config", false, "validate config and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("switchboard %s (commit %s, built %s)\n", version, gitCommit, buildTime)
		return
	}

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
		if *validateConfig {
			fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
			os.Exit(1)
		}
		slog.Error("failed to load config", "error", err, "path", *configPath)
		os.Exit(1)
	}

	// Apply render description settings from config.
	render.ConfigureDescriptions(cfg.Render.Descriptions.TargetWords, cfg.Render.Descriptions.HardTruncateWords)

	if *validateConfig {
		fmt.Printf("OK: config valid (%d channels, %d routes, ingest=%s)\n",
			len(cfg.Channels), len(cfg.Routes), cfg.Ingest.ListenAddr)
		return
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
	rt := router.New(cfg, st, jc, edge, out, *configPath)

	// 7. Cron scheduler (merge config + DB jobs)
	cronJobs := mergeCronJobs(cronJobsFromConfig(cfg.Crons), cronJobsFromDB(st))
	cronSched, err := cron.New(cronJobs, &cronDispatchAdapter{rt: rt}, st)
	if err != nil {
		slog.Error("failed to initialize cron scheduler", "error", err)
		os.Exit(1)
	}

	// Wire ingest -> router.
	ing.SetHandler(func(item *store.WebhookInboxItem) {
		// Parse the webhook body and dispatch to router.
		rt.EnqueueWebhook(webhookFromInbox(item))
	})

	// Enable test injection in debug mode.
	if *debug {
		ing.SetTestInjectHandler(func(channelID, threadTS, userID, text string) string {
			return rt.InjectMessage(channelID, threadTS, userID, text)
		})
	}

	// Wire dispatch endpoint -> router.
	ing.SetDispatchHandler(func(ctx context.Context, channelID, prompt, userID string) (string, string, error) {
		result, err := rt.Dispatch(ctx, router.DispatchRequest{
			ChannelID: channelID,
			Prompt:    prompt,
			UserID:    userID,
		})
		if err != nil {
			return "", "", err
		}
		return result.ThreadTS, result.SessionID, nil
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
	go cronSched.Run(ctx)

	slog.Info("switchboard started",
		"listen_addr", cfg.Ingest.ListenAddr,
		"channels", len(cfg.Channels),
		"routes", len(cfg.Routes),
	)

	// Write PID file so CLI subcommands can send SIGHUP.
	pidPath := filepath.Join(cfg.Bridge.DataDir, "switchboard.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		slog.Warn("failed to write PID file", "path", pidPath, "error", err)
	} else {
		defer os.Remove(pidPath)
	}

	// Signal handling: SIGHUP for config reload, SIGINT/SIGTERM for shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	forceShutdown := false
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
			rt.Reload(newCfg)
			edge.ReloadConfig(newCfg.Channels, newCfg.Identities)
			edge.SetBotAllowlist(newCfg.Bridge.BotAllowlist)
			render.ConfigureDescriptions(newCfg.Render.Descriptions.TargetWords, newCfg.Render.Descriptions.HardTruncateWords)
			if err := cronSched.Reload(mergeCronJobs(cronJobsFromConfig(newCfg.Crons), cronJobsFromDB(st))); err != nil {
				slog.Error("cron reload failed", "error", err)
			}
			slog.Info("config reloaded successfully")
		case syscall.SIGINT, syscall.SIGTERM:
			// Check for active processing sessions before shutting down.
			if !forceShutdown {
				sessions, _ := st.ListActiveSessions()
				var processing []string
				for _, s := range sessions {
					if s.Status == "processing" {
						processing = append(processing, fmt.Sprintf("  %s (thread %s)", s.FriendlyName, s.ThreadTS))
					}
				}
				if len(processing) > 0 {
					slog.Warn("shutdown blocked: agents are still processing",
						"count", len(processing),
					)
					for _, p := range processing {
						slog.Warn("  active session", "session", p)
					}
					slog.Warn("send signal again to force shutdown")
					forceShutdown = true
					continue
				}
			}
			slog.Info("shutdown signal received", "signal", sig.String())
			rt.NotifyShutdown()
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
		Headers:     make(map[string]string),
	}

	// Parse persisted headers.
	if item.HeadersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(item.HeadersJSON), &headers); err == nil {
			evt.Headers = headers
		}
	}

	// Try to parse body as JSON to extract payload.
	var payload map[string]interface{}
	if err := json.Unmarshal(item.BodyBlob, &payload); err == nil {
		evt.Payload = payload
	}

	// Determine event type based on source.
	switch item.Source {
	case "github":
		// GitHub uses X-GitHub-Event header as the canonical event type
		// (e.g., "issues", "pull_request", "push").
		if ghEvent := evt.Headers["X-Github-Event"]; ghEvent != "" {
			evt.EventType = ghEvent
		} else if ghEvent := evt.Headers["X-GitHub-Event"]; ghEvent != "" {
			evt.EventType = ghEvent
		}
	default:
		// Generic: try common body fields.
		if evt.Payload != nil {
			if et, ok := evt.Payload["event_type"].(string); ok {
				evt.EventType = et
			} else if et, ok := evt.Payload["action"].(string); ok {
				evt.EventType = et
			}
		}
	}

	return evt
}

// ---------------------------------------------------------------------------
// Cron adapter
// ---------------------------------------------------------------------------

// cronDispatchAdapter adapts router.Router to the cron.Dispatcher interface.
type cronDispatchAdapter struct {
	rt *router.Router
}

func (a *cronDispatchAdapter) Dispatch(ctx context.Context, req cron.DispatchRequest) (*cron.DispatchResult, error) {
	result, err := a.rt.Dispatch(ctx, router.DispatchRequest{
		ChannelID: req.ChannelID,
		Prompt:    req.Prompt,
		UserID:    req.UserID,
	})
	if err != nil {
		return nil, err
	}
	return &cron.DispatchResult{
		ThreadTS:  result.ThreadTS,
		SessionID: result.SessionID,
	}, nil
}

// cronJobsFromConfig converts config cron entries to cron.Job values.
func cronJobsFromConfig(crons []config.CronConfig) []cron.Job {
	jobs := make([]cron.Job, len(crons))
	for i, c := range crons {
		jobs[i] = cron.Job{
			ID:        c.ID,
			Schedule:  c.Schedule,
			ChannelID: c.ChannelID,
			Prompt:    c.Prompt,
			UserID:    c.UserID,
			Enabled:   c.Enabled,
		}
	}
	return jobs
}

// cronJobsFromDB reads runtime cron jobs from the store.
func cronJobsFromDB(st *store.Store) []cron.Job {
	dbJobs, err := st.ListCronJobs()
	if err != nil {
		slog.Error("cron: failed to load DB jobs", "error", err)
		return nil
	}
	jobs := make([]cron.Job, len(dbJobs))
	for i, j := range dbJobs {
		jobs[i] = cron.Job{
			ID:        j.ID,
			Schedule:  j.Schedule,
			ChannelID: j.ChannelID,
			Prompt:    j.Prompt,
			UserID:    j.UserID,
			Enabled:   j.Enabled,
		}
	}
	return jobs
}

// mergeCronJobs merges config and DB cron jobs. DB jobs take precedence on
// ID conflicts, which allows runtime overrides of config-defined schedules.
func mergeCronJobs(configJobs, dbJobs []cron.Job) []cron.Job {
	byID := make(map[string]cron.Job, len(configJobs)+len(dbJobs))
	order := make([]string, 0, len(configJobs)+len(dbJobs))

	for _, j := range configJobs {
		byID[j.ID] = j
		order = append(order, j.ID)
	}
	for _, j := range dbJobs {
		if _, exists := byID[j.ID]; !exists {
			order = append(order, j.ID)
		}
		byID[j.ID] = j // DB wins
	}

	merged := make([]cron.Job, 0, len(order))
	for _, id := range order {
		merged = append(merged, byID[id])
	}
	return merged
}

// ---------------------------------------------------------------------------
// Cron CLI
// ---------------------------------------------------------------------------

// runCronCLI handles `switchboard cron <subcommand>` invocations.
// It opens the store directly (no daemon needed) and operates on the DB.
func runCronCLI(args []string) int {
	if len(args) == 0 {
		printCronUsage()
		return 1
	}

	subcmd := args[0]
	subArgs := args[1:]

	switch subcmd {
	case "add":
		return cronAdd(subArgs)
	case "list", "ls":
		return cronList(subArgs)
	case "delete", "rm":
		return cronDelete(subArgs)
	case "enable":
		return cronSetEnabled(subArgs, true)
	case "disable":
		return cronSetEnabled(subArgs, false)
	case "help", "--help", "-h":
		printCronUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown cron subcommand: %s\n\n", subcmd)
		printCronUsage()
		return 1
	}
}

func printCronUsage() {
	fmt.Fprintf(os.Stderr, `Usage: switchboard cron <command> [options]

Commands:
  add       Add a new cron job
  list      List all cron jobs (config + DB)
  delete    Delete a runtime cron job
  enable    Enable a runtime cron job
  disable   Disable a runtime cron job

Examples:
  switchboard cron add --id daily-audit --schedule "0 21 * * *" \
    --channel C0AL12WCNBG --prompt "Run the audit"

  switchboard cron list
  switchboard cron list --json

  switchboard cron delete daily-audit
  switchboard cron enable daily-audit
  switchboard cron disable daily-audit
`)
}

func cronAdd(args []string) int {
	fs := flag.NewFlagSet("cron add", flag.ExitOnError)
	id := fs.String("id", "", "unique job identifier (required)")
	schedule := fs.String("schedule", "", "5-field cron expression (required)")
	channel := fs.String("channel", "", "Slack channel ID (required)")
	prompt := fs.String("prompt", "", "prompt text to dispatch (required)")
	user := fs.String("user", "", "Slack user ID (optional)")
	disabled := fs.Bool("disabled", false, "create in disabled state")
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	fs.Parse(args)

	// Validate required fields.
	var errs []string
	if *id == "" {
		errs = append(errs, "--id is required")
	}
	if *schedule == "" {
		errs = append(errs, "--schedule is required")
	}
	if *channel == "" {
		errs = append(errs, "--channel is required")
	}
	if *prompt == "" {
		errs = append(errs, "--prompt is required")
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "error: %s\n", e)
		}
		return 1
	}

	// Validate cron expression.
	if _, err := cron.Parse(*schedule); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid schedule %q: %v\n", *schedule, err)
		return 1
	}

	st, err := openStoreFromConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	now := time.Now().Unix()
	job := &store.CronJob{
		ID:        *id,
		Schedule:  *schedule,
		ChannelID: *channel,
		Prompt:    *prompt,
		UserID:    *user,
		Enabled:   !*disabled,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := st.InsertCronJob(job); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	status := "enabled"
	if *disabled {
		status = "disabled"
	}
	fmt.Printf("added cron job %q (%s) [%s]\n", *id, *schedule, status)

	signalDaemon(*configPath)
	return 0
}

func cronList(args []string) int {
	fs := flag.NewFlagSet("cron list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "output as JSON")
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	st, err := store.New(cfg.Bridge.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	dbJobs, err := st.ListCronJobs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	type listEntry struct {
		ID        string `json:"id"`
		Schedule  string `json:"schedule"`
		ChannelID string `json:"channel_id"`
		Prompt    string `json:"prompt"`
		UserID    string `json:"user_id,omitempty"`
		Enabled   bool   `json:"enabled"`
		Source    string `json:"source"` // "config" or "db"
	}

	var entries []listEntry

	// Config jobs first.
	dbIDs := make(map[string]bool, len(dbJobs))
	for _, j := range dbJobs {
		dbIDs[j.ID] = true
	}
	for _, c := range cfg.Crons {
		if dbIDs[c.ID] {
			continue // DB override takes precedence; will show as "db"
		}
		entries = append(entries, listEntry{
			ID:        c.ID,
			Schedule:  c.Schedule,
			ChannelID: c.ChannelID,
			Prompt:    c.Prompt,
			UserID:    c.UserID,
			Enabled:   c.Enabled,
			Source:    "config",
		})
	}

	// DB jobs.
	for _, j := range dbJobs {
		entries = append(entries, listEntry{
			ID:        j.ID,
			Schedule:  j.Schedule,
			ChannelID: j.ChannelID,
			Prompt:    j.Prompt,
			UserID:    j.UserID,
			Enabled:   j.Enabled,
			Source:    "db",
		})
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(entries)
		return 0
	}

	if len(entries) == 0 {
		fmt.Println("no cron jobs configured")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSCHEDULE\tCHANNEL\tENABLED\tSOURCE\tPROMPT")
	for _, e := range entries {
		prompt := e.Prompt
		if len(prompt) > 50 {
			prompt = prompt[:47] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\n",
			e.ID, e.Schedule, e.ChannelID, e.Enabled, e.Source, prompt)
	}
	w.Flush()
	return 0
}

func cronDelete(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: switchboard cron delete <id>\n")
		return 1
	}
	id := args[0]

	configPath := defaultConfigPath()
	if len(args) > 2 && args[1] == "--config" {
		configPath = args[2]
	}

	st, err := openStoreFromConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	if err := st.DeleteCronJob(id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("deleted cron job %q\n", id)
	signalDaemon(configPath)
	return 0
}

func cronSetEnabled(args []string, enabled bool) int {
	if len(args) == 0 {
		verb := "enable"
		if !enabled {
			verb = "disable"
		}
		fmt.Fprintf(os.Stderr, "usage: switchboard cron %s <id>\n", verb)
		return 1
	}
	id := args[0]

	configPath := defaultConfigPath()
	if len(args) > 2 && args[1] == "--config" {
		configPath = args[2]
	}

	st, err := openStoreFromConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer st.Close()

	if err := st.UpdateCronJobEnabled(id, enabled); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	fmt.Printf("%s cron job %q\n", verb, id)
	signalDaemon(configPath)
	return 0
}

// openStoreFromConfig loads config to find DataDir, then opens the store.
func openStoreFromConfig(configPath string) (*store.Store, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	st, err := store.New(cfg.Bridge.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

// signalDaemon sends SIGHUP to the running switchboard process so it reloads
// cron jobs. Silently does nothing if no daemon is running.
func signalDaemon(configPath string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return
	}
	pidPath := filepath.Join(cfg.Bridge.DataDir, "switchboard.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		return
	}
	fmt.Println("sent reload signal to running switchboard")
}
