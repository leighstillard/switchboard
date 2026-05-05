package render

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// Plan directive
// ---------------------------------------------------------------------------

// PlanDirective is the typed representation of a `render: plan` directive.
type PlanDirective struct {
	Render  string     `json:"render"`
	Version int        `json:"version,omitempty"`
	Title   string     `json:"title"`
	Tasks   []PlanTask `json:"tasks"`
}

// PlanTask is a single task within a plan.
type PlanTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // "complete", "in_progress", "pending"
}

func renderPlanDirective(data []byte) ([]map[string]interface{}, string, error) {
	var p PlanDirective
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, "", fmt.Errorf("plan: %w", err)
	}
	if p.Title == "" {
		return nil, "", fmt.Errorf("plan: missing 'title' field")
	}
	if len(p.Tasks) == 0 {
		return nil, "", fmt.Errorf("plan: missing 'tasks' field")
	}

	// Build blocks
	blocks := []map[string]interface{}{
		{
			"type": "header",
			"text": map[string]interface{}{
				"type": "plain_text",
				"text": "📋 " + p.Title,
			},
		},
	}

	// Build task list as a section with mrkdwn
	var taskLines []string
	complete, total := 0, len(p.Tasks)
	for _, t := range p.Tasks {
		var icon string
		switch t.Status {
		case "complete":
			icon = "✅"
			complete++
		case "in_progress":
			icon = "⏳"
		default:
			icon = "⬜"
		}
		taskLines = append(taskLines, fmt.Sprintf("%s %s", icon, t.Title))
	}

	blocks = append(blocks, map[string]interface{}{
		"type": "section",
		"text": map[string]interface{}{
			"type": "mrkdwn",
			"text": strings.Join(taskLines, "\n"),
		},
	})

	// Context with progress
	blocks = append(blocks, map[string]interface{}{
		"type": "context",
		"elements": []map[string]interface{}{
			{
				"type": "mrkdwn",
				"text": fmt.Sprintf("Progress: %d/%d complete", complete, total),
			},
		},
	})

	fallback := fmt.Sprintf("Plan: %s (%d/%d tasks complete)", p.Title, complete, total)
	return blocks, fallback, nil
}

// ---------------------------------------------------------------------------
// Brief directive
// ---------------------------------------------------------------------------

// BriefDirective is the typed representation of a `render: brief` directive.
type BriefDirective struct {
	Render  string        `json:"render"`
	Version int           `json:"version,omitempty"`
	Title   string        `json:"title"`
	Summary string        `json:"summary"`
	Sources []BriefSource `json:"sources,omitempty"`
}

// BriefSource is a citation in a brief.
type BriefSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Excerpt string `json:"excerpt,omitempty"`
}

func renderBriefDirective(data []byte) ([]map[string]interface{}, string, error) {
	var b BriefDirective
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, "", fmt.Errorf("brief: %w", err)
	}
	if b.Title == "" {
		return nil, "", fmt.Errorf("brief: missing 'title' field")
	}
	if b.Summary == "" {
		return nil, "", fmt.Errorf("brief: missing 'summary' field")
	}

	blocks := []map[string]interface{}{
		{
			"type": "header",
			"text": map[string]interface{}{
				"type": "plain_text",
				"text": "📝 " + b.Title,
			},
		},
		{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": b.Summary,
			},
		},
	}

	// Add sources if present
	if len(b.Sources) > 0 {
		blocks = append(blocks, map[string]interface{}{
			"type": "divider",
		})

		var sourceLines []string
		for i, src := range b.Sources {
			line := fmt.Sprintf("%d. ", i+1)
			if src.URL != "" {
				line += fmt.Sprintf("<%s|%s>", src.URL, src.Title)
			} else {
				line += src.Title
			}
			if src.Excerpt != "" {
				line += "\n    _" + truncateStr(src.Excerpt, 100) + "_"
			}
			sourceLines = append(sourceLines, line)
		}

		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": "*Sources:*\n" + strings.Join(sourceLines, "\n"),
			},
		})
	}

	fallback := fmt.Sprintf("Brief: %s — %s", b.Title, truncateStr(b.Summary, 80))
	return blocks, fallback, nil
}

// ---------------------------------------------------------------------------
// Poll directive
// ---------------------------------------------------------------------------

// PollDirective is the typed representation of a `render: poll` directive.
type PollDirective struct {
	Render   string       `json:"render"`
	Version  int          `json:"version,omitempty"`
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
}

// PollOption is a single option in a poll.
type PollOption struct {
	Text string `json:"text"`
	ID   string `json:"id,omitempty"`
}

func renderPollDirective(data []byte) ([]map[string]interface{}, string, error) {
	var p PollDirective
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, "", fmt.Errorf("poll: %w", err)
	}
	if p.Question == "" {
		return nil, "", fmt.Errorf("poll: missing 'question' field")
	}
	if len(p.Options) == 0 {
		return nil, "", fmt.Errorf("poll: missing 'options' field")
	}

	blocks := []map[string]interface{}{
		{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": "📊 *" + p.Question + "*",
			},
		},
		{"type": "divider"},
	}

	for i, opt := range p.Options {
		emoji := string(rune('🅰' + i)) // 🅰, 🅱, etc. - fallback to numbers
		if i >= 2 {
			emoji = fmt.Sprintf("%d️⃣", i+1)
		}
		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": fmt.Sprintf("%s %s", emoji, opt.Text),
			},
		})
	}

	fallback := fmt.Sprintf("Poll: %s (%d options)", p.Question, len(p.Options))
	return blocks, fallback, nil
}

