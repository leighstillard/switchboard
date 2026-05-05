package slack

import "testing"

func TestHasMentionOtherThan(t *testing.T) {
	botID := "U0BOT123"
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "no mentions",
			text:     "hello world",
			expected: false,
		},
		{
			name:     "only bot mention",
			text:     "hey <@U0BOT123> do this",
			expected: false,
		},
		{
			name:     "other user mention",
			text:     "hey <@UOTHER456> can you review?",
			expected: true,
		},
		{
			name:     "bot and other user mention",
			text:     "<@U0BOT123> ask <@UOTHER456> about it",
			expected: true,
		},
		{
			name:     "multiple other mentions",
			text:     "<@UALICE> <@UBOB> thoughts?",
			expected: true,
		},
		{
			name:     "mention in URL-like context (not a real mention)",
			text:     "see https://example.com/<@fake>",
			expected: false, // doesn't match U[A-Z0-9]+ pattern
		},
		{
			name:     "empty text",
			text:     "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasMentionOtherThan(tt.text, botID)
			if got != tt.expected {
				t.Errorf("hasMentionOtherThan(%q, %q) = %v, want %v", tt.text, botID, got, tt.expected)
			}
		})
	}
}
