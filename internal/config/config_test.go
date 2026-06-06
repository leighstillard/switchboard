package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Set up test environment variables.
	os.Setenv("SLACK_APP_TOKEN", "xapp-test-token")
	os.Setenv("SLACK_BOT_TOKEN", "xoxb-test-token")
	os.Setenv("SLACK_SIGNING_SECRET", "test-secret")
	os.Setenv("GITHUB_WEBHOOK_SECRET", "gh-secret-long-enough-for-validation")
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
id = "C0123ABCDEF"
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
channel_id = "C0123ABCDEF"
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
	if ch.ID != "C0123ABCDEF" {
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
	if route.Destination.ChannelID != "C0123ABCDEF" {
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

func TestInsertChannel_RespectsProvidedValues(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	base := `[bridge]
name = "test"

[ingest]
listen_addr = "127.0.0.1:9999"
`
	if err := os.WriteFile(cfgPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	// Insert with explicit workdir and icon_url.
	ch := ChannelConfig{
		ID:       "C999TEST",
		Name:     "my-project",
		Workdir:  "~/custom/path",
		Identity: "Worker",
		IconURL:  "https://example.com/icon.png",
	}
	if err := InsertChannel(cfgPath, ch); err != nil {
		t.Fatalf("InsertChannel failed: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, `workdir = "~/custom/path"`) {
		t.Errorf("expected provided workdir, got:\n%s", content)
	}
	if !strings.Contains(content, `icon_url = "https://example.com/icon.png"`) {
		t.Errorf("expected provided icon_url, got:\n%s", content)
	}
}

func TestInsertChannel_FallbackWorkdir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	base := `[bridge]
name = "test"
`
	if err := os.WriteFile(cfgPath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}

	// Insert with empty workdir and icon_url -- should fall back.
	ch := ChannelConfig{
		ID:       "C888TEST",
		Name:     "fallback-proj",
		Identity: "Bot",
	}
	if err := InsertChannel(cfgPath, ch); err != nil {
		t.Fatalf("InsertChannel failed: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, `workdir = "~/workspace/fallback-proj"`) {
		t.Errorf("expected fallback workdir, got:\n%s", content)
	}
	// icon_url should be empty string (quoted)
	if !strings.Contains(content, `icon_url = ""`) {
		t.Errorf("expected empty icon_url, got:\n%s", content)
	}
}

func TestBackendConfig(t *testing.T) {
	os.Setenv("SLACK_APP_TOKEN", "xapp-test")
	os.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	defer func() {
		os.Unsetenv("SLACK_APP_TOKEN")
		os.Unsetenv("SLACK_BOT_TOKEN")
	}()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[bridge]
name = "test"
data_dir = "` + dir + `/data"

[slack]
app_token = "${SLACK_APP_TOKEN}"
bot_token = "${SLACK_BOT_TOKEN}"

[jcode]
socket_path = ""

[routing.backend]
default = "claude"

[claude]
binary = "/usr/local/bin/claude"
permission_mode = "bypassPermissions"
model = "claude-sonnet-4-20250514"
append_system_prompt = "Be concise"
extra_args = ["--max-turns", "5"]

[[channels]]
id = "C0123ABCDEF"
name = "jcode-chan"
workdir = "` + dir + `/w1"

[[channels]]
id = "C0456GHIJKL"
name = "claude-chan"
workdir = "` + dir + `/w2"
backend = "claude"
model = "claude-sonnet-4-20250514"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Global backend default.
	if cfg.Routing.Backend.Default != "claude" {
		t.Errorf("routing.backend.default = %q, want claude", cfg.Routing.Backend.Default)
	}

	// Claude config.
	if cfg.Claude.Binary != "/usr/local/bin/claude" {
		t.Errorf("claude.binary = %q", cfg.Claude.Binary)
	}
	if cfg.Claude.Model != "claude-sonnet-4-20250514" {
		t.Errorf("claude.model = %q", cfg.Claude.Model)
	}
	if cfg.Claude.AppendSystemPrompt != "Be concise" {
		t.Errorf("claude.append_system_prompt = %q", cfg.Claude.AppendSystemPrompt)
	}
	if len(cfg.Claude.ExtraArgs) != 2 || cfg.Claude.ExtraArgs[0] != "--max-turns" {
		t.Errorf("claude.extra_args = %v", cfg.Claude.ExtraArgs)
	}

	// Per-channel backend.
	if cfg.Channels[0].Backend != "" {
		t.Errorf("channels[0].backend = %q, want empty", cfg.Channels[0].Backend)
	}
	if cfg.Channels[1].Backend != "claude" {
		t.Errorf("channels[1].backend = %q, want claude", cfg.Channels[1].Backend)
	}
	if cfg.Channels[1].Model != "claude-sonnet-4-20250514" {
		t.Errorf("channels[1].model = %q", cfg.Channels[1].Model)
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
