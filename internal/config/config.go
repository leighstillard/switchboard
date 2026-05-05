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
	Ingest     IngestConfig                `toml:"ingest"`
	GitHub     GitHubConfig                `toml:"github"`
	Render     RenderConfig                `toml:"render"`
	Routing    RoutingConfig2              `toml:"routing"`
	Channels   []ChannelConfig             `toml:"channels"`
	Routes     []RouteConfig               `toml:"routes"`
	Identities map[string]IdentityConfig   `toml:"identities"`
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
	LLM LLMRoutingConfig `toml:"llm"`
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

	// Use ~/workspace/<name> format in the file (not expanded path)
	entry := fmt.Sprintf("\n[[channels]]\nid = %q\nname = %q\nworkdir = \"~/workspace/%s\"\nidentity = %q\nicon_url = \"\"\n",
		ch.ID, ch.Name, ch.Name, ch.Identity)

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
