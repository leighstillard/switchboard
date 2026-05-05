package render

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Heuristic mapping tests
// ---------------------------------------------------------------------------

func TestDescribe_Read(t *testing.T) {
	got := Describe("Read", map[string]any{"file_path": "/home/user/workspace/auth.go"})
	want := "Reading `auth.go`"
	if got != want {
		t.Errorf("Describe(Read) = %q, want %q", got, want)
	}
}

func TestDescribe_ReadNestedPath(t *testing.T) {
	got := Describe("Read", map[string]any{"file_path": "internal/coalesce/coalesce.go"})
	want := "Reading `coalesce.go`"
	if got != want {
		t.Errorf("Describe(Read nested) = %q, want %q", got, want)
	}
}

func TestDescribe_Bash(t *testing.T) {
	got := Describe("Bash", map[string]any{"command": "npm test"})
	want := "Running npm test"
	if got != want {
		t.Errorf("Describe(Bash) = %q, want %q", got, want)
	}
}

func TestDescribe_BashLongCommand(t *testing.T) {
	long := "find /home/user/workspace -name '*.go' -exec grep -l 'handleAuth' {} \\;"
	got := Describe("Bash", map[string]any{"command": long})
	// "find" is handled by the ls/find heuristic
	if got == "" {
		t.Error("expected non-empty description for find command")
	}
	// Should be a meaningful description, not just raw command
	if strings.Contains(got, "find /home") {
		t.Errorf("description should be human-friendly, got %q", got)
	}
}

func TestDescribe_Grep(t *testing.T) {
	got := Describe("Grep", map[string]any{"pattern": "handleAuth"})
	want := "Find `handleAuth` uses"
	if got != want {
		t.Errorf("Describe(Grep) = %q, want %q", got, want)
	}
}

func TestDescribe_Edit(t *testing.T) {
	got := Describe("Edit", map[string]any{"file_path": "/workspace/bar.py"})
	want := "Editing `bar.py`"
	if got != want {
		t.Errorf("Describe(Edit) = %q, want %q", got, want)
	}
}

func TestDescribe_Glob(t *testing.T) {
	got := Describe("Glob", map[string]any{"pattern": "*.tsx"})
	want := "Listing `*.tsx`"
	if got != want {
		t.Errorf("Describe(Glob) = %q, want %q", got, want)
	}
}

func TestDescribe_WebSearch(t *testing.T) {
	got := Describe("WebSearch", map[string]any{"query": "golang context cancellation"})
	want := "Search: golang context cancellation"
	if got != want {
		t.Errorf("Describe(WebSearch) = %q, want %q", got, want)
	}
}

func TestDescribe_WebFetch(t *testing.T) {
	got := Describe("WebFetch", map[string]any{"url": "https://docs.github.com/en/rest/pulls"})
	want := "Fetching docs.github.com"
	if got != want {
		t.Errorf("Describe(WebFetch) = %q, want %q", got, want)
	}
}

func TestDescribe_Write(t *testing.T) {
	got := Describe("Write", map[string]any{"file_path": "/tmp/out.json"})
	want := "Writing `out.json`"
	if got != want {
		t.Errorf("Describe(Write) = %q, want %q", got, want)
	}
}

func TestDescribe_MultiEdit(t *testing.T) {
	got := Describe("MultiEdit", map[string]any{"file_path": "internal/foo.go"})
	want := "Editing `foo.go` (multi)"
	if got != want {
		t.Errorf("Describe(MultiEdit) = %q, want %q", got, want)
	}
}

func TestDescribe_Task(t *testing.T) {
	got := Describe("Task", map[string]any{"prompt": "Refactor the database layer to use connection pooling"})
	want := "Subagent: Refactor the database layer..."
	if got != want {
		t.Errorf("Describe(Task) = %q, want %q", got, want)
	}
}

