// Package llmrouter provides LLM-based notification routing for webhook events
// that don't match any deterministic rule.
package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config holds the LLM router settings from [routing.llm] in the TOML config.
type Config struct {
	Enabled             bool    `toml:"enabled"`
	Model               string  `toml:"model"`
	ConfidenceThreshold int     `toml:"confidence_threshold"`
	MaxInputTokens      int     `toml:"max_input_tokens"`
	IncludeThreadCount  int     `toml:"include_thread_count"`
	APIKey              string  `toml:"api_key"`
	MonthlyBudgetUSD    float64 `toml:"monthly_budget_usd"`
}

// DefaultConfig returns sensible defaults for the LLM router.
func DefaultConfig() Config {
	return Config{
		Enabled:             false,
		Model:               "claude-haiku-4-5",
		ConfidenceThreshold: 80,
		MaxInputTokens:      4000,
		IncludeThreadCount:  30,
		MonthlyBudgetUSD:    5.0,
	}
}

// ---------------------------------------------------------------------------
// Thread context (input to the LLM)
// ---------------------------------------------------------------------------

// ThreadContext describes a recently active thread for the LLM prompt.
type ThreadContext struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ThreadTS    string `json:"thread_ts"`
	Topic       string `json:"topic"`        // first message excerpt
	Workdir     string `json:"workdir"`      // session workdir
	LastActive  string `json:"last_active"`  // human-readable timestamp
}

// WebhookSummary is the redacted event data passed to the LLM.
type WebhookSummary struct {
	Source    string `json:"source"`
	EventType string `json:"event_type"`
	Summary   string `json:"summary"` // redacted payload summary
}

// ---------------------------------------------------------------------------
// Decision (output from the LLM)
// ---------------------------------------------------------------------------

// Decision is the parsed LLM routing decision.
type Decision struct {
	ThreadID   *string `json:"thread_id"`  // "channel_id:thread_ts" or null
	Confidence int     `json:"confidence"` // 0-100
	Reasoning  string  `json:"reasoning"`
}

// ---------------------------------------------------------------------------
// Cost tracker
// ---------------------------------------------------------------------------

// CostTracker monitors cumulative API spend to enforce budget guardrails.
type CostTracker struct {
	mu         sync.Mutex
	totalCost  float64
	resetAt    time.Time
	budgetUSD  float64
}

// NewCostTracker creates a cost tracker with the given monthly budget.
func NewCostTracker(budgetUSD float64) *CostTracker {
	now := time.Now()
	// Reset at the start of next month.
	resetAt := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	return &CostTracker{
		budgetUSD: budgetUSD,
		resetAt:   resetAt,
	}
}

// Add records a cost. Returns true if within budget, false if over.
func (ct *CostTracker) Add(cost float64) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Reset if we've passed the reset time.
	if time.Now().After(ct.resetAt) {
		ct.totalCost = 0
		now := time.Now()
		ct.resetAt = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	}

	ct.totalCost += cost
	return ct.totalCost <= ct.budgetUSD
}

// OverBudget returns true if the current period's spending exceeds budget.
func (ct *CostTracker) OverBudget() bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if time.Now().After(ct.resetAt) {
		return false // new period
	}
	return ct.totalCost > ct.budgetUSD
}

// TotalCost returns the current period's spending.
func (ct *CostTracker) TotalCost() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.totalCost
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// Router is the LLM-based notification router.
type Router struct {
	cfg         Config
	httpClient  *http.Client
	costTracker *CostTracker
}

// New creates a new LLM router.
func New(cfg Config) *Router {
	return &Router{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		costTracker: NewCostTracker(cfg.MonthlyBudgetUSD),
	}
}

// Route sends a webhook event through the LLM router and returns a routing decision.
// It returns (nil, nil) if the router is disabled or over budget.
func (r *Router) Route(ctx context.Context, event WebhookSummary, threads []ThreadContext) (*Decision, error) {
	if !r.cfg.Enabled {
		return nil, nil
	}

	if r.cfg.APIKey == "" {
		slog.Warn("llmrouter: API key not configured")
		return nil, nil
	}

	if r.costTracker.OverBudget() {
		slog.Warn("llmrouter: over monthly budget, degrading to fallback",
			"total_cost", r.costTracker.TotalCost(),
			"budget", r.cfg.MonthlyBudgetUSD,
		)
		return nil, nil
	}

	// Build the prompt.
	prompt := r.buildPrompt(event, threads)

	// Call the Anthropic API.
	response, err := r.callAnthropic(ctx, prompt)
	if err != nil {
		slog.Error("llmrouter: API call failed", "error", err)
		return nil, err
	}

	// Parse the decision.
	decision, err := parseDecision(response)
	if err != nil {
		slog.Warn("llmrouter: failed to parse LLM response", "error", err, "response", response)
		return nil, err
	}

	// Estimate cost (rough: ~0.25/MTok input, ~1.25/MTok output for Haiku).
	estimatedCost := float64(len(prompt))/4000.0*0.00025 + float64(len(response))/4000.0*0.00125
	r.costTracker.Add(estimatedCost)

	return decision, nil
}

// MeetsThreshold returns true if the decision's confidence meets the configured threshold.
func (r *Router) MeetsThreshold(d *Decision) bool {
	if d == nil {
		return false
	}
	return d.Confidence >= r.cfg.ConfidenceThreshold
}

