// Package render provides human-friendly rendering utilities for tool
// invocations in the Switchboard bridge. Feature 1c of the v1.1 spec.
package render

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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

	// Try exact match first, then case-insensitive title case.
	// Use targetWords as the soft truncation limit for heuristic descriptions.
	if fn, ok := heuristics[tool]; ok {
		if desc := fn(input); desc != "" {
			return TruncateWords(desc, targetWords)
		}
	}

	// Try title-cased version (jcode sends "bash", heuristics use "Bash").
	titleTool := toTitleCase(strings.ToLower(tool))
	if titleTool != tool {
		if fn, ok := heuristics[titleTool]; ok {
			if desc := fn(input); desc != "" {
				return TruncateWords(desc, targetWords)
			}
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
// Configuration (match [render.descriptions] config)
// ---------------------------------------------------------------------------

var (
	targetWords       = 12
	hardTruncateWords = 14
	argTruncateChars  = 50
)

// ConfigureDescriptions updates the description length limits from config.
// Values of 0 retain the defaults.
func ConfigureDescriptions(target, hardTruncate int) {
	if target > 0 {
		targetWords = target
	}
	if hardTruncate > 0 {
		hardTruncateWords = hardTruncate
	}
}

// toTitleCase converts a string to title case using the x/text cases package.
// Replaces the deprecated strings.Title.
func toTitleCase(s string) string {
	return cases.Title(language.English, cases.NoLower).String(s)
}

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

	// Try to extract a meaningful description from the command.
	if desc := describeBashCommand(cmd); desc != "" {
		return desc
	}

	// Fallback: show truncated command.
	cmd = truncateStr(cmd, argTruncateChars)
	return fmt.Sprintf("Running `%s`", cmd)
}

// describeBashCommand attempts to produce a human-friendly 4-8 word description
// of a bash command by recognising common patterns.
func describeBashCommand(cmd string) string {
	// Strip leading cd, env vars, sudo, pipes-to-tail etc.
	// Focus on the primary command.
	cmd = strings.TrimSpace(cmd)

	// Strip leading "cd ... &&" or "cd ... ;"
	if strings.HasPrefix(cmd, "cd ") {
		if idx := strings.Index(cmd, "&&"); idx > 0 {
			cmd = strings.TrimSpace(cmd[idx+2:])
		} else if idx := strings.Index(cmd, ";"); idx > 0 {
			cmd = strings.TrimSpace(cmd[idx+1:])
		}
	}

	// Split into first token (the binary) and rest.
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}
	binary := filepath.Base(parts[0])

	switch binary {
	case "go":
		return describeBashGo(parts)
	case "git":
		return describeBashGit(parts)
	case "make":
		if len(parts) > 1 {
			return fmt.Sprintf("Running make %s", parts[1])
		}
		return "Running make"
	case "npm", "yarn", "pnpm":
		if len(parts) > 1 {
			return fmt.Sprintf("Running %s %s", binary, parts[1])
		}
		return fmt.Sprintf("Running %s", binary)
	case "docker", "docker-compose":
		if len(parts) > 1 {
			return fmt.Sprintf("Docker %s", parts[1])
		}
		return "Running docker"
	case "curl":
		return describeBashCurl(parts)
	case "grep", "rg", "ag":
		return describeBashGrep(parts)
	case "cat", "head", "tail", "less":
		return describeBashCat(binary, parts)
	case "ls", "find":
		return describeBashLs(binary, parts)
	case "mkdir":
		if len(parts) > 1 {
			return fmt.Sprintf("Creating directory %s", filepath.Base(lastNonFlag(parts[1:])))
		}
		return "Creating directory"
	case "rm":
		return "Removing files"
	case "cp", "mv":
		return describeBashCopy(binary, parts)
	case "sed", "awk":
		return describeBashSed(binary, parts)
	case "pip", "pip3":
		if len(parts) > 1 {
			return fmt.Sprintf("Pip %s packages", parts[1])
		}
		return "Running pip"
	case "pytest", "jest", "cargo":
		if binary == "cargo" && len(parts) > 1 {
			return fmt.Sprintf("Running cargo %s", parts[1])
		}
		return fmt.Sprintf("Running %s tests", binary)
	case "systemctl":
		return describeBashSystemctl(parts)
	case "journalctl":
		return "Checking service logs"
	case "sleep":
		return "Waiting"
	case "echo":
		return "Printing output"
	case "wc":
		return "Counting lines"
	case "sort", "uniq":
		return "Sorting output"
	case "tee":
		if len(parts) > 1 {
			return fmt.Sprintf("Writing to %s", filepath.Base(lastNonFlag(parts[1:])))
		}
		return "Writing output"
	}

	return ""
}

