// Package router - GitHub webhook formatting and routing.
package router

import (
	"fmt"
	"strings"
)

// formatGitHubWebhook renders a GitHub webhook event as Slack mrkdwn.
func formatGitHubWebhook(evt *WebhookEvent) string {
	switch evt.EventType {
	case "issues":
		return formatGitHubIssue(evt)
	case "pull_request":
		return formatGitHubPR(evt)
	case "push":
		return formatGitHubPush(evt)
	case "issue_comment":
		return formatGitHubIssueComment(evt)
	case "pull_request_review_comment", "pull_request_review":
		return formatGitHubPRComment(evt)
	default:
		return formatGitHubGeneric(evt)
	}
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func formatGitHubIssue(evt *WebhookEvent) string {
	action := ghString(evt.Payload, "action")
	issue := ghMap(evt.Payload, "issue")
	repo := ghRepoName(evt.Payload)

	title := ghString(issue, "title")
	number := ghNumber(issue, "number")
	url := ghString(issue, "html_url")
	author := ghString(ghMap(issue, "user"), "login")
	body := ghString(issue, "body")

	emoji := ghActionEmoji(action, false)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s *Issue %s* in `%s`\n", emoji, action, repo))
	sb.WriteString(fmt.Sprintf("<%s|#%d %s>", url, number, escMrkdwn(title)))

	if author != "" {
		sb.WriteString(fmt.Sprintf(" by `%s`", author))
	}
	sb.WriteString("\n")

	if body != "" && (action == "opened" || action == "edited") {
		sb.WriteString(truncateText(body, 300))
		sb.WriteString("\n")
	}

	// Labels on opened/labeled.
	if action == "opened" || action == "labeled" {
		if labels := ghLabels(issue); labels != "" {
			sb.WriteString(fmt.Sprintf("Labels: %s\n", labels))
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Pull Requests
// ---------------------------------------------------------------------------

func formatGitHubPR(evt *WebhookEvent) string {
	action := ghString(evt.Payload, "action")
	pr := ghMap(evt.Payload, "pull_request")
	repo := ghRepoName(evt.Payload)

	title := ghString(pr, "title")
	number := ghNumber(pr, "number")
	url := ghString(pr, "html_url")
	author := ghString(ghMap(pr, "user"), "login")
	body := ghString(pr, "body")
	base := ghString(ghMap(pr, "base"), "ref")
	head := ghString(ghMap(pr, "head"), "ref")
	merged := ghBool(pr, "merged")

	emoji := ghActionEmoji(action, merged)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s *PR %s* in `%s`\n", emoji, action, repo))
	sb.WriteString(fmt.Sprintf("<%s|#%d %s>", url, number, escMrkdwn(title)))

	if author != "" {
		sb.WriteString(fmt.Sprintf(" by `%s`", author))
	}
	sb.WriteString("\n")

	if base != "" && head != "" {
		sb.WriteString(fmt.Sprintf("`%s` <- `%s`\n", base, head))
	}

	if body != "" && (action == "opened" || action == "edited") {
		sb.WriteString(truncateText(body, 300))
		sb.WriteString("\n")
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Push
// ---------------------------------------------------------------------------

func formatGitHubPush(evt *WebhookEvent) string {
	repo := ghRepoName(evt.Payload)
	pusher := ghString(ghMap(evt.Payload, "pusher"), "name")
	ref := ghString(evt.Payload, "ref")
	branch := strings.TrimPrefix(ref, "refs/heads/")
	compareURL := ghString(evt.Payload, "compare")

	commits := ghSlice(evt.Payload, "commits")
	forced := ghBool(evt.Payload, "forced")

	var sb strings.Builder
	if forced {
		sb.WriteString(fmt.Sprintf("⚠️ *Force push* to `%s` in `%s`", branch, repo))
	} else {
		sb.WriteString(fmt.Sprintf("📦 *Push* to `%s` in `%s`", branch, repo))
	}

	if pusher != "" {
		sb.WriteString(fmt.Sprintf(" by `%s`", pusher))
	}

	if len(commits) > 0 && compareURL != "" {
		sb.WriteString(fmt.Sprintf(" (%d commits) <%s|compare>", len(commits), compareURL))
	} else if len(commits) > 0 {
		sb.WriteString(fmt.Sprintf(" (%d commits)", len(commits)))
	}
	sb.WriteString("\n")

	// Show first 3 commits.
	for i, c := range commits {
		if i >= 3 {
			sb.WriteString(fmt.Sprintf("  ... and %d more\n", len(commits)-3))
			break
		}
		cm := toMap(c)
		sha := ghString(cm, "id")
		msg := ghString(cm, "message")
		// Truncate SHA and take first line of message.
		if len(sha) > 7 {
			sha = sha[:7]
		}
		if idx := strings.IndexByte(msg, '\n'); idx > 0 {
			msg = msg[:idx]
		}
		sb.WriteString(fmt.Sprintf("  `%s` %s\n", sha, escMrkdwn(truncateText(msg, 80))))
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func formatGitHubIssueComment(evt *WebhookEvent) string {
	action := ghString(evt.Payload, "action")
	if action != "created" && action != "edited" {
		return formatGitHubGeneric(evt)
	}

	comment := ghMap(evt.Payload, "comment")
	issue := ghMap(evt.Payload, "issue")
	repo := ghRepoName(evt.Payload)

	author := ghString(ghMap(comment, "user"), "login")
	body := ghString(comment, "body")
	url := ghString(comment, "html_url")
	issueTitle := ghString(issue, "title")
	issueNumber := ghNumber(issue, "number")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💬 *Comment* on <%s|#%d %s> in `%s`", url, issueNumber, escMrkdwn(issueTitle), repo))
	if author != "" {
		sb.WriteString(fmt.Sprintf(" by `%s`", author))
	}
	sb.WriteString("\n")

	if body != "" {
		sb.WriteString(truncateText(body, 300))
		sb.WriteString("\n")
	}

	return sb.String()
}

func formatGitHubPRComment(evt *WebhookEvent) string {
	action := ghString(evt.Payload, "action")
	comment := ghMap(evt.Payload, "comment")
	pr := ghMap(evt.Payload, "pull_request")
	repo := ghRepoName(evt.Payload)

	author := ghString(ghMap(comment, "user"), "login")
	body := ghString(comment, "body")
	url := ghString(comment, "html_url")
	prTitle := ghString(pr, "title")
	prNumber := ghNumber(pr, "number")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("💬 *Review %s* on <%s|#%d %s> in `%s`", action, url, prNumber, escMrkdwn(prTitle), repo))
	if author != "" {
		sb.WriteString(fmt.Sprintf(" by `%s`", author))
	}
	sb.WriteString("\n")

	if body != "" {
		sb.WriteString(truncateText(body, 300))
		sb.WriteString("\n")
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Generic fallback
// ---------------------------------------------------------------------------

func formatGitHubGeneric(evt *WebhookEvent) string {
	repo := ghRepoName(evt.Payload)
	action := ghString(evt.Payload, "action")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📨 *GitHub %s*", evt.EventType))
	if action != "" {
		sb.WriteString(fmt.Sprintf(" (%s)", action))
	}
	if repo != "" {
		sb.WriteString(fmt.Sprintf(" in `%s`", repo))
	}
	sb.WriteString("\n")

	// Show truncated payload for unknown events.
	summary := truncateJSON(evt.Payload, 500)
	if summary != "" {
		sb.WriteString("```\n")
		sb.WriteString(summary)
		sb.WriteString("\n```\n")
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ghActionEmoji returns an appropriate emoji for a GitHub action.
func ghActionEmoji(action string, merged bool) string {
	switch action {
	case "opened":
		return "🆕"
	case "closed":
		if merged {
			return "✅"
		}
		return "❌"
	case "reopened":
		return "🔄"
	case "synchronize":
		return "🔄"
	case "edited":
		return "✏️"
	case "labeled":
		return "🏷️"
	case "assigned":
		return "👤"
	case "review_requested":
		return "👀"
	default:
		return "📨"
	}
}

// ghRepoName extracts repository.full_name from a GitHub webhook payload.
func ghRepoName(payload map[string]interface{}) string {
	return ghString(ghMap(payload, "repository"), "full_name")
}

// ghString safely extracts a string field from a map.
func ghString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// ghNumber safely extracts an integer from a map (JSON numbers are float64).
func ghNumber(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return 0
}

// ghBool safely extracts a bool from a map.
func ghBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// ghMap safely extracts a nested map from a map.
func ghMap(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if sub, ok := v.(map[string]interface{}); ok {
		return sub
	}
	return nil
}

// ghSlice safely extracts a slice from a map.
func ghSlice(m map[string]interface{}, key string) []interface{} {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// ghLabels formats issue/PR labels as a comma-separated string.
func ghLabels(issue map[string]interface{}) string {
	labels := ghSlice(issue, "labels")
	if len(labels) == 0 {
		return ""
	}
	var names []string
	for _, l := range labels {
		if lm := toMap(l); lm != nil {
			if name := ghString(lm, "name"); name != "" {
				names = append(names, "`"+name+"`")
			}
		}
	}
	return strings.Join(names, ", ")
}

// toMap converts an interface{} to map[string]interface{}.
func toMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// escMrkdwn escapes Slack mrkdwn special characters in user-provided text.
func escMrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncateText truncates a string to maxLen characters with an ellipsis.
func truncateText(s string, maxLen int) string {
	// Take first line if multi-line and short enough.
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