func TestDescribe_Agent(t *testing.T) {
	got := Describe("Agent", map[string]any{"prompt": "Fix the broken CI pipeline for staging"})
	want := "Subagent: Fix the broken CI..."
	if got != want {
		t.Errorf("Describe(Agent) = %q, want %q", got, want)
	}
}

func TestDescribe_TodoRead(t *testing.T) {
	got := Describe("TodoRead", map[string]any{})
	want := "Reading todos"
	if got != want {
		t.Errorf("Describe(TodoRead) = %q, want %q", got, want)
	}
}

func TestDescribe_TodoWrite(t *testing.T) {
	got := Describe("TodoWrite", map[string]any{"task": "Implement login"})
	want := "Updating todos"
	if got != want {
		t.Errorf("Describe(TodoWrite) = %q, want %q", got, want)
	}
}

func TestDescribe_ToolSearch(t *testing.T) {
	got := Describe("ToolSearch", map[string]any{"query": "database migration"})
	want := "Search: database migration tools"
	if got != want {
		t.Errorf("Describe(ToolSearch) = %q, want %q", got, want)
	}
}

func TestDescribe_Skill(t *testing.T) {
	got := Describe("Skill", map[string]any{"skill": "seo"})
	want := "Running skill `seo`"
	if got != want {
		t.Errorf("Describe(Skill) = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Fallback / unknown tools
// ---------------------------------------------------------------------------

func TestDescribe_UnknownTool(t *testing.T) {
	got := Describe("SomeCustomTool", map[string]any{})
	want := "Running SomeCustomTool"
	if got != want {
		t.Errorf("Describe(unknown) = %q, want %q", got, want)
	}
}

func TestDescribe_UnknownToolWithName(t *testing.T) {
	got := Describe("mcp__docker__run", map[string]any{})
	want := "Running mcp__docker__run"
	if got != want {
		t.Errorf("Describe(mcp tool) = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestDescribe_EmptyArgs(t *testing.T) {
	got := Describe("Read", map[string]any{})
	want := "Running Read"
	if got != want {
		t.Errorf("Describe(Read, empty args) = %q, want %q", got, want)
	}
}

func TestDescribe_NilInput(t *testing.T) {
	got := Describe("Bash", nil)
	want := "Running Bash"
	if got != want {
		t.Errorf("Describe(Bash, nil) = %q, want %q", got, want)
	}
}

func TestDescribe_EmptyToolName(t *testing.T) {
	got := Describe("", nil)
	want := "Running tool"
	if got != want {
		t.Errorf("Describe(empty, nil) = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Word-count truncation (hard limit 10 words with ellipsis)
// ---------------------------------------------------------------------------

func TestTruncateWords_Short(t *testing.T) {
	input := "Reading `auth.go`"
	got := TruncateWords(input, 10)
	if got != input {
		t.Errorf("TruncateWords short = %q, want %q", got, input)
	}
}

func TestTruncateWords_ExactlyAtLimit(t *testing.T) {
	// 10 words exactly.
	input := "one two three four five six seven eight nine ten"
	got := TruncateWords(input, 10)
	if got != input {
		t.Errorf("TruncateWords exact = %q, want %q", got, input)
	}
}

func TestTruncateWords_OverLimit(t *testing.T) {
	input := "one two three four five six seven eight nine ten eleven twelve"
	got := TruncateWords(input, 10)
	want := "one two three four five six seven eight nine ten..."
	if got != want {
		t.Errorf("TruncateWords over = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Directive-supplied description (priority 1)
// ---------------------------------------------------------------------------

func TestDescribeWithDirective(t *testing.T) {
	desc := DescribeWithDirective("Read", map[string]any{}, "Checking auth module")
	want := "Checking auth module"
	if desc != want {
		t.Errorf("DescribeWithDirective = %q, want %q", desc, want)
	}
}

func TestDescribeWithDirective_Empty(t *testing.T) {
	// Empty directive falls through to heuristic.
	desc := DescribeWithDirective("Read", map[string]any{"file_path": "foo.go"}, "")
	want := "Reading `foo.go`"
	if desc != want {
		t.Errorf("DescribeWithDirective empty = %q, want %q", desc, want)
	}
}

func TestDescribeWithDirective_TruncatesLong(t *testing.T) {
	longDirective := "Reading the entire authentication module to understand the flow of tokens through the system and verifying correctness of the implementation"
	desc := DescribeWithDirective("Read", map[string]any{}, longDirective)
	words := strings.Fields(desc)
	// hardTruncateWords is 14; with ellipsis appended to last word the count stays at 14.
	if len(words) > 15 {
		t.Errorf("directive should be truncated to ~14 words, got %d: %q", len(words), desc)
	}
	if !strings.HasSuffix(desc, "...") {
		t.Errorf("long directive should end with ..., got %q", desc)
	}
}

// ---------------------------------------------------------------------------
// Drift monitoring
// ---------------------------------------------------------------------------

func TestDriftMonitor_Empty(t *testing.T) {
	dm := NewDriftMonitor(7)
	avg := dm.Average()
	if avg != 0 {
		t.Errorf("empty average = %f, want 0", avg)
	}
	if dm.IsAboveThreshold() {
		t.Error("empty monitor should not be above threshold")
	}
}

func TestDriftMonitor_BelowThreshold(t *testing.T) {
	dm := NewDriftMonitor(7)
	dm.Record("Reading `foo.go`")     // 2 words
	dm.Record("Running `npm test`")   // 2 words
	dm.Record("Editing `bar.py`")     // 2 words

	avg := dm.Average()
	if avg >= 7 {
		t.Errorf("average = %f, should be below 7", avg)
	}
	if dm.IsAboveThreshold() {
		t.Error("should not be above threshold with short descriptions")
	}
}

func TestDriftMonitor_AboveThreshold(t *testing.T) {
	dm := NewDriftMonitor(7)
	// Record descriptions with many words.
	for i := 0; i < 10; i++ {
		dm.Record("Reading the entire authentication module to understand the flow of tokens")
	}

	if !dm.IsAboveThreshold() {
		t.Errorf("should be above threshold, avg = %f", dm.Average())
	}
}

func TestDriftMonitor_RollingWindow(t *testing.T) {
	dm := NewDriftMonitor(7)
	// Fill with long descriptions.
	for i := 0; i < 100; i++ {
		dm.Record("one two three four five six seven eight nine ten")
	}
	// Then add short ones - rolling average should eventually drop.
	for i := 0; i < 200; i++ {
		dm.Record("Reading `x`")
	}
	if dm.IsAboveThreshold() {
		t.Errorf("after many short descriptions, avg should drop below 7, got %f", dm.Average())
	}
}

// ---------------------------------------------------------------------------
// Corpus-based validation (>90% match rate as per spec)
// ---------------------------------------------------------------------------

func TestDescribe_Corpus(t *testing.T) {
	type testCase struct {
		tool   string
		input  map[string]any
		expect string // exact expected output
	}

	corpus := []testCase{
		{"Read", map[string]any{"file_path": "/home/user/project/main.go"}, "Reading `main.go`"},
		{"Read", map[string]any{"file_path": "internal/config/config.go"}, "Reading `config.go`"},
		{"Read", map[string]any{"file_path": "README.md"}, "Reading `README.md`"},
		{"Bash", map[string]any{"command": "go test ./..."}, "Testing ./..."},
		{"Bash", map[string]any{"command": "docker compose up -d"}, "Docker compose"},
		{"Bash", map[string]any{"command": "cat /etc/hostname"}, "Reading hostname"},
		{"Bash", map[string]any{"command": "git log --oneline -10"}, "Viewing git history"},
		{"Grep", map[string]any{"pattern": "TODO"}, "Find `TODO` uses"},
		{"Grep", map[string]any{"pattern": "func handleAuth"}, "Find `func handleAuth` uses"},
		{"Edit", map[string]any{"file_path": "src/components/Button.tsx"}, "Editing `Button.tsx`"},
		{"Edit", map[string]any{"file_path": "/tmp/scratch.py"}, "Editing `scratch.py`"},
		{"Glob", map[string]any{"pattern": "**/*.go"}, "Listing `**/*.go`"},
		{"Glob", map[string]any{"pattern": "src/**/*.ts"}, "Listing `src/**/*.ts`"},
		{"WebSearch", map[string]any{"query": "rust async trait"}, "Search: rust async trait"},
		{"WebFetch", map[string]any{"url": "https://pkg.go.dev/net/http"}, "Fetching pkg.go.dev"},
		{"WebFetch", map[string]any{"url": "https://api.github.com/repos/format5/switchboard"}, "Fetching api.github.com"},
		{"Write", map[string]any{"file_path": "output/report.md"}, "Writing `report.md`"},
		{"Write", map[string]any{"file_path": "/home/user/.config/app.json"}, "Writing `app.json`"},
		{"MultiEdit", map[string]any{"file_path": "server/handler.go"}, "Editing `handler.go` (multi)"},
		{"Task", map[string]any{"prompt": "Analyze the test coverage and find gaps"}, "Subagent: Analyze the test coverage..."},
		{"Agent", map[string]any{"prompt": "Review and optimize database queries for performance"}, "Subagent: Review and optimize database..."},
		{"Skill", map[string]any{"skill": "graphify"}, "Running skill `graphify`"},
		{"TodoRead", map[string]any{}, "Reading todos"},
		{"TodoWrite", map[string]any{"task": "Deploy staging"}, "Updating todos"},
	}

	matched := 0
	for _, tc := range corpus {
		got := Describe(tc.tool, tc.input)
		if got == tc.expect {
			matched++
		} else {
			t.Errorf("Describe(%q, %v) = %q, want %q", tc.tool, tc.input, got, tc.expect)
		}
	}

	matchRate := float64(matched) / float64(len(corpus)) * 100
	t.Logf("Corpus match rate: %.1f%% (%d/%d)", matchRate, matched, len(corpus))
	if matchRate < 90 {
		t.Errorf("Match rate %.1f%% is below 90%% threshold", matchRate)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractBacktickContent(s string) string {
	start := strings.Index(s, "`")
	if start == -1 {
		return ""
	}
	end := strings.LastIndex(s, "`")
	if end <= start {
		return ""
	}
	return s[start+1 : end]
}

func TestDescribe_BashSmartHeuristics(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{"go build", "go build -o switchboard ./cmd/switchboard/", "Building switchboard"},
		{"go test", "go test ./... -count=1", "Testing ./..."},
		{"go test pkg", "go test ./internal/render/ -v", "Testing ./internal/render/"},
		{"git commit", "git commit -m 'fix: stuff'", "Committing changes"},
		{"git push", "git push origin main", "Pushing to remote"},
		{"systemctl restart", "systemctl --user restart switchboard", "Restarting switchboard service"},
		{"curl api", "curl -s -X POST https://slack.com/api/chat.postMessage -H 'Auth: x'", "Calling slack.com API"},
		{"grep pattern", "grep -rn 'buildBlocks' internal/", "Searching for `buildBlocks`"},
		{"cat file", "cat /home/leigh/.config/switchboard/config.toml", "Reading config.toml"},
		{"cd then go", "cd /home/leigh/workspace/switchboard && go build ./...", "Building ./..."},
		{"journalctl", "journalctl --user -u switchboard --since '5min ago'", "Checking service logs"},
		{"sleep", "sleep 30", "Waiting"},
		{"make target", "make install", "Running make install"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Describe("Bash", map[string]any{"command": tc.command})
			if got != tc.want {
				t.Errorf("Describe(Bash, %q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}