// OverBudget exposes the cost tracker's over-budget status.
func (r *Router) OverBudget() bool {
	return r.costTracker.OverBudget()
}

// ---------------------------------------------------------------------------
// Prompt construction
// ---------------------------------------------------------------------------

func (r *Router) buildPrompt(event WebhookSummary, threads []ThreadContext) string {
	// Build prompt in 3 parts: prefix (event details), middle (thread list),
	// suffix (instructions). If truncation is needed, only the middle section
	// is trimmed so that the JSON response format instructions are preserved.
	var prefix, middle, suffix strings.Builder

	prefix.WriteString("You are a notification router. Given an incoming webhook event and a list of active Slack threads, determine which thread (if any) this event is most likely associated with.\n\n")

	prefix.WriteString("## Incoming Event\n")
	prefix.WriteString(fmt.Sprintf("Source: %s\n", event.Source))
	prefix.WriteString(fmt.Sprintf("Type: %s\n", event.EventType))
	prefix.WriteString(fmt.Sprintf("Summary: %s\n\n", event.Summary))

	middle.WriteString("## Active Threads (last 24 hours)\n")
	if len(threads) == 0 {
		middle.WriteString("(no active threads)\n\n")
	} else {
		limit := r.cfg.IncludeThreadCount
		if limit == 0 {
			limit = 30
		}
		if limit > len(threads) {
			limit = len(threads)
		}
		for i, t := range threads[:limit] {
			middle.WriteString(fmt.Sprintf("%d. channel_id=%s channel=%s thread=%s topic=%q workdir=%s last_active=%s\n",
				i+1, t.ChannelID, t.ChannelName, t.ThreadTS, t.Topic, t.Workdir, t.LastActive))
		}
		middle.WriteString("\n")
	}

	suffix.WriteString("## Instructions\n")
	suffix.WriteString("Which thread is this event most likely associated with? Consider:\n")
	suffix.WriteString("- Does the event source/type match the workdir or topic of any thread?\n")
	suffix.WriteString("- Is there a repo name, project name, or keyword match?\n")
	suffix.WriteString("- If no thread is a good match, say so.\n\n")
	suffix.WriteString("Respond as JSON only, no other text:\n")
	suffix.WriteString(`{"thread_id": "channel_id:thread_ts", "confidence": 0-100, "reasoning": "brief explanation"}`)
	suffix.WriteString("\n\nIf no thread is a good match:\n")
	suffix.WriteString(`{"thread_id": null, "confidence": 0, "reasoning": "brief explanation"}`)

	prefixStr := prefix.String()
	middleStr := middle.String()
	suffixStr := suffix.String()

	// Enforce max_input_tokens as a safety limit (rough estimate: 4 chars/token).
	// Only truncate the middle (thread list) section to preserve instructions.
	if r.cfg.MaxInputTokens > 0 {
		maxChars := r.cfg.MaxInputTokens * 4
		totalLen := len(prefixStr) + len(middleStr) + len(suffixStr)
		if totalLen > maxChars {
			allowedMiddle := maxChars - len(prefixStr) - len(suffixStr)
			if allowedMiddle < 0 {
				allowedMiddle = 0
			}
			if allowedMiddle < len(middleStr) {
				middleStr = middleStr[:allowedMiddle]
				slog.Warn("llmrouter: thread context truncated to fit max_input_tokens",
					"max_tokens", r.cfg.MaxInputTokens, "chars", maxChars)
			}
		}
	}

	return prefixStr + middleStr + suffixStr
}

// ---------------------------------------------------------------------------
// Anthropic API call
// ---------------------------------------------------------------------------

// AnthropicRequest is the request body for the Anthropic Messages API.
type AnthropicRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Messages  []AnthropicMsg    `json:"messages"`
}

// AnthropicMsg is a single message in the Anthropic API format.
type AnthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicResponse is the response from the Anthropic Messages API.
type AnthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r *Router) callAnthropic(ctx context.Context, prompt string) (string, error) {
	model := r.cfg.Model
	if model == "" {
		model = "claude-haiku-4-5"
	}

	reqBody := AnthropicRequest{
		Model:     model,
		MaxTokens: 200,
		Messages: []AnthropicMsg{
			{Role: "user", Content: prompt},
		},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", r.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	var apiResp AnthropicResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if apiResp.Error != nil {
		return "", fmt.Errorf("API error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
	}

	if len(apiResp.Content) == 0 {
		return "", fmt.Errorf("empty response content")
	}

	return apiResp.Content[0].Text, nil
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

func parseDecision(response string) (*Decision, error) {
	// Try to extract JSON from the response (the LLM might wrap it in text).
	response = strings.TrimSpace(response)

	// Find JSON in the response.
	start := strings.Index(response, "{")
	end := strings.LastIndex(response, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response: %q", response)
	}

	jsonStr := response[start : end+1]

	var d Decision
	if err := json.Unmarshal([]byte(jsonStr), &d); err != nil {
		return nil, fmt.Errorf("parse decision JSON: %w", err)
	}

	// Validate confidence range.
	if d.Confidence < 0 {
		d.Confidence = 0
	}
	if d.Confidence > 100 {
		d.Confidence = 100
	}

	return &d, nil
}
