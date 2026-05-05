// Package render provides human-friendly rendering utilities for tool
// invocations in the Switchboard bridge. Feature 1c of the v1.1 spec.
package render

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Describe generates a terse, present-tense description of a tool invocation
// using heuristic mappings from tool name + first argument.
// This is priority 2 in the description resolution chain (after directives).
//
// Style: telegraphic, present-tense, no "I'll"/"Let me"/"Going to".
// Target: <= 8 words, hard truncate at 10 with ellipsis.
func Describe(tool string, input map[string]any) string {
	if tool == "" {
		return "Running tool"
	}

	if fn, ok := heuristics[tool]; ok {
		if desc := fn(input); desc != "" {
			return TruncateWords(desc, hardTruncateWords)
		}
	}

	return fmt.Sprintf("Running %s", tool)
}

// DescribeWithDirective resolves a tool description using the three-source
// priority chain:
//  1. Agent-supplied directive (if non-empty)
//  2. Heuristic lookup (Describe)
//  3. Fallback: "Running <tool>"
func DescribeWithDirective(tool string, input map[string]any, directive string) string {
	if directive != "" {
		return TruncateWords(directive, hardTruncateWords)
	}
	return Describe(tool, input)
}

// TruncateWords truncates a string to at most maxWords words. If truncation
// occurs, an ellipsis ("...") is appended to the last kept word.
func TruncateWords(s string, maxWords int) string {
	words := strings.Fields(s)
	if len(words) <= maxWords {
		return s
	}
	return strings.Join(words[:maxWords], " ") + "..."
}

// ---------------------------------------------------------------------------
// Configuration constants (match [render.descriptions] config)
// ---------------------------------------------------------------------------

const (
	targetWords       = 8
	hardTruncateWords = 10
	argTruncateChars  = 30
)

// ---------------------------------------------------------------------------
// Heuristic table
// ---------------------------------------------------------------------------

// heuristicFn takes the tool's input map and returns a friendly description.
// Returns "" if the required argument is missing (caller falls back to default).
type heuristicFn func(input map[string]any) string

var heuristics = map[string]heuristicFn{
	"Read":       heuristicFilePath("Reading"),
	"Write":      heuristicFilePath("Writing"),
	"Edit":       heuristicFilePath("Editing"),
	"MultiEdit":  heuristicMultiEdit,
	"Bash":       heuristicBash,
	"Grep":       heuristicGrep,
	"Glob":       heuristicGlob,
	"WebSearch":  heuristicWebSearch,
	"WebFetch":   heuristicWebFetch,
	"Task":       heuristicSubagent,
	"Agent":      heuristicSubagent,
	"TodoRead":   heuristicStatic("Reading todos"),
	"TodoWrite":  heuristicStatic("Updating todos"),
	"ToolSearch": heuristicToolSearch,
	"Skill":      heuristicSkill,
}

// ---------------------------------------------------------------------------
// Heuristic implementations
// ---------------------------------------------------------------------------

// heuristicFilePath returns a heuristic that extracts the basename from
// "file_path" and formats it as: "<verb> `<basename>`".
func heuristicFilePath(verb string) heuristicFn {
	return func(input map[string]any) string {
		path := strArg(input, "file_path")
		if path == "" {
			return ""
		}
		base := filepath.Base(path)
		return fmt.Sprintf("%s `%s`", verb, base)
	}
}

func heuristicMultiEdit(input map[string]any) string {
	path := strArg(input, "file_path")
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	return fmt.Sprintf("Editing `%s` (multi)", base)
}

func heuristicBash(input map[string]any) string {
	cmd := strArg(input, "command")
	if cmd == "" {
		return ""
	}
	cmd = truncateStr(cmd, argTruncateChars)
	return fmt.Sprintf("Running `%s`", cmd)
}

func heuristicGrep(input map[string]any) string {
	pattern := strArg(input, "pattern")
	if pattern == "" {
		return ""
	}
	pattern = truncateStr(pattern, argTruncateChars)
	return fmt.Sprintf("Find `%s` uses", pattern)
}

func heuristicGlob(input map[string]any) string {
	pattern := strArg(input, "pattern")
	if pattern == "" {
		return ""
	}
	return fmt.Sprintf("Listing `%s`", pattern)
}

func heuristicWebSearch(input map[string]any) string {
	query := strArg(input, "query")
	if query == "" {
		return ""
	}
	return fmt.Sprintf("Search: %s", truncateStr(query, argTruncateChars))
}

func heuristicWebFetch(input map[string]any) string {
	rawURL := strArg(input, "url")
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return fmt.Sprintf("Fetching %s", truncateStr(rawURL, argTruncateChars))
	}
	return fmt.Sprintf("Fetching %s", parsed.Hostname())
}

func heuristicSubagent(input map[string]any) string {
	prompt := strArg(input, "prompt")
	if prompt == "" {
		return ""
	}
	words := strings.Fields(prompt)
	if len(words) <= 4 {
		return fmt.Sprintf("Subagent: %s", strings.Join(words, " "))
	}
	return fmt.Sprintf("Subagent: %s...", strings.Join(words[:4], " "))
}

func heuristicToolSearch(input map[string]any) string {
	query := strArg(input, "query")
	if query == "" {
		return ""
	}
	return fmt.Sprintf("Search: %s tools", truncateStr(query, argTruncateChars))
}

func heuristicSkill(input map[string]any) string {
	skill := strArg(input, "skill")
	if skill == "" {
		return ""
	}
	return fmt.Sprintf("Running skill `%s`", skill)
}

// heuristicStatic returns a heuristic that always returns the given string,
// regardless of input.
func heuristicStatic(desc string) heuristicFn {
	return func(_ map[string]any) string {
		return desc
	}
}

// ---------------------------------------------------------------------------
// Drift monitoring
// ---------------------------------------------------------------------------

// DriftMonitor tracks the rolling average word count of tool descriptions
// across a session. The spec requires a warning if the average exceeds the
// configured threshold (default 7 words).
type DriftMonitor struct {
	mu        sync.Mutex
	threshold float64
	counts    []int
	// Use a circular buffer with a window of the last N descriptions.
	// 100 is enough for rolling average stability.
	maxWindow int
}

// NewDriftMonitor creates a monitor with the given word-count threshold.
func NewDriftMonitor(threshold float64) *DriftMonitor {
	return &DriftMonitor{
		threshold: threshold,
		maxWindow: 100,
	}
}

// Record logs the word count of a description.
func (dm *DriftMonitor) Record(description string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	wordCount := len(strings.Fields(description))
	dm.counts = append(dm.counts, wordCount)

	// Keep only the last maxWindow entries.
	if len(dm.counts) > dm.maxWindow {
		dm.counts = dm.counts[len(dm.counts)-dm.maxWindow:]
	}
}

// Average returns the rolling average word count.
func (dm *DriftMonitor) Average() float64 {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if len(dm.counts) == 0 {
		return 0
	}
	sum := 0
	for _, c := range dm.counts {
		sum += c
	}
	return float64(sum) / float64(len(dm.counts))
}

// IsAboveThreshold returns true if the rolling average exceeds the threshold.
func (dm *DriftMonitor) IsAboveThreshold() bool {
	return dm.Average() > dm.threshold
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// strArg safely extracts a string argument from the input map.
func strArg(input map[string]any, key string) string {
	if input == nil {
		return ""
	}
	v, ok := input[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// truncateStr truncates a string to maxLen characters, appending "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
