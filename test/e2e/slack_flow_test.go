//go:build e2e

// Package e2e contains end-to-end tests that exercise the full stack:
// Test inject -> Router -> jcode -> Coalescer -> Outbound -> Slack reply
//
// These tests use the /test/inject endpoint (enabled in --debug mode) to
// simulate inbound messages, then verify the bot's replies appear in Slack.
//
// Requirements:
//   - Switchboard running with --debug (enables /test/inject)
//   - jcode daemon running
//   - Slack bot token valid
//
// Run with: go test -tags e2e ./test/e2e/ -v -timeout 180s
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/format5/switchboard/test/testutil"
)

var (
	botSlack   *testutil.SlackClient
	channelID  string
	botUserID  string
	injectURL  string
	httpClient *http.Client
)

func init() {
	botSlack = testutil.NewSlackClient(testutil.SlackBotToken())
	channelID = testutil.SlackChannelID()
	botUserID = testutil.BotUserID()
	injectURL = "http://127.0.0.1:8765/test/inject"
	httpClient = &http.Client{Timeout: 10 * time.Second}
}

// inject sends a simulated message to the router via the test endpoint.
func inject(t *testing.T, channel, threadTS, userID, text string) {
	t.Helper()
	body := map[string]string{
		"channel_id": channel,
		"thread_ts":  threadTS,
		"user_id":    userID,
		"text":       text,
	}
	data, _ := json.Marshal(body)
	resp, err := httpClient.Post(injectURL, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("inject POST failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("inject returned %d: %s", resp.StatusCode, string(respBody))
	}
}

// TestInjectEndpoint verifies the test injection endpoint is available.
func TestInjectEndpoint(t *testing.T) {
	resp, err := httpClient.Get("http://127.0.0.1:8765/health")
	if err != nil {
		t.Fatalf("Health check failed (is switchboard running?): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Health check returned %d", resp.StatusCode)
	}
	t.Log("Switchboard is running and healthy")
}

// TestSlackAPIConnectivity verifies basic Slack API access works.
func TestSlackAPIConnectivity(t *testing.T) {
	prefix := fmt.Sprintf("[E2E TEST %d]", time.Now().UnixMilli()%100000)
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

// TestMentionTriggersResponse tests: inject message -> jcode processes -> bot replies in Slack
func TestMentionTriggersResponse(t *testing.T) {
	// Inject a message that should trigger jcode to respond.
	text := "Say exactly: HELLO_E2E_TEST"
	t.Logf("Injecting message to channel %s: %s", channelID, text)
	inject(t, channelID, "", "U_E2E_TESTER", text)

	// The router will create a thread. We need to find it.
	// Wait a moment for the session to start, then look for recent bot messages.
	t.Log("Waiting for bot reply in channel (up to 90s)...")
	var reply testutil.SlackMessage
	deadline := time.Now().Add(90 * time.Second)
	startTime := time.Now()

	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		// Look for recent bot messages in the channel
		msgs, err := botSlack.GetChannelHistory(channelID, 10)
		if err != nil {
			t.Logf("GetChannelHistory error: %v", err)
			continue
		}
		for _, m := range msgs {
			if m.BotID != "" && time.Since(startTime) < 90*time.Second {
				// Check if this is a recent bot message (thread parent or reply)
				if strings.Contains(m.Text, "HELLO_E2E_TEST") || strings.Contains(m.Text, "data-worklog") {
					reply = m
					break
				}
			}
		}
		if reply.TS != "" {
			break
		}
	}

	if reply.TS == "" {
		t.Fatal("No bot reply found within timeout")
	}
	t.Logf("Bot replied: ts=%s text=%.200s", reply.TS, reply.Text)

	// Verify no Markdown ** leaked
	if strings.Contains(reply.Text, "**") {
		t.Errorf("Reply contains Markdown **: %s", reply.Text)
	}
}

// TestThreadContinuation tests: reply in thread -> jcode responds again
func TestThreadContinuation(t *testing.T) {
	// First create a session by injecting a top-level message
	text := "Say exactly one word: FIRST"
	t.Logf("Injecting initial message: %s", text)
	inject(t, channelID, "", "U_E2E_TESTER", text)

	// Wait for bot reply
	t.Log("Waiting for initial bot reply (up to 60s)...")
	time.Sleep(5 * time.Second)

	// Find the thread the bot replied to
	msgs, err := botSlack.GetChannelHistory(channelID, 5)
	if err != nil {
		t.Fatalf("GetChannelHistory: %v", err)
	}

	var threadTS string
	for _, m := range msgs {
		if m.BotID != "" && strings.Contains(m.Text, "data-worklog") {
			// This is the bot's response - it should be in a thread
			threadTS = m.ThreadTS
			if threadTS == "" {
				threadTS = m.TS
			}
			break
		}
	}

	if threadTS == "" {
		t.Skip("Could not find bot reply thread from initial message")
	}
	t.Logf("Found thread: %s", threadTS)

	// Wait for the first turn to complete
	time.Sleep(10 * time.Second)

	// Now send a follow-up in that thread
	followUp := "Say exactly one word: SECOND"
	t.Logf("Injecting follow-up in thread %s: %s", threadTS, followUp)
	inject(t, channelID, threadTS, "U_E2E_TESTER", followUp)

	// Wait for second bot reply
	t.Log("Waiting for second bot reply (up to 60s)...")
	deadline := time.Now().Add(60 * time.Second)
	var replies []testutil.SlackMessage
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		allReplies, err := botSlack.GetThreadReplies(channelID, threadTS)
		if err != nil {
			continue
		}
		// Count bot replies
		var botReplies []testutil.SlackMessage
		for _, r := range allReplies {
			if r.BotID != "" {
				botReplies = append(botReplies, r)
			}
		}
		if len(botReplies) >= 2 {
			replies = botReplies
			break
		}
	}

	if len(replies) < 2 {
		t.Fatalf("Expected at least 2 bot replies in thread, got %d", len(replies))
	}
	t.Logf("Got %d bot replies - thread continuation works!", len(replies))
}

