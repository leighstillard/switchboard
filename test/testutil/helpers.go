// Package testutil provides shared helpers for integration and e2e tests.
package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// JcodeSocketPath returns the jcode daemon socket path.
func JcodeSocketPath() string {
	if p := os.Getenv("JCODE_SOCKET"); p != "" {
		return p
	}
	return fmt.Sprintf("/run/user/%d/jcode.sock", os.Getuid())
}

// SlackBotToken returns the Slack bot token from env or config.
func SlackBotToken() string {
	if t := os.Getenv("SWITCHBOARD_BOT_TOKEN"); t != "" {
		return t
	}
	return ""
}

// SlackChannelID returns the e2e test channel.
func SlackChannelID() string {
	if c := os.Getenv("SWITCHBOARD_TEST_CHANNEL"); c != "" {
		return c
	}
	return "C0AL12WCNBG" // #data-worklog
}

// BotUserID returns the bot's Slack user ID.
func BotUserID() string {
	if b := os.Getenv("SWITCHBOARD_BOT_USER_ID"); b != "" {
		return b
	}
	return "U0B1FNCSWP6"
}

// WaitFor polls a condition function until it returns true or the timeout
// expires. Returns an error if the timeout is reached.
func WaitFor(t *testing.T, timeout time.Duration, interval time.Duration, desc string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if cond() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WaitFor timed out after %v: %s", timeout, desc)
		case <-ticker.C:
		}
	}
}

// WaitForChan waits for a value on a channel with a timeout.
func WaitForChan[T any](t *testing.T, ch <-chan T, timeout time.Duration, desc string) T {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed while waiting for: %s", desc)
		}
		return v
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for: %s", timeout, desc)
	}
	var zero T
	return zero
}

// CollectEvents drains events from a channel for a duration.
func CollectEvents[T any](ch <-chan T, duration time.Duration) []T {
	var result []T
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return result
			}
			result = append(result, v)
		case <-timer.C:
			return result
		}
	}
}

// RequireEnv skips the test if a required environment variable is not set.
func RequireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("required env var %s not set", key)
	}
	return v
}