func describeBashGo(parts []string) string {
	if len(parts) < 2 {
		return "Running go"
	}
	switch parts[1] {
	case "build":
		target := ""
		for _, p := range parts[2:] {
			if !strings.HasPrefix(p, "-") {
				target = p
				break
			}
		}
		if target != "" {
			return fmt.Sprintf("Building %s", truncateStr(target, 20))
		}
		return "Building Go binary"
	case "test":
		pkg := ""
		for _, p := range parts[2:] {
			if !strings.HasPrefix(p, "-") {
				pkg = p
				break
			}
		}
		if pkg != "" {
			return fmt.Sprintf("Testing %s", truncateStr(pkg, 25))
		}
		return "Running Go tests"
	case "run":
		return "Running Go program"
	case "mod":
		if len(parts) > 2 {
			return fmt.Sprintf("Go mod %s", parts[2])
		}
		return "Managing Go modules"
	case "vet":
		return "Vetting Go code"
	case "fmt":
		return "Formatting Go code"
	default:
		return fmt.Sprintf("Running go %s", parts[1])
	}
}

func describeBashGit(parts []string) string {
	if len(parts) < 2 {
		return "Running git"
	}
	switch parts[1] {
	case "add":
		return "Staging changes"
	case "commit":
		return "Committing changes"
	case "push":
		return "Pushing to remote"
	case "pull":
		return "Pulling from remote"
	case "checkout", "switch":
		if len(parts) > 2 {
			return fmt.Sprintf("Switching to %s", parts[2])
		}
		return "Switching branches"
	case "log":
		return "Viewing git history"
	case "diff":
		return "Viewing git diff"
	case "status":
		return "Checking git status"
	case "stash":
		return "Stashing changes"
	case "rebase":
		return "Rebasing branch"
	case "merge":
		return "Merging branches"
	case "clone":
		return "Cloning repository"
	default:
		return fmt.Sprintf("Running git %s", parts[1])
	}
}

func describeBashCurl(parts []string) string {
	for _, p := range parts {
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			parsed, err := url.Parse(p)
			if err == nil && parsed.Host != "" {
				return fmt.Sprintf("Calling %s API", parsed.Hostname())
			}
		}
	}
	return "Making HTTP request"
}

func describeBashGrep(parts []string) string {
	// Find the pattern (first non-flag arg)
	pattern := ""
	for _, p := range parts[1:] {
		if !strings.HasPrefix(p, "-") {
			pattern = p
			break
		}
	}
	if pattern != "" {
		// Strip surrounding quotes
		pattern = strings.Trim(pattern, "'\"")
		return fmt.Sprintf("Searching for `%s`", truncateStr(pattern, 20))
	}
	return "Searching files"
}

func describeBashCat(binary string, parts []string) string {
	file := lastNonFlag(parts[1:])
	if file != "" {
		return fmt.Sprintf("Reading %s", filepath.Base(file))
	}
	return fmt.Sprintf("Reading file")
}

func describeBashLs(binary string, parts []string) string {
	dir := lastNonFlag(parts[1:])
	if dir != "" {
		return fmt.Sprintf("Listing %s", truncateStr(filepath.Base(dir), 20))
	}
	return "Listing directory"
}

func describeBashCopy(binary string, parts []string) string {
	verb := "Copying"
	if binary == "mv" {
		verb = "Moving"
	}
	nonFlags := nonFlagArgs(parts[1:])
	if len(nonFlags) >= 2 {
		return fmt.Sprintf("%s to %s", verb, filepath.Base(nonFlags[len(nonFlags)-1]))
	}
	return fmt.Sprintf("%s files", verb)
}

func describeBashSed(binary string, parts []string) string {
	// Try to find the file being edited
	file := ""
	for i := len(parts) - 1; i >= 1; i-- {
		if !strings.HasPrefix(parts[i], "-") && !strings.HasPrefix(parts[i], "'") && !strings.HasPrefix(parts[i], "\"") && !strings.HasPrefix(parts[i], "s/") {
			file = parts[i]
			break
		}
	}
	if file != "" {
		return fmt.Sprintf("Editing %s with %s", filepath.Base(file), binary)
	}
	return fmt.Sprintf("Transforming text with %s", binary)
}

func describeBashSystemctl(parts []string) string {
	// systemctl [--user] <action> <service>
	action := ""
	service := ""
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "-") {
			continue
		}
		if action == "" {
			action = p
		} else {
			service = p
			break
		}
	}
	if action != "" && service != "" {
		switch action {
		case "restart":
			return fmt.Sprintf("Restarting %s service", service)
		case "start":
			return fmt.Sprintf("Starting %s service", service)
		case "stop":
			return fmt.Sprintf("Stopping %s service", service)
		case "status":
			return fmt.Sprintf("Checking %s status", service)
		default:
			return fmt.Sprintf("Service %s %s", action, service)
		}
	}
	if action != "" {
		return fmt.Sprintf("Running systemctl %s", action)
	}
	return "Managing services"
}

// lastNonFlag returns the last argument that doesn't start with "-".
func lastNonFlag(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
}

// nonFlagArgs returns all arguments that don't start with "-".
func nonFlagArgs(args []string) []string {
	var result []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			result = append(result, a)
		}
	}
	return result
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
