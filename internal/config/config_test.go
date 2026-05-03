package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Set up test environment variables.
	os.Setenv("SLACK_APP_TOKEN", "xapp-test-token")
	os.Setenv("SLACK_BOT_TOKEN", "xoxb-test-token")
	os.Setenv("SLACK_SIGNING_SECRET", "test-secret")
	os.Setenv("GITHUB_WEBHOOK_SECRET", "gh-secret")
	defer func() {
		os.Unsetenv("SLACK_APP_TOKEN")
		os.Unsetenv("SLACK_BOT_TOKEN")
		os.Unsetenv("SLACK_SIGNING_SECRET")
		os.Unsetenv("GITHUB_WEBHOOK_SECRET")
	}()

	// Write a test config file.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[bridge]
name = "test-bridge"
data_dir = "` + dir + `/data"

[slack]
app_token = "${SLACK_APP_TOKEN}"
bot_token = "${SLACK_BOT_TOKEN}"
signing_secret = "${SLACK_SIGNING_SECRET}"

[jcode]
socket_path = ""
auto_spawn = true
spawn_command = "jcode serve --quiet"

[identities.pm]
display_name = "PM"
icon_url = "https://example.com/pm.png"

[bridge.routing]
workspace_fallback = false

[bridge.audit]
retention_days = 14
verbose = false

[bridge.files]
max_inbound_mb = 10
max_outbound_mb = 25

[[channels]]
id = "C0123ABC"
name = "test-channel"
workdir = "` + dir + `/workspace/test"
identity = "Test Worker"
icon_url = "https://example.com/test.png"

[ingest]
listen_addr = "127.0.0.1:9999"
fallback_channel_id = "C0789XYZ"
durable_inbox = true
max_body_kb = 512
max_attempts = 3

[ingest.sources.github]
secret = "${GITHUB_WEBHOOK_SECRET}"
signature_header = "X-Hub-Signature-256"

[[routes]]
source = "github"
[routes.match]
event_type = "pull_request"
repo = "format5/test"
[routes.destination]
channel_id = "C0123ABC"
[routes.correlation]
field = "pull_request.html_url"
ttl_days = 7
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify environment variable substitution.
	if cfg.Slack.AppToken != "xapp-test-token" {
		t.Errorf("app_token = %q, want %q", cfg.Slack.AppToken, "xapp-test-token")
	}
	if cfg.Slack.BotToken != "xoxb-test-token" {
		t.Errorf("bot_token = %q, want %q", cfg.Slack.BotToken, "xoxb-test-token")
	}

	// Verify bridge config.
	if cfg.Bridge.Name != "test-bridge" {
		t.Errorf("name = %q, want %q", cfg.Bridge.Name, "test-bridge")
	}
	if cfg.Bridge.Audit.RetentionDays != 14 {
		t.Errorf("retention_days = %d, want 14", cfg.Bridge.Audit.RetentionDays)
	}
	if cfg.Bridge.Files.MaxInboundMB != 10 {
		t.Errorf("max_inbound_mb = %d, want 10", cfg.Bridge.Files.MaxInboundMB)
	}
	if cfg.Bridge.Routing.WorkspaceFallback {
		t.Error("workspace_fallback should be false")
	}

	// Verify channels.
	if len(cfg.Channels) != 1 {
		t.Fatalf("channels = %d, want 1", len(cfg.Channels))
	}
	ch := cfg.Channels[0]
	if ch.ID != "C0123ABC" {
		t.Errorf("channel id = %q", ch.ID)
	}
	if ch.Identity != "Test Worker" {
		t.Errorf("channel identity = %q", ch.Identity)
	}

	// Verify jcode config.
	if !cfg.Jcode.AutoSpawn {
		t.Error("auto_spawn should be true")
	}

	// Verify ingest config.
	if cfg.Ingest.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("listen_addr = %q", cfg.Ingest.ListenAddr)
	}
	if cfg.Ingest.MaxBodyKB != 512 {
		t.Errorf("max_body_kb = %d", cfg.Ingest.MaxBodyKB)
	}

	// Verify routes.
	if len(cfg.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(cfg.Routes))
	}
	route := cfg.Routes[0]
	if route.Source != "github" {
		t.Errorf("route source = %q", route.Source)
	}
	if route.Match["event_type"] != "pull_request" {
		t.Errorf("route match event_type = %q", route.Match["event_type"])
	}
	if route.Destination.ChannelID != "C0123ABC" {
		t.Errorf("route destination channel_id = %q", route.Destination.ChannelID)
	}
	if route.Correlation.Field != "pull_request.html_url" {
		t.Errorf("route correlation field = %q", route.Correlation.Field)
	}
	if route.Correlation.TTLDays != 7 {
		t.Errorf("route correlation ttl_days = %d", route.Correlation.TTLDays)
	}

	// Verify identities.
	pm, ok := cfg.Identities["pm"]
	if !ok {
		t.Fatal("pm identity not found")
	}
	if pm.DisplayName != "PM" {
		t.Errorf("pm display_name = %q", pm.DisplayName)
	}
}

func TestLoadConfig_MissingRequired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[bridge]
name = "test"

[slack]
app_token = ""
bot_token = ""
`
	os.WriteFile(cfgPath, []byte(content), 0644)

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/workspace/test", filepath.Join(home, "workspace/test")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		got := expandPath(tt.input)
		if got != tt.want {
			t.Errorf("expandPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
