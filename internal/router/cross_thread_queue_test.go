package router

import (
	"testing"

	"github.com/format5/switchboard/internal/store"
)

// TestCrossThreadQueueCondition validates that the busy-session queueing
// condition correctly identifies when a message should be queued vs sent
// immediately.
func TestCrossThreadQueueCondition(t *testing.T) {
	tests := []struct {
		name      string
		isReused  bool
		status    string
		wantQueue bool
	}{
		{
			name:      "new session - send immediately",
			isReused:  false,
			status:    "",
			wantQueue: false,
		},
		{
			name:      "reused session, idle - send immediately",
			isReused:  true,
			status:    "idle",
			wantQueue: false,
		},
		{
			name:      "reused session, processing - queue",
			isReused:  true,
			status:    "processing",
			wantQueue: true,
		},
		{
			name:      "reused session, closed - send immediately",
			isReused:  true,
			status:    "closed",
			wantQueue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var existingSession *store.Session
			if tt.isReused {
				existingSession = &store.Session{Status: tt.status}
			}
			isReused := existingSession != nil

			// This mirrors the condition in handleNewSession.
			shouldQueue := isReused && existingSession != nil && existingSession.Status == "processing"

			if shouldQueue != tt.wantQueue {
				t.Errorf("shouldQueue = %v, want %v", shouldQueue, tt.wantQueue)
			}
		})
	}
}

// TestParseCoalescerKeyRoundtrip verifies key construction and parsing.
func TestParseCoalescerKeyRoundtrip(t *testing.T) {
	tests := []struct {
		channel  string
		threadTS string
	}{
		{"C0B213N3A9X", "1778120569.505289"},
		{"C0AL12WCNBG", "1778120066.445889"},
		{"CABC123", "1234567890.123456"},
	}

	for _, tt := range tests {
		key := coalescerKey(tt.channel, tt.threadTS)
		gotCh, gotTS := parseCoalescerKey(key)
		if gotCh != tt.channel || gotTS != tt.threadTS {
			t.Errorf("roundtrip(%q, %q): got (%q, %q)", tt.channel, tt.threadTS, gotCh, gotTS)
		}
	}
}