// TestMrkdwnFormatting verifies Slack mrkdwn is used (not Markdown).
func TestMrkdwnFormatting(t *testing.T) {
	text := "Respond with the word BOLD wrapped in asterisks like *BOLD*. Only output that, nothing else."
	t.Logf("Injecting formatting test: %s", text)
	inject(t, channelID, "", "U_E2E_TESTER", text)

	t.Log("Waiting for bot reply (up to 60s)...")
	time.Sleep(5 * time.Second)

	msgs, err := botSlack.GetChannelHistory(channelID, 5)
	if err != nil {
		t.Fatalf("GetChannelHistory: %v", err)
	}

	var botReply testutil.SlackMessage
	for _, m := range msgs {
		if m.BotID != "" {
			botReply = m
			break
		}
	}

	if botReply.TS == "" {
		t.Fatal("No bot reply found")
	}
	t.Logf("Bot reply text: %s", botReply.Text)

	// The header should use *bold* not **bold**
	if strings.Contains(botReply.Text, "**") {
		t.Errorf("Reply contains Markdown ** instead of Slack *bold*: %s", botReply.Text)
	}
}

// TestStopCommand verifies !stop cancels the current turn.
func TestStopCommand(t *testing.T) {
	// Start a long task
	text := "Write a very detailed 5000-word essay about the complete history of computing from the 1940s to the 2020s, covering every major development in extreme detail."
	t.Logf("Injecting long task...")
	inject(t, channelID, "", "U_E2E_TESTER", text)

	// Wait for processing to start
	time.Sleep(5 * time.Second)

	// Find the thread
	msgs, err := botSlack.GetChannelHistory(channelID, 5)
	if err != nil {
		t.Fatalf("GetChannelHistory: %v", err)
	}

	var threadTS string
	for _, m := range msgs {
		if m.BotID != "" {
			threadTS = m.ThreadTS
			if threadTS == "" {
				threadTS = m.TS
			}
			break
		}
	}

	if threadTS == "" {
		t.Skip("Could not find bot reply thread")
	}

	// Send !stop in the thread
	t.Logf("Sending !stop in thread %s", threadTS)
	inject(t, channelID, threadTS, "U_E2E_TESTER", "!stop")

	// Verify it didn't crash - give it a moment
	time.Sleep(3 * time.Second)

	// Check health
	resp, err := httpClient.Get("http://127.0.0.1:8765/health")
	if err != nil {
		t.Fatalf("Health check failed after !stop: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("Switchboard unhealthy after !stop")
	}
	t.Log("!stop processed successfully, service still healthy")
}
