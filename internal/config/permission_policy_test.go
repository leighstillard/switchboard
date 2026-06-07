package config

import "testing"

func TestResolvePermissionPolicy(t *testing.T) {
	cases := []struct {
		name        string
		c           ClaudeConfig
		wantPolicy  string
		wantWarn    bool
		wantErr     bool
	}{
		{"empty default allow", ClaudeConfig{}, "allow_all", false, false},
		{"bypass silent", ClaudeConfig{PermissionMode: "bypassPermissions"}, "allow_all", false, false},
		{"explicit policy", ClaudeConfig{PermissionPolicy: "deny_all"}, "deny_all", false, false},
		{"explicit accept_edits_only", ClaudeConfig{PermissionPolicy: "accept_edits_only"}, "accept_edits_only", false, false},
		{"unknown policy errors", ClaudeConfig{PermissionPolicy: "denyall"}, "", false, true},
		{"legacy default warns", ClaudeConfig{PermissionMode: "default"}, "allow_all", true, false},
		{"legacy acceptEdits", ClaudeConfig{PermissionMode: "acceptEdits"}, "accept_edits_only", true, false},
		{"legacy dontAsk", ClaudeConfig{PermissionMode: "dontAsk"}, "deny_all", true, false},
		{"legacy plan", ClaudeConfig{PermissionMode: "plan"}, "allow_all", true, false},
		{"unknown legacy warns", ClaudeConfig{PermissionMode: "weird"}, "allow_all", true, false},
		{"both set errors", ClaudeConfig{PermissionMode: "default", PermissionPolicy: "allow_all"}, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, warn, err := tc.c.ResolvePermissionPolicy()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if policy != tc.wantPolicy {
				t.Errorf("policy=%q want %q", policy, tc.wantPolicy)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn=%q wantWarn=%v", warn, tc.wantWarn)
			}
		})
	}
}
