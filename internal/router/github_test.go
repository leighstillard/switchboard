package router

import (
	"strings"
	"testing"
)

func TestFormatGitHubIssueOpened(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "issues",
		Payload: map[string]interface{}{
			"action": "opened",
			"issue": map[string]interface{}{
				"title":    "Fix broken deploy pipeline",
				"number":   float64(42),
				"html_url": "https://github.com/format5/infra/issues/42",
				"body":     "The deploy pipeline fails when the config file is missing.\n\nSteps to reproduce:\n1. Remove config\n2. Run deploy",
				"user": map[string]interface{}{
					"login": "leigh",
				},
				"labels": []interface{}{
					map[string]interface{}{"name": "bug"},
					map[string]interface{}{"name": "priority:high"},
				},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "Issue opened")
	assertContains(t, result, "format5/infra")
	assertContains(t, result, "#42")
	assertContains(t, result, "Fix broken deploy pipeline")
	assertContains(t, result, "leigh")
	assertContains(t, result, "deploy pipeline fails")
	assertContains(t, result, "`bug`")
	assertContains(t, result, "`priority:high`")
	assertContains(t, result, "🆕")
}

func TestFormatGitHubIssueClosed(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "issues",
		Payload: map[string]interface{}{
			"action": "closed",
			"issue": map[string]interface{}{
				"title":    "Old bug",
				"number":   float64(10),
				"html_url": "https://github.com/format5/infra/issues/10",
				"user":     map[string]interface{}{"login": "leigh"},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "Issue closed")
	assertContains(t, result, "❌")
	assertContains(t, result, "#10")
}

func TestFormatGitHubPROpened(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "pull_request",
		Payload: map[string]interface{}{
			"action": "opened",
			"pull_request": map[string]interface{}{
				"title":    "Add webhook routing",
				"number":   float64(7),
				"html_url": "https://github.com/format5/switchboard/pull/7",
				"body":     "Implements GitHub webhook routing with intelligent channel mapping.",
				"merged":   false,
				"user":     map[string]interface{}{"login": "leigh"},
				"base":     map[string]interface{}{"ref": "main"},
				"head":     map[string]interface{}{"ref": "feat/webhooks"},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/switchboard",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "PR opened")
	assertContains(t, result, "format5/switchboard")
	assertContains(t, result, "#7")
	assertContains(t, result, "Add webhook routing")
	assertContains(t, result, "`main` <- `feat/webhooks`")
	assertContains(t, result, "🆕")
}

func TestFormatGitHubPRMerged(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "pull_request",
		Payload: map[string]interface{}{
			"action": "closed",
			"pull_request": map[string]interface{}{
				"title":    "Hotfix",
				"number":   float64(8),
				"html_url": "https://github.com/format5/switchboard/pull/8",
				"merged":   true,
				"user":     map[string]interface{}{"login": "leigh"},
				"base":     map[string]interface{}{"ref": "main"},
				"head":     map[string]interface{}{"ref": "hotfix/123"},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/switchboard",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "PR closed")
	assertContains(t, result, "✅") // merged
}

func TestFormatGitHubPRClosedNotMerged(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "pull_request",
		Payload: map[string]interface{}{
			"action": "closed",
			"pull_request": map[string]interface{}{
				"title":    "Bad PR",
				"number":   float64(9),
				"html_url": "https://github.com/format5/switchboard/pull/9",
				"merged":   false,
				"user":     map[string]interface{}{"login": "leigh"},
				"base":     map[string]interface{}{"ref": "main"},
				"head":     map[string]interface{}{"ref": "bad-branch"},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/switchboard",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "PR closed")
	assertContains(t, result, "❌") // not merged
}

func TestFormatGitHubPush(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "push",
		Payload: map[string]interface{}{
			"ref":     "refs/heads/main",
			"compare": "https://github.com/format5/infra/compare/abc...def",
			"forced":  false,
			"pusher":  map[string]interface{}{"name": "leigh"},
			"commits": []interface{}{
				map[string]interface{}{
					"id":      "abc123def456789",
					"message": "fix: handle nil pointer in webhook handler",
				},
				map[string]interface{}{
					"id":      "def456abc789012",
					"message": "test: add push formatting test",
				},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "Push")
	assertContains(t, result, "`main`")
	assertContains(t, result, "format5/infra")
	assertContains(t, result, "leigh")
	assertContains(t, result, "2 commits")
	assertContains(t, result, "`abc123d`")
	assertContains(t, result, "handle nil pointer")
	assertContains(t, result, "compare")
}

func TestFormatGitHubPushForced(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "push",
		Payload: map[string]interface{}{
			"ref":    "refs/heads/feature",
			"forced": true,
			"pusher": map[string]interface{}{"name": "leigh"},
			"commits": []interface{}{
				map[string]interface{}{
					"id":      "abc123def456789",
					"message": "rebase",
				},
			},
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "Force push")
	assertContains(t, result, "⚠️")
}

func TestFormatGitHubIssueComment(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "issue_comment",
		Payload: map[string]interface{}{
			"action": "created",
			"comment": map[string]interface{}{
				"body":     "I think this needs a different approach.",
				"html_url": "https://github.com/format5/infra/issues/42#issuecomment-1",
				"user":     map[string]interface{}{"login": "reviewer"},
			},
			"issue": map[string]interface{}{
				"title":  "Fix broken deploy pipeline",
				"number": float64(42),
			},
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "💬")
	assertContains(t, result, "Comment")
	assertContains(t, result, "#42")
	assertContains(t, result, "reviewer")
	assertContains(t, result, "different approach")
}

func TestFormatGitHubGenericEvent(t *testing.T) {
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "deployment_status",
		Payload: map[string]interface{}{
			"action": "completed",
			"repository": map[string]interface{}{
				"full_name": "format5/infra",
			},
		},
	}

	result := formatGitHubWebhook(evt)

	assertContains(t, result, "deployment_status")
	assertContains(t, result, "format5/infra")
}

func TestFormatGitHubMissingFields(t *testing.T) {
	// Should not panic with minimal/empty payload.
	evt := &WebhookEvent{
		Source:    "github",
		EventType: "issues",
		Payload:   map[string]interface{}{"action": "opened"},
	}

	result := formatGitHubWebhook(evt)
	if result == "" {
		t.Error("expected non-empty result for minimal payload")
	}
}

func TestEscMrkdwn(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"<script>alert</script>", "&lt;script&gt;alert&lt;/script&gt;"},
		{"a & b", "a &amp; b"},
		{"a < b > c", "a &lt; b &gt; c"},
	}

	for _, tt := range tests {
		got := escMrkdwn(tt.input)
		if got != tt.want {
			t.Errorf("escMrkdwn(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTruncateText(t *testing.T) {
	short := "hello"
	if got := truncateText(short, 10); got != "hello" {
		t.Errorf("truncateText(%q, 10) = %q", short, got)
	}

	long := strings.Repeat("x", 500)
	got := truncateText(long, 100)
	if len(got) != 103 { // 100 + "..."
		t.Errorf("truncateText(500 chars, 100) len = %d, want 103", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("expected ... suffix")
	}
}

func TestGhActionEmoji(t *testing.T) {
	tests := []struct {
		action string
		merged bool
		want   string
	}{
		{"opened", false, "🆕"},
		{"closed", true, "✅"},
		{"closed", false, "❌"},
		{"reopened", false, "🔄"},
		{"synchronize", false, "🔄"},
		{"unknown_action", false, "📨"},
	}

	for _, tt := range tests {
		got := ghActionEmoji(tt.action, tt.merged)
		if got != tt.want {
			t.Errorf("ghActionEmoji(%q, %v) = %q, want %q", tt.action, tt.merged, got, tt.want)
		}
	}
}

func TestGhHelpers(t *testing.T) {
	m := map[string]interface{}{
		"str":  "hello",
		"num":  float64(42),
		"bool": true,
		"nested": map[string]interface{}{
			"inner": "value",
		},
		"list": []interface{}{"a", "b"},
	}

	if got := ghString(m, "str"); got != "hello" {
		t.Errorf("ghString str = %q", got)
	}
	if got := ghString(m, "missing"); got != "" {
		t.Errorf("ghString missing = %q", got)
	}
	if got := ghString(nil, "str"); got != "" {
		t.Errorf("ghString nil = %q", got)
	}
	if got := ghNumber(m, "num"); got != 42 {
		t.Errorf("ghNumber num = %d", got)
	}
	if got := ghNumber(m, "missing"); got != 0 {
		t.Errorf("ghNumber missing = %d", got)
	}
	if got := ghBool(m, "bool"); !got {
		t.Error("ghBool bool should be true")
	}
	if got := ghBool(m, "missing"); got {
		t.Error("ghBool missing should be false")
	}
	if got := ghMap(m, "nested"); got == nil || got["inner"] != "value" {
		t.Errorf("ghMap nested = %v", got)
	}
	if got := ghMap(m, "missing"); got != nil {
		t.Errorf("ghMap missing = %v", got)
	}
	if got := ghSlice(m, "list"); len(got) != 2 {
		t.Errorf("ghSlice list len = %d", len(got))
	}
}

// assertContains is a test helper that checks if s contains substr.
func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
