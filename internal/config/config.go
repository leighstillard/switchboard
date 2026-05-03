// Package config handles TOML configuration loading with environment variable
// substitution and hot-reload support for Switchboard.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration structure matching the Switchboard spec §5.
type Config struct {
	Bridge  BridgeConfig   `toml:"bridge"`
	Slack   SlackConfig    `toml:"slack"`
	Jcode   JcodeConfig    `toml:"jcode"`
	Ingest  IngestConfig   `toml:"ingest"`
	Channels []ChannelConfig `toml:"channels"`
	Routes  []RouteConfig  `toml:"routes"`
	Identities map[string]IdentityConfig `toml:"identities"`
}

// BridgeConfig holds top-level bridge settings.
type BridgeConfig struct {
	Name         string        `toml:"name"`
	DataDir      string        `toml:"data_dir"`
	Routing      RoutingConfig `toml:"routing"`
	Audit        AuditConfig   `toml:"audit"`
	Files        FilesConfig   `toml:"files"`
	BotAllowlist []string      `toml:"bot_allowlist"`
}

// RoutingConfig controls message routing behaviour.
type RoutingConfig struct {
	WorkspaceFallback bool `toml:"workspace_fallback"`
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
	for i := range cfg.Channels {
		cfg.Channels[i].Workdir = expandPath(cfg.Channels[i].Workdir)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks that required fields are set.
func (c *Config) validate() error {
	if c.Slack.BotToken == "" {
		return fmt.Errorf("config: slack.bot_token is required")
	}
	if c.Slack.AppToken == "" {
		return fmt.Errorf("config: slack.app_token is required")
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
