package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// SlackClient is a minimal Slack Web API client for test use.
type SlackClient struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// NewSlackClient creates a new test Slack client.
func NewSlackClient(token string) *SlackClient {
	return &SlackClient{
		Token:   token,
		BaseURL: "https://slack.com/api",
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// SlackMessage represents a message from the Slack API.
type SlackMessage struct {
	Text     string `json:"text"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	SubType  string `json:"subtype"`
}

// PostMessage sends a message to a channel.
func (c *SlackClient) PostMessage(channel, text string, threadTS string) (string, error) {
	params := url.Values{
		"channel": {channel},
		"text":    {text},
	}
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := c.apiCall("chat.postMessage", params, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("slack: chat.postMessage: %s", resp.Error)
	}
	return resp.TS, nil
}

// GetThreadReplies fetches all replies in a thread.
func (c *SlackClient) GetThreadReplies(channel, threadTS string) ([]SlackMessage, error) {
	params := url.Values{
		"channel": {channel},
		"ts":      {threadTS},
	}

	var resp struct {
		OK       bool           `json:"ok"`
		Error    string         `json:"error"`
		Messages []SlackMessage `json:"messages"`
	}
	if err := c.apiCall("conversations.replies", params, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("slack: conversations.replies: %s", resp.Error)
	}
	return resp.Messages, nil
}

// WaitForBotReply polls a thread until a bot reply appears or timeout.
func (c *SlackClient) WaitForBotReply(t *testing.T, channel, threadTS string, botUserID string, timeout time.Duration) SlackMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for time.Now().Before(deadline) {
		msgs, err := c.GetThreadReplies(channel, threadTS)
		if err != nil {
			t.Logf("GetThreadReplies error (will retry): %v", err)
			time.Sleep(interval)
			continue
		}
		for _, m := range msgs {
			// Bot messages can come via bot_id or user matching bot user ID
			if m.TS != threadTS && (m.User == botUserID || m.BotID != "") {
				return m
			}
		}
		time.Sleep(interval)
	}
	t.Fatalf("no bot reply in thread %s within %v", threadTS, timeout)
	return SlackMessage{}
}

// WaitForBotReplies polls a thread until at least n bot replies appear.
func (c *SlackClient) WaitForBotReplies(t *testing.T, channel, threadTS string, botUserID string, n int, timeout time.Duration) []SlackMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for time.Now().Before(deadline) {
		msgs, err := c.GetThreadReplies(channel, threadTS)
		if err != nil {
			t.Logf("GetThreadReplies error (will retry): %v", err)
			time.Sleep(interval)
			continue
		}
		var botMsgs []SlackMessage
		for _, m := range msgs {
			if m.TS != threadTS && (m.User == botUserID || m.BotID != "") {
				botMsgs = append(botMsgs, m)
			}
		}
		if len(botMsgs) >= n {
			return botMsgs
		}
		time.Sleep(interval)
	}
	t.Fatalf("wanted %d bot replies in thread %s, timed out after %v", n, threadTS, timeout)
	return nil
}

// DeleteMessage deletes a message (for cleanup).
func (c *SlackClient) DeleteMessage(channel, ts string) error {
	params := url.Values{
		"channel": {channel},
		"ts":      {ts},
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.apiCall("chat.delete", params, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack: chat.delete: %s", resp.Error)
	}
	return nil
}

func (c *SlackClient) apiCall(method string, params url.Values, result interface{}) error {
	req, err := http.NewRequest("POST", c.BaseURL+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return json.Unmarshal(body, result)
}
