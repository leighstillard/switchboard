package llmrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Decision parsing tests
// ---------------------------------------------------------------------------

func TestParseDecision_ValidMatch(t *testing.T) {
	response := `{"thread_id": "C123:1234567890.123456", "confidence": 85, "reasoning": "GitHub PR matches the repo in thread"}`
	d, err := parseDecision(response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ThreadID == nil || *d.ThreadID != "C123:1234567890.123456" {
		t.Errorf("thread_id = %v, want C123:1234567890.123456", d.ThreadID)
	}
	if d.Confidence != 85 {
		t.Errorf("confidence = %d, want 85", d.Confidence)
	}
	if d.Reasoning == "" {
		t.Error("reasoning should not be empty")
	}
}

func TestParseDecision_NoMatch(t *testing.T) {
	response := `{"thread_id": null, "confidence": 0, "reasoning": "No thread relates to this deployment event"}`
	d, err := parseDecision(response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ThreadID != nil {
		t.Errorf("thread_id = %v, want nil", d.ThreadID)
	}
	if d.Confidence != 0 {
		t.Errorf("confidence = %d, want 0", d.Confidence)
	}
}

func TestParseDecision_WrappedInText(t *testing.T) {
	response := `Here's my analysis:
{"thread_id": "C456:999.888", "confidence": 72, "reasoning": "weak match"}
That's my best guess.`
	d, err := parseDecision(response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.ThreadID == nil || *d.ThreadID != "C456:999.888" {
		t.Errorf("thread_id = %v, want C456:999.888", d.ThreadID)
	}
	if d.Confidence != 72 {
		t.Errorf("confidence = %d, want 72", d.Confidence)
	}
}

func TestParseDecision_NoJSON(t *testing.T) {
	response := "I don't know how to handle this."
	_, err := parseDecision(response)
	if err == nil {
		t.Error("expected error for non-JSON response")
	}
}

func TestParseDecision_InvalidJSON(t *testing.T) {
	response := `{thread_id: invalid}`
	_, err := parseDecision(response)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseDecision_ConfidenceClamped(t *testing.T) {
	response := `{"thread_id": null, "confidence": -5, "reasoning": "negative"}`
	d, err := parseDecision(response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Confidence != 0 {
		t.Errorf("confidence = %d, want 0 (clamped)", d.Confidence)
	}

	response = `{"thread_id": null, "confidence": 150, "reasoning": "over"}`
	d, err = parseDecision(response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Confidence != 100 {
		t.Errorf("confidence = %d, want 100 (clamped)", d.Confidence)
	}
}

// ---------------------------------------------------------------------------
// Cost tracker tests
// ---------------------------------------------------------------------------

func TestCostTracker_UnderBudget(t *testing.T) {
	ct := NewCostTracker(5.0)
	if ct.OverBudget() {
		t.Error("should not be over budget initially")
	}
	ok := ct.Add(2.0)
	if !ok {
		t.Error("should be within budget at $2")
	}
	ok = ct.Add(2.0)
	if !ok {
		t.Error("should be within budget at $4")
	}
	if ct.OverBudget() {
		t.Error("$4 should be under $5 budget")
	}
}

func TestCostTracker_OverBudget(t *testing.T) {
	ct := NewCostTracker(1.0)
	ct.Add(0.5)
	ct.Add(0.6)
	if !ct.OverBudget() {
		t.Error("$1.10 should be over $1.00 budget")
	}
}

func TestCostTracker_ResetOnNewMonth(t *testing.T) {
	ct := NewCostTracker(1.0)
	ct.Add(2.0) // over budget
	if !ct.OverBudget() {
		t.Error("should be over budget")
	}
	// Simulate new month by setting resetAt to the past.
	ct.mu.Lock()
	ct.resetAt = time.Now().Add(-time.Hour)
	ct.mu.Unlock()

	if ct.OverBudget() {
		t.Error("should reset on new period")
	}
}

// ---------------------------------------------------------------------------
// Threshold tests
// ---------------------------------------------------------------------------

func TestMeetsThreshold(t *testing.T) {
	r := New(Config{ConfidenceThreshold: 80})

	tests := []struct {
		name     string
		decision *Decision
		want     bool
	}{
		{"nil decision", nil, false},
		{"above threshold", &Decision{Confidence: 85}, true},
		{"at threshold", &Decision{Confidence: 80}, true},
		{"below threshold", &Decision{Confidence: 79}, false},
		{"zero", &Decision{Confidence: 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.MeetsThreshold(tt.decision)
			if got != tt.want {
				t.Errorf("MeetsThreshold() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Prompt building tests
// ---------------------------------------------------------------------------

func TestBuildPrompt_ContainsEvent(t *testing.T) {
	r := New(Config{IncludeThreadCount: 5})
	event := WebhookSummary{
		Source:    "github",
		EventType: "push",
		Summary:   "Push to main on leighstillard/switchboard",
	}
	threads := []ThreadContext{
		{ChannelID: "C123", ChannelName: "#dev", ThreadTS: "ts1", Topic: "Working on switchboard", Workdir: "/home/leigh/workspace/switchboard", LastActive: "2m ago"},
		{ChannelID: "C456", ChannelName: "#ops", ThreadTS: "ts2", Topic: "Deploy pipeline", Workdir: "/home/leigh/workspace/deploy", LastActive: "30m ago"},
	}

	prompt := r.buildPrompt(event, threads)

	if !contains(prompt, "github") {
		t.Error("prompt missing source")
	}
	if !contains(prompt, "push") {
		t.Error("prompt missing event type")
	}
	if !contains(prompt, "switchboard") {
		t.Error("prompt missing event summary")
	}
	if !contains(prompt, "#dev") {
		t.Error("prompt missing thread channel")
	}
	if !contains(prompt, "#ops") {
		t.Error("prompt missing second thread")
	}
}

func TestBuildPrompt_NoThreads(t *testing.T) {
	r := New(Config{})
	event := WebhookSummary{Source: "sentry", EventType: "error", Summary: "NPE in auth"}
	prompt := r.buildPrompt(event, nil)
	if !contains(prompt, "no active threads") {
		t.Error("prompt should mention no active threads")
	}
}

func TestBuildPrompt_ThreadLimitRespected(t *testing.T) {
	r := New(Config{IncludeThreadCount: 2})
	threads := make([]ThreadContext, 10)
	for i := range threads {
		threads[i] = ThreadContext{ChannelName: "#ch", ThreadTS: "ts", Topic: "topic"}
	}
	prompt := r.buildPrompt(WebhookSummary{Source: "x", EventType: "y", Summary: "z"}, threads)
	// Should only include 2 threads
	count := 0
	for _, line := range splitLines(prompt) {
		if len(line) > 3 && line[0] >= '0' && line[0] <= '9' {
			count++
		}
	}
	if count > 2 {
		t.Errorf("expected at most 2 threads in prompt, got %d", count)
	}
}

func TestBuildPrompt_TruncationPreservesInstructions(t *testing.T) {
	// Use a very small max_input_tokens to force truncation.
	r := New(Config{
		MaxInputTokens:     100, // 400 chars max - will require truncation
		IncludeThreadCount: 50,
	})

	// Generate many threads to create a large middle section.
	threads := make([]ThreadContext, 20)
	for i := range threads {
		threads[i] = ThreadContext{
			ChannelID:   "C123",
			ChannelName: "#dev",
			ThreadTS:    "ts",
			Topic:       "Long topic description for thread padding",
			Workdir:     "/home/user/workspace/project",
			LastActive:  "5m ago",
		}
	}

	event := WebhookSummary{Source: "github", EventType: "push", Summary: "Push to main"}
	prompt := r.buildPrompt(event, threads)

	// The JSON response format instructions must be preserved even after truncation.
	if !contains(prompt, "Respond as JSON only") {
		t.Error("truncation removed JSON response instructions")
	}
	if !contains(prompt, `"thread_id"`) {
		t.Error("truncation removed JSON format example")
	}
	if !contains(prompt, "## Instructions") {
		t.Error("truncation removed Instructions header")
	}
	// Event details should also be preserved.
	if !contains(prompt, "github") {
		t.Error("truncation removed event source")
	}
}

// ---------------------------------------------------------------------------
// Integration test with mock Anthropic API
// ---------------------------------------------------------------------------

func TestRoute_WithMockAPI(t *testing.T) {
	// Mock Anthropic API server.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request headers.
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing API key header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("missing anthropic-version header")
		}

		resp := map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": `{"thread_id": "C123:ts1", "confidence": 90, "reasoning": "repo matches"}`,
				},
			},
			"usage": map[string]int{
				"input_tokens":  500,
				"output_tokens": 50,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := Config{
		Enabled:             true,
		Model:               "claude-haiku-4-5",
		ConfidenceThreshold: 80,
		IncludeThreadCount:  5,
		APIKey:              "test-key",
		MonthlyBudgetUSD:    5.0,
	}
	r := New(cfg)
	// Override the HTTP client to point to our mock server.
	r.httpClient = server.Client()
	// We also need to override the URL - use a custom transport.
	r.httpClient = &http.Client{
		Transport: &rewriteTransport{base: http.DefaultTransport, target: server.URL},
	}

	event := WebhookSummary{Source: "github", EventType: "push", Summary: "Push to main"}
	threads := []ThreadContext{{ChannelID: "C123", ChannelName: "#dev", ThreadTS: "ts1", Topic: "Dev work", Workdir: "/ws/switchboard"}}

	decision, err := r.Route(context.Background(), event, threads)
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}
	if decision == nil {
		t.Fatal("expected non-nil decision")
	}
	if decision.Confidence != 90 {
		t.Errorf("confidence = %d, want 90", decision.Confidence)
	}
	if !r.MeetsThreshold(decision) {
		t.Error("decision should meet threshold of 80")
	}
}

func TestRoute_Disabled(t *testing.T) {
	r := New(Config{Enabled: false})
	d, err := r.Route(context.Background(), WebhookSummary{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Error("disabled router should return nil")
	}
}

func TestRoute_NoAPIKey(t *testing.T) {
	r := New(Config{Enabled: true, APIKey: ""})
	d, err := r.Route(context.Background(), WebhookSummary{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Error("router without API key should return nil")
	}
}

func TestRoute_OverBudget(t *testing.T) {
	cfg := Config{Enabled: true, APIKey: "key", MonthlyBudgetUSD: 0.001}
	r := New(cfg)
	r.costTracker.Add(1.0) // way over budget

	d, err := r.Route(context.Background(), WebhookSummary{Source: "x", EventType: "y", Summary: "z"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Error("over-budget router should return nil")
	}
}

func TestRoute_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": {"type": "server_error", "message": "internal error"}}`))
	}))
	defer server.Close()

	cfg := Config{Enabled: true, APIKey: "key", MonthlyBudgetUSD: 5.0}
	r := New(cfg)
	r.httpClient = &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, target: server.URL}}

	_, err := r.Route(context.Background(), WebhookSummary{Source: "x", EventType: "y", Summary: "z"}, []ThreadContext{{ChannelName: "#t"}})
	if err == nil {
		t.Error("expected error on API failure")
	}
}

func TestRoute_MalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": "I don't understand the question."},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := Config{Enabled: true, APIKey: "key", MonthlyBudgetUSD: 5.0}
	r := New(cfg)
	r.httpClient = &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, target: server.URL}}

	_, err := r.Route(context.Background(), WebhookSummary{Source: "x", EventType: "y", Summary: "z"}, []ThreadContext{{ChannelName: "#t"}})
	if err == nil {
		t.Error("expected error on malformed LLM response")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && containsStr(s, substr)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// rewriteTransport rewrites all requests to point to a test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = t.target[7:] // strip "http://"
	return t.base.RoundTrip(req)
}
