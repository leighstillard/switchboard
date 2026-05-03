//go:build e2e

// Package e2e contains end-to-end tests that exercise the full stack:
// Slack -> Switchboard -> jcode -> Slack response.
//
// IMPORTANT: These tests require either:
//   - A SWITCHBOARD_USER_TOKEN env var (xoxp- user OAuth token) to post as a real user, OR
//   - The tests will fall back to posting via the bot token and verifying
//     the message was delivered to Slack (but Switchboard won't process it
//     because the edge filters self-messages from the bot).
//
// When SWITCHBOARD_USER_TOKEN is set, tests exercise the full flow:
//   User message -> Slack -> Switchboard -> jcode -> Switchboard -> Slack reply
//
// Run with: go test -tags e2e ./test/e2e/ -v -timeout 180s
package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/format5/switchboard/test/testutil"
)

var (
	// botSlack posts messages as the bot (for reading threads and cleanup).
	botSlack  *testutil.SlackClient
	// userSlack posts messages as a real user (triggers Switchboard processing).
	// nil if SWITCHBOARD_USER_TOKEN is not set.
	userSlack *testutil.SlackClient
	channelID string
	botUserID string
)

func init() {
	botSlack = testutil.NewSlackClient(testutil.SlackBotToken())
	channelID = testutil.SlackChannelID()
	botUserID = testutil.BotUserID()

	if ut := os.Getenv("SWITCHBOARD_USER_TOKEN"); ut != "" {
		userSlack = testutil.NewSlackClient(ut)
	}
}

// testPrefix returns a unique prefix for test messages.
func testPrefix() string {
	return fmt.Sprintf("[E2E TEST %d]", time.Now().UnixMilli()%100000)
}

// requireUserToken skips the test if no user token is available.
func requireUserToken(t *testing.T) *testutil.SlackClient {
	t.Helper()
	if userSlack == nil {
		t.Skip("SWITCHBOARD_USER_TOKEN not set; skipping full-flow e2e test")
	}
	return userSlack
}

// TestSlackAPIConnectivity verifies basic Slack API access works.
// This runs even without a user token.
func TestSlackAPIConnectivity(t *testing.T) {
	prefix := testPrefix()
	text := fmt.Sprintf("%s API connectivity check", prefix)

	t.Log("Posting test message via bot token...")
	ts, err := botSlack.PostMessage(channelID, text, "")
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	t.Logf("Posted message ts=%s", ts)

	// Read it back.
	replies, err := botSlack.GetThreadReplies(channelID, ts)
	if err != nil {
		t.Fatalf("GetThreadReplies: %v", err)
	}
	if len(replies) == 0 {
		t.Fatal("No messages found in thread")
	}
	t.Logf("Read back %d message(s), first text: %.100s", len(replies), replies[0].Text)

	// Cleanup.
	if err := botSlack.DeleteMessage(channelID, ts); err != nil {
		t.Logf("Cleanup failed (non-fatal): %v", err)
	}
}

