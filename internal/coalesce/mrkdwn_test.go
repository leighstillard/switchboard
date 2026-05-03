package coalesce

import "testing"

func TestMarkdownToMrkdwn(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "bold",
			input:    "This is **bold** text",
			expected: "This is *bold* text",
		},
		{
			name:     "italic with underscores",
			input:    "This is _italic_ text",
			expected: "This is _italic_ text",
		},
		{
			name:     "italic with asterisks",
			input:    "This is *italic* text",
			expected: "This is _italic_ text",
		},
		{
			name:     "bold and italic",
			input:    "This is ***bold italic*** text",
			expected: "This is *_bold italic_* text",
		},
		{
			name:     "strikethrough",
			input:    "This is ~~deleted~~ text",
			expected: "This is ~deleted~ text",
		},
		{
			name:     "link",
			input:    "Click [here](https://example.com) now",
			expected: "Click <https://example.com|here> now",
		},
		{
			name:     "image",
			input:    "![alt text](https://example.com/img.png)",
			expected: "<https://example.com/img.png|alt text>",
		},
		{
			name:     "h1 header",
			input:    "# Main Header",
			expected: "*Main Header*",
		},
		{
			name:     "h2 header",
			input:    "## Sub Header",
			expected: "*Sub Header*",
		},
		{
			name:     "h3 header",
			input:    "### Minor Header",
			expected: "*Minor Header*",
		},
		{
			name:     "inline code preserved",
			input:    "Run `go test` now",
			expected: "Run `go test` now",
		},
		{
			name:     "code block preserved",
			input:    "Example:\n```go\nfmt.Println(\"**hello**\")\n```\nDone",
			expected: "Example:\n```go\nfmt.Println(\"**hello**\")\n```\nDone",
		},
		{
			name:     "bold inside code block not converted",
			input:    "```\n**not bold**\n```",
			expected: "```\n**not bold**\n```",
		},
		{
			name:     "horizontal rule",
			input:    "Above\n---\nBelow",
			expected: "Above\n───────\nBelow",
		},
		{
			name:     "multiple headers",
			input:    "# First\nSome text\n## Second",
			expected: "*First*\nSome text\n*Second*",
		},
		{
			name:     "mixed formatting",
			input:    "# Title\n\nThis has **bold** and _italic_ and a [link](https://x.com).",
			expected: "*Title*\n\nThis has *bold* and _italic_ and a <https://x.com|link>.",
		},
		{
			name:     "list items preserved",
			input:    "* item one\n* item two",
			expected: "* item one\n* item two",
		},
		{
			name:     "block quote preserved",
			input:    "> quoted text",
			expected: "> quoted text",
		},
		{
			name:     "bold not inside inline code",
			input:    "Use `**kwargs` in Python",
			expected: "Use `**kwargs` in Python",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "no markdown",
			input:    "Plain text here",
			expected: "Plain text here",
		},
		{
			name:     "nested bold in header",
			input:    "## **Already Bold**",
			expected: "**Already Bold**",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToMrkdwn(tt.input)
			if got != tt.expected {
				t.Errorf("MarkdownToMrkdwn(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.expected)
			}
		})
	}
}
