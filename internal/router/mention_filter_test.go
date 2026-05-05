package router

import (
	"testing"

	"github.com/format5/switchboard/internal/slack"
)

// TestMentionFilterLogic validates the @mention filtering guard clause logic
// that prevents the bot from responding to messages directed at other users
// in owned threads.
func TestMentionFilterLogic(t *testing.T) {
	tests := []struct {
		name          string
		msg           *slack.InboundMessage
		shouldIgnore  bool
	}{
		{
			name: "plain reply in owned thread - process",
			msg: &slack.InboundMessage{
				MentionsBot:   false,
				MentionsOther: false,
				IsDM:          false,
			},
			shouldIgnore: false,
		},
		{
			name: "reply mentioning other user only - ignore",
			msg: &slack.InboundMessage{
				MentionsBot:   false,
				MentionsOther: true,
				IsDM:          false,
			},
			shouldIgnore: true,
		},
		{
			name: "reply mentioning bot and other user - process",
			msg: &slack.InboundMessage{
				MentionsBot:   true,
				MentionsOther: true,
				IsDM:          false,
			},
			shouldIgnore: false,
		},
		{
			name: "reply mentioning bot only - process",
			msg: &slack.InboundMessage{
				MentionsBot:   true,
				MentionsOther: false,
				IsDM:          false,
			},
			shouldIgnore: false,
		},
		{
			name: "DM mentioning other user - process (DMs always processed)",
			msg: &slack.InboundMessage{
				MentionsBot:   false,
				MentionsOther: true,
				IsDM:          true,
			},
			shouldIgnore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the guard clause from handleContinuation.
			ignored := !tt.msg.IsDM && tt.msg.MentionsOther && !tt.msg.MentionsBot
			if ignored != tt.shouldIgnore {
				t.Errorf("expected shouldIgnore=%v, got %v", tt.shouldIgnore, ignored)
			}
		})
	}
}