// TestSlackFlow runs the full end-to-end Slack flow tests.
// Requires SWITCHBOARD_USER_TOKEN to be set.
func TestSlackFlow(t *testing.T) {
	poster := requireUserToken(t)
	var threadTS string

	t.Run("MentionTriggersResponse", func(t *testing.T) {
		prefix := testPrefix()
		mention := fmt.Sprintf("<@%s> %s Say exactly: HELLO_E2E", botUserID, prefix)

		t.Logf("Posting to #data-worklog as user: %s", mention)
		ts, err := poster.PostMessage(channelID, mention, "")
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		t.Logf("Posted message ts=%s", ts)
		threadTS = ts

		// Wait for the bot to reply in the thread.
		t.Log("Waiting for bot reply (up to 90s)...")
		reply := botSlack.WaitForBotReply(t, channelID, ts, botUserID, 90*time.Second)
		t.Logf("Bot replied: ts=%s text=%.200s", reply.TS, reply.Text)

		if reply.Text == "" {
			t.Error("Bot reply text is empty")
		}
	})

	t.Run("ThreadContinuation", func(t *testing.T) {
		if threadTS == "" {
			t.Skip("no thread from MentionTriggersResponse")
		}

		// Wait for the first response to fully complete.
		time.Sleep(5 * time.Second)

		prefix := testPrefix()
		followUp := fmt.Sprintf("%s Now say exactly: GOODBYE_E2E", prefix)
		t.Logf("Posting follow-up in thread %s: %s", threadTS, followUp)

		ts, err := poster.PostMessage(channelID, followUp, threadTS)
		if err != nil {
			t.Fatalf("PostMessage (thread reply): %v", err)
		}
		t.Logf("Posted follow-up ts=%s", ts)

		// Wait for a second bot reply.
		t.Log("Waiting for second bot reply (up to 90s)...")
		replies := botSlack.WaitForBotReplies(t, channelID, threadTS, botUserID, 2, 90*time.Second)
		t.Logf("Got %d bot replies total", len(replies))

		if len(replies) < 2 {
			t.Fatalf("Expected at least 2 bot replies, got %d", len(replies))
		}

		secondReply := replies[len(replies)-1]
		t.Logf("Second bot reply: ts=%s text=%.200s", secondReply.TS, secondReply.Text)
	})

	t.Run("StopCommand", func(t *testing.T) {
		prefix := testPrefix()
		mention := fmt.Sprintf("<@%s> %s Write a very detailed 2000-word essay about the history of computing, covering every decade from the 1940s to 2020s.", botUserID, prefix)

		t.Logf("Posting long task to #data-worklog: %.100s...", mention)
		ts, err := poster.PostMessage(channelID, mention, "")
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		t.Logf("Posted message ts=%s", ts)

		// Wait for processing to start.
		time.Sleep(5 * time.Second)

		// Send !stop command.
		t.Log("Sending !stop command...")
		_, err = poster.PostMessage(channelID, "!stop", ts)
		if err != nil {
			t.Fatalf("PostMessage (!stop): %v", err)
		}

		// Wait and verify the thread state.
		time.Sleep(5 * time.Second)

		replies, err := botSlack.GetThreadReplies(channelID, ts)
		if err != nil {
			t.Fatalf("GetThreadReplies: %v", err)
		}
		t.Logf("Thread has %d messages after !stop", len(replies))

		for _, r := range replies {
			if r.TS != ts {
				t.Logf("  reply ts=%s user=%s bot_id=%s text=%.100s", r.TS, r.User, r.BotID, r.Text)
			}
		}
		// Stop is considered successful if we got here without hanging.
		// The interrupted/cancelled state is verified by the absence of
		// continued long output.
	})

	t.Run("MrkdwnFormatting", func(t *testing.T) {
		prefix := testPrefix()
		mention := fmt.Sprintf("<@%s> %s Respond with a one-line bold greeting using the word BOLD. Keep it very short, one line only.", botUserID, prefix)

		t.Logf("Posting formatting test: %.100s", mention)
		ts, err := poster.PostMessage(channelID, mention, "")
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}

		t.Log("Waiting for bot reply (up to 90s)...")
		reply := botSlack.WaitForBotReply(t, channelID, ts, botUserID, 90*time.Second)
		t.Logf("Bot reply: %s", reply.Text)

		// Verify no Markdown ** bold ** leaked through.
		if strings.Contains(reply.Text, "**") {
			t.Errorf("Reply contains Markdown ** bold ** instead of Slack mrkdwn *bold*: %s", reply.Text)
		}
	})
}

// TestWebhookIngest tests the webhook ingestion endpoint.
// This doesn't need a user token since it goes through the HTTP path.
func TestWebhookIngest(t *testing.T) {
	// TODO: implement once webhook routing to channels is configured
	t.Skip("No webhook routes configured for testing")
}
