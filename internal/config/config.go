// Package config handles TOML configuration loading with environment variable
// substitution and hot-reload support for Switchboard.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// slackIDRe matches a Slack channel/user ID: starts with C, D, G, U, or W
// followed by alphanumerics.
var slackIDRe = regexp.MustCompile(`^[CDGUW][A-Z0-9]{8,12}$`)

// Config is the top-level configuration structure matching the Switchboard spec §5.
type Config struct {
	Bridge     BridgeConfig                `toml:"bridge"`
	Slack      SlackConfig                 `toml:"slack"`
	Jcode      JcodeConfig                 `toml:"jcode"`
	Claude     ClaudeConfig                `toml:"claude"`
	Ingest     IngestConfig                `toml:"ingest"`
	GitHub     GitHubConfig                `toml:"github"`
	Render     RenderConfig                `toml:"render"`
	Routing    RoutingConfig2              `toml:"routing"`
	Channels   []ChannelConfig             `toml:"channels"`
	Routes     []RouteConfig               `toml:"routes"`
	Crons      []CronConfig                `toml:"cron"`
	Identities map[string]IdentityConfig   `toml:"identities"`
}

// CronConfig defines a scheduled cron job that dispatches a prompt.
type CronConfig struct {
	ID        string `toml:"id"`
	Schedule  string `toml:"schedule"`
	ChannelID string `toml:"channel_id"`
	Prompt    string `toml:"prompt"`
	UserID    string `toml:"user_id"`
	Enabled   bool   `toml:"enabled"`
}

// GitHubConfig holds GitHub-specific routing configuration.
type GitHubConfig struct {
	// Repos maps "owner/repo" to a Slack channel ID for webhook routing.
	Repos map[string]string `toml:"repos"`
}

// RenderConfig holds rendering settings for agent output.
type RenderConfig struct {
	Descriptions              DescriptionsConfig `toml:"descriptions"`
	MarkdownSplitChars        int                `toml:"markdown_split_chars"`
	StrictDirectiveValidation bool               `toml:"strict_directive_validation"`
}

// DescriptionsConfig controls terse tool description generation (Feature 1c).
type DescriptionsConfig struct {
	// TargetWords is the soft word-count target for descriptions (default 8).
	TargetWords int `toml:"target_words"`
	// HardTruncateWords is the hard word-count limit; descriptions exceeding
	// this are truncated with an ellipsis (default 10).
	HardTruncateWords int `toml:"hard_truncate_words"`
	// LogTruncations enables logging when a description is truncated (default true).
	LogTruncations bool `toml:"log_truncations"`
	// LogDriftThreshold is the rolling-average word count above which a warning
	// is logged (default 7).
	LogDriftThreshold float64 `toml:"log_drift_threshold"`
}

// BridgeConfig holds top-level bridge settings.
type BridgeConfig struct {
	Name           string        `toml:"name"`
	DataDir        string        `toml:"data_dir"`
	DefaultWorkdir string        `toml:"default_workdir"`
	Routing        RoutingConfig `toml:"routing"`
	Audit          AuditConfig   `toml:"audit"`
	Files          FilesConfig   `toml:"files"`
	BotAllowlist   []string      `toml:"bot_allowlist"`
}

// RoutingConfig controls message routing behaviour.
type RoutingConfig struct {
	WorkspaceFallback bool `toml:"workspace_fallback"`
}

// RoutingConfig2 holds the top-level [routing] section with LLM router config.
type RoutingConfig2 struct {
	LLM     LLMRoutingConfig     `toml:"llm"`
	Backend BackendRoutingConfig `toml:"backend"`
}

// BackendRoutingConfig selects the default agent backend ("jcode" or "claude").
type BackendRoutingConfig struct {
	Default string `toml:"default"` // "jcode" or "claude"; empty defaults to "jcode"
}

// LLMRoutingConfig holds settings for the LLM-based notification router.
type LLMRoutingConfig struct {
	Enabled             bool    `toml:"enabled"`
	Model               string  `toml:"model"`
	ConfidenceThreshold int     `toml:"confidence_threshold"`
	MaxInputTokens      int     `toml:"max_input_tokens"`
	IncludeThreadCount  int     `toml:"include_thread_count"`
	APIKey              string  `toml:"api_key"`
	MonthlyBudgetUSD    float64 `toml:"monthly_budget_usd"`
}

// AuditConfig controls audit log settings.
type AuditConfig struct {
	RetentionDays int  `toml:"retention_days"`
	Verbose       bool `toml:"verbose"`
}

// FilesConfig controls file transfer limits.
type FilesConfig struct {
	MaxInboundMB  int `toml:"max_inbound_mb"`
	MaxOutboundMB int `toml:"max_outbound_mb"`
}

// SlackConfig holds Slack API credentials and settings.
type SlackConfig struct {
	AppToken      string `toml:"app_token"`
	BotToken      string `toml:"bot_token"`
	SigningSecret  string `toml:"signing_secret"`
}

