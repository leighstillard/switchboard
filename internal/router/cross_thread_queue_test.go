package router

import (
	"testing"
)

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