// ---------------------------------------------------------------------------
// Tickets directive
// ---------------------------------------------------------------------------

// TicketsDirective is the typed representation of a `render: tickets` directive.
type TicketsDirective struct {
	Render  string   `json:"render"`
	Version int      `json:"version,omitempty"`
	Title   string   `json:"title,omitempty"`
	Tickets []Ticket `json:"tickets"`
}

// Ticket is a single ticket/issue.
type Ticket struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Assignee string `json:"assignee,omitempty"`
	Priority string `json:"priority,omitempty"`
	URL      string `json:"url,omitempty"`
}

func renderTicketsDirective(data []byte) ([]map[string]interface{}, string, error) {
	var t TicketsDirective
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, "", fmt.Errorf("tickets: %w", err)
	}
	if len(t.Tickets) == 0 {
		return nil, "", fmt.Errorf("tickets: missing 'tickets' field")
	}

	var blocks []map[string]interface{}

	if t.Title != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "header",
			"text": map[string]interface{}{
				"type": "plain_text",
				"text": "🎫 " + t.Title,
			},
		})
	}

	for _, ticket := range t.Tickets {
		var icon string
		switch ticket.Status {
		case "open":
			icon = "🟢"
		case "in_progress", "in-progress":
			icon = "🔵"
		case "closed", "done":
			icon = "✅"
		case "blocked":
			icon = "🔴"
		default:
			icon = "⚪"
		}

		titleText := fmt.Sprintf("%s *%s*", icon, ticket.Title)
		if ticket.URL != "" {
			titleText = fmt.Sprintf("%s *<%s|%s>*", icon, ticket.URL, ticket.Title)
		}

		var meta []string
		if ticket.ID != "" {
			meta = append(meta, ticket.ID)
		}
		if ticket.Status != "" {
			meta = append(meta, ticket.Status)
		}
		if ticket.Assignee != "" {
			meta = append(meta, "@"+ticket.Assignee)
		}
		if ticket.Priority != "" {
			meta = append(meta, "P:"+ticket.Priority)
		}

		block := map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": titleText + "\n" + strings.Join(meta, " · "),
			},
		}
		blocks = append(blocks, block)
	}

	fallback := fmt.Sprintf("Tickets: %d items", len(t.Tickets))
	if t.Title != "" {
		fallback = fmt.Sprintf("Tickets: %s (%d items)", t.Title, len(t.Tickets))
	}
	return blocks, fallback, nil
}

// ---------------------------------------------------------------------------
// Todos directive
// ---------------------------------------------------------------------------

// TodosDirective is the typed representation of a `render: todos` directive.
type TodosDirective struct {
	Render string     `json:"render"`
	Version int       `json:"version,omitempty"`
	Title  string     `json:"title,omitempty"`
	Items  []TodoItem `json:"items"`
}

// TodoItem is a single todo/task item.
type TodoItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func renderTodosDirective(data []byte) ([]map[string]interface{}, string, error) {
	var t TodosDirective
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, "", fmt.Errorf("todos: %w", err)
	}
	if len(t.Items) == 0 {
		return nil, "", fmt.Errorf("todos: missing 'items' field")
	}

	var blocks []map[string]interface{}

	title := t.Title
	if title == "" {
		title = "To-do"
	}

	blocks = append(blocks, map[string]interface{}{
		"type": "header",
		"text": map[string]interface{}{
			"type": "plain_text",
			"text": "☑️ " + title,
		},
	})

	var lines []string
	done, total := 0, len(t.Items)
	for _, item := range t.Items {
		if item.Done {
			lines = append(lines, "✅ ~"+item.Text+"~")
			done++
		} else {
			lines = append(lines, "⬜ "+item.Text)
		}
	}

	blocks = append(blocks, map[string]interface{}{
		"type": "section",
		"text": map[string]interface{}{
			"type": "mrkdwn",
			"text": strings.Join(lines, "\n"),
		},
	})

	blocks = append(blocks, map[string]interface{}{
		"type": "context",
		"elements": []map[string]interface{}{
			{
				"type": "mrkdwn",
				"text": fmt.Sprintf("%d/%d complete", done, total),
			},
		},
	})

	fallback := fmt.Sprintf("Todos: %s (%d/%d done)", title, done, total)
	return blocks, fallback, nil
}

// ---------------------------------------------------------------------------
// Alert block renderer (used by coalescer for errors/warnings)
// ---------------------------------------------------------------------------

// AlertLevel specifies the severity of an alert.
type AlertLevel string

const (
	AlertSuccess AlertLevel = "success"
	AlertWarning AlertLevel = "warning"
	AlertError   AlertLevel = "error"
)

// RenderAlert produces Block Kit blocks for an alert message.
func RenderAlert(level AlertLevel, message string) []map[string]interface{} {
	var emoji string
	switch level {
	case AlertSuccess:
		emoji = "✅"
	case AlertWarning:
		emoji = "⚠️"
	case AlertError:
		emoji = "❌"
	default:
		emoji = "ℹ️"
	}

	return []map[string]interface{}{
		{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": fmt.Sprintf("%s *%s*\n%s", emoji, strings.Title(string(level)), message),
			},
		},
	}
}

// AlertFallbackText returns the plain-text fallback for an alert.
func AlertFallbackText(level AlertLevel, message string) string {
	return fmt.Sprintf("[%s] %s", strings.ToUpper(string(level)), truncateStr(message, 100))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// hostnameFromURL extracts just the hostname from a URL string.
func hostnameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