// JcodeConfig holds jcode socket connection settings.
type JcodeConfig struct {
	SocketPath   string `toml:"socket_path"`
	AutoSpawn    bool   `toml:"auto_spawn"`
	SpawnCommand string `toml:"spawn_command"`
}

// ClaudeConfig holds Claude Code CLI settings.
type ClaudeConfig struct {
	Binary              string            `toml:"binary"`                // default "claude"
	Model               string            `toml:"model"`                 // default "claude-sonnet-4-20250514"
	PermissionPolicy    string            `toml:"permission_policy"`     // allow_all | deny_all | accept_edits_only
	SettingSources      string            `toml:"setting_sources"`       // default "project,local" (excludes user-layer hooks)
	AppendSystemPrompt  string            `toml:"append_system_prompt"`  // appended to system prompt
	GracefulStopTimeout string            `toml:"graceful_stop_timeout"` // e.g. "30s"
	IdleEvictionTimeout string            `toml:"idle_eviction_timeout"` // e.g. "30m"; "0" = never
	MinVersion          string            `toml:"min_version"`           // default "2.1.0"
	MaxVersion          string            `toml:"max_version"`           // optional ceiling
	ExtraEnv            map[string]string `toml:"extra_env"`             // applied last, overrides inherited env
	ExtraArgs           []string          `toml:"extra_args"`            // additional CLI arguments

	// PermissionMode is the legacy (pre-policy) knob, retained for backwards
	// compatibility and translated on load. Use PermissionPolicy instead.
	PermissionMode string `toml:"permission_mode"`
}

// ResolvePermissionPolicy translates the (possibly legacy) permission config to a
// policy name. It returns a non-empty warning when a legacy permission_mode is
// mapped, and an error when both permission_mode and permission_policy are set.
func (c ClaudeConfig) ResolvePermissionPolicy() (policy, warning string, err error) {
	if c.PermissionMode != "" && c.PermissionPolicy != "" {
		return "", "", fmt.Errorf("config: set either claude.permission_policy or the legacy claude.permission_mode, not both (remove permission_mode)")
	}
	if c.PermissionPolicy != "" {
		switch c.PermissionPolicy {
		case "allow_all", "deny_all", "accept_edits_only":
			return c.PermissionPolicy, "", nil
		default:
			// Reject unknown values loudly — silently falling back to allow_all
			// would be a security footgun (a typo'd deny becomes allow).
			return "", "", fmt.Errorf("config: unknown claude.permission_policy %q (want allow_all, deny_all, or accept_edits_only)", c.PermissionPolicy)
		}
	}
	switch c.PermissionMode {
	case "", "bypassPermissions":
		return "allow_all", "", nil // silent: equivalent behaviour
	case "default":
		return "allow_all", "claude.permission_mode=default mapped to permission_policy=allow_all", nil
	case "acceptEdits":
		return "accept_edits_only", "claude.permission_mode=acceptEdits mapped to permission_policy=accept_edits_only", nil
	case "dontAsk":
		return "deny_all", "claude.permission_mode=dontAsk mapped to permission_policy=deny_all", nil
	case "plan":
		return "allow_all", "claude.permission_mode=plan mapped to permission_policy=allow_all (plan mode is not preserved)", nil
	default:
		return "allow_all", fmt.Sprintf("unknown claude.permission_mode=%q mapped to permission_policy=allow_all", c.PermissionMode), nil
	}
}

// IdentityConfig defines a bot identity persona.
type IdentityConfig struct {
	DisplayName string `toml:"display_name"`
	IconURL     string `toml:"icon_url"`
}

// ChannelConfig maps a Slack channel to a workspace directory.
type ChannelConfig struct {
	ID       string `toml:"id"`
	Name     string `toml:"name"`
	Workdir  string `toml:"workdir"`
	Identity string `toml:"identity"`
	IconURL  string `toml:"icon_url"`
	Backend  string `toml:"backend"` // "jcode" or "claude"; empty = inherit default
	Model    string `toml:"model"`   // optional per-channel model override
}

// IngestConfig holds webhook ingestion server settings.
type IngestConfig struct {
	ListenAddr       string                   `toml:"listen_addr"`
	FallbackChannelID string                  `toml:"fallback_channel_id"`
	DurableInbox     bool                     `toml:"durable_inbox"`
	MaxBodyKB        int                      `toml:"max_body_kb"`
	MaxAttempts      int                      `toml:"max_attempts"`
	Sources          map[string]SourceConfig  `toml:"sources"`
}

// SourceConfig defines an ingest source with HMAC verification.
type SourceConfig struct {
	Secret          string `toml:"secret"`
	SignatureHeader string `toml:"signature_header"`
}

// RouteConfig defines a routing rule from source to destination.
type RouteConfig struct {
	Source      string            `toml:"source"`
	Match       map[string]string `toml:"match"`
	Destination RouteDestination  `toml:"destination"`
	Template    string            `toml:"template"`
	Correlation CorrelationConfig `toml:"correlation"`
}

