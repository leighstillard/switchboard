package slack

import "testing"

func TestHasMentionOtherThan_EnterpriseGridUsers(t *testing.T) {
	botID := "U0BOT123"
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "W-prefix enterprise user mention",
			text:     "hey <@W0ENTERPRISE> can you check this?",
			expected: true,
		},
		{
			name:     "W-prefix user with bot mention",
			text:     "<@U0BOT123> please ask <@W0GRID456>",
			expected: true,
		},
		{
			name:     "only W-prefix mention that equals excludeID (hypothetical)",
			text:     "hey <@W0BOT123> do this",
			expected: true, // W0BOT123 != U0BOT123, so it's "other"
		},
		{
			name:     "W-prefix bot excluded",
			text:     "hey <@W0BOT123> do this",
			expected: true, // even if we pass W0BOT123 as exclude below, this uses U0BOT123
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

func TestHasMentionOtherThan_WPrefixExclude(t *testing.T) {
	// Test that a W-prefix bot user ID can be properly excluded.
	botID := "W0GRIDBOT"
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{
			name:     "only W-prefix bot mention excluded",
			text:     "hey <@W0GRIDBOT> do this",
			expected: false,
		},
		{
			name:     "W-prefix bot and U-prefix other",
			text:     "<@W0GRIDBOT> ask <@UHUMAN123>",
			expected: true,
		},
		{
			name:     "W-prefix bot and W-prefix other",
			text:     "<@W0GRIDBOT> ask <@WOTHER99>",
			expected: true,
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

func TestUserMentionRe_MatchesWPrefix(t *testing.T) {
	// Directly test the regex matches W-prefix IDs.
	tests := []struct {
		input   string
		wantIDs []string
	}{
		{"<@U12345>", []string{"U12345"}},
		{"<@W98765>", []string{"W98765"}},
		{"<@U1> and <@W2>", []string{"U1", "W2"}},
		{"no mentions here", nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			matches := userMentionRe.FindAllStringSubmatch(tt.input, -1)
			var gotIDs []string
			for _, m := range matches {
				gotIDs = append(gotIDs, m[1])
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("got %v, want %v", gotIDs, tt.wantIDs)
			}
			for i, id := range gotIDs {
				if id != tt.wantIDs[i] {
					t.Errorf("match[%d] = %q, want %q", i, id, tt.wantIDs[i])
				}
			}
		})
	}
}