// RouteDestination specifies where a matched event is routed.
type RouteDestination struct {
	ChannelID        string `toml:"channel_id"`
	CorrelationField string `toml:"correlation_field"`
}

// CorrelationConfig defines how webhook events are correlated to threads.
type CorrelationConfig struct {
	Field   string `toml:"field"`
	TTLDays int    `toml:"ttl_days"`
}

// envSubstRe matches ${VAR} patterns for environment variable substitution.
var envSubstRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Load reads and parses a TOML config file, performing environment variable
// substitution and path expansion.
func Load(path string) (*Config, error) {
	path = expandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	// Substitute ${VAR} with environment variables.
	content := envSubstRe.ReplaceAllStringFunc(string(data), func(match string) string {
		varName := match[2 : len(match)-1]
		return os.Getenv(varName)
	})

	var cfg Config
	if _, err := toml.Decode(content, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Expand paths.
	cfg.Bridge.DataDir = expandPath(cfg.Bridge.DataDir)
	cfg.Bridge.DefaultWorkdir = expandPath(cfg.Bridge.DefaultWorkdir)
	for i := range cfg.Channels {
		cfg.Channels[i].Workdir = expandPath(cfg.Channels[i].Workdir)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks that required fields are set and values are sane.
func (c *Config) validate() error {
	if c.Slack.BotToken == "" {
		return fmt.Errorf("config: slack.bot_token is required")
	}
	if c.Slack.AppToken == "" {
		return fmt.Errorf("config: slack.app_token is required")
	}

	// Validate channel IDs and check for duplicates.
	seen := make(map[string]string) // id -> name
	for _, ch := range c.Channels {
		if ch.ID == "" {
			return fmt.Errorf("config: channel %q has empty id", ch.Name)
		}
		if !slackIDRe.MatchString(ch.ID) {
			return fmt.Errorf("config: channel %q has invalid Slack ID %q (expected C/D/G + 8-12 alphanumerics)", ch.Name, ch.ID)
		}
		if prevName, dup := seen[ch.ID]; dup {
			return fmt.Errorf("config: duplicate channel id %q (used by %q and %q)", ch.ID, prevName, ch.Name)
		}
		seen[ch.ID] = ch.Name
	}

	// Validate HMAC secrets are not trivially short.
	for name, src := range c.Ingest.Sources {
		if src.Secret != "" && len(src.Secret) < 16 {
			return fmt.Errorf("config: ingest source %q secret is too short (%d chars, minimum 16)", name, len(src.Secret))
		}
	}

	// Warn (but don't fail) if route destinations reference unknown channels.
	for _, route := range c.Routes {
		destID := route.Destination.ChannelID
		if destID != "" {
			if _, known := seen[destID]; !known {
				slog.Warn("config: route destination references unknown channel",
					"route_source", route.Source,
					"channel_id", destID,
				)
			}
		}
	}

	// Validate cron jobs.
	cronIDs := make(map[string]bool)
	for i, cj := range c.Crons {
		if cj.ID == "" {
			return fmt.Errorf("config: cron[%d] has empty id", i)
		}
		if cronIDs[cj.ID] {
			return fmt.Errorf("config: duplicate cron id %q", cj.ID)
		}
		cronIDs[cj.ID] = true

		fields := strings.Fields(cj.Schedule)
		if len(fields) != 5 {
			return fmt.Errorf("config: cron %q has invalid schedule %q (expected 5 fields)", cj.ID, cj.Schedule)
		}
		if cj.ChannelID == "" {
			return fmt.Errorf("config: cron %q has empty channel_id", cj.ID)
		}
		if cj.Prompt == "" {
			return fmt.Errorf("config: cron %q has empty prompt", cj.ID)
		}
		if _, known := seen[cj.ChannelID]; !known {
			slog.Warn("config: cron job references unknown channel",
				"cron_id", cj.ID,
				"channel_id", cj.ChannelID,
			)
		}
	}

	return nil
}

// expandPath expands ~ to the user's home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// InsertChannel inserts a new [[channels]] entry into the config file.
// It places the entry before the [ingest] section to maintain file structure.
func InsertChannel(configPath string, ch ChannelConfig) error {
	path := expandPath(configPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read for insert: %w", err)
	}

	// Use provided workdir, falling back to ~/workspace/<name> if empty.
	workdir := ch.Workdir
	if workdir == "" {
		workdir = fmt.Sprintf("~/workspace/%s", ch.Name)
	}
	entry := fmt.Sprintf("\n[[channels]]\nid = %q\nname = %q\nworkdir = %q\nidentity = %q\nicon_url = %q\n",
		ch.ID, ch.Name, workdir, ch.Identity, ch.IconURL)

	content := string(data)

	// Insert before [ingest] section to maintain file structure
	ingestIdx := strings.Index(content, "\n[ingest]")
	if ingestIdx == -1 {
		content += entry
	} else {
		content = content[:ingestIdx] + entry + content[ingestIdx:]
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("config: write after insert: %w", err)
	}
	return nil
}
