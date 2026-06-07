package claude

import "testing"

func TestAllowAllDecide(t *testing.T) {
	p := AllowAll{}
	input := map[string]any{"command": "echo hi"}
	res := p.Decide("Bash", input)
	if res.Behavior != "allow" {
		t.Fatalf("Behavior = %q, want allow", res.Behavior)
	}
	if res.UpdatedInput["command"] != "echo hi" {
		t.Errorf("UpdatedInput not echoed back: %v", res.UpdatedInput)
	}
}

func TestAllowAllNilInput(t *testing.T) {
	p := AllowAll{}
	res := p.Decide("Read", nil)
	if res.Behavior != "allow" {
		t.Fatalf("Behavior = %q, want allow", res.Behavior)
	}
	// allow with nil input must still serialize to a non-nil updatedInput
	// object (claude rejects a null updatedInput on an allow).
	if res.UpdatedInput == nil {
		t.Error("UpdatedInput should be non-nil for an allow decision")
	}
}

func TestDenyAllDecide(t *testing.T) {
	p := DenyAll{}
	res := p.Decide("Bash", map[string]any{"command": "rm -rf /"})
	if res.Behavior != "deny" {
		t.Fatalf("Behavior = %q, want deny", res.Behavior)
	}
	if res.Message == "" {
		t.Error("deny decision must carry a non-empty message")
	}
}

func TestAcceptEditsOnlyAllowsEditTools(t *testing.T) {
	p := AcceptEditsOnly{}
	for _, tool := range []string{"Edit", "Write", "NotebookEdit", "MultiEdit"} {
		if res := p.Decide(tool, map[string]any{}); res.Behavior != "allow" {
			t.Errorf("%s: Behavior = %q, want allow", tool, res.Behavior)
		}
	}
}

func TestAcceptEditsOnlyDeniesOthers(t *testing.T) {
	p := AcceptEditsOnly{}
	res := p.Decide("Bash", map[string]any{"command": "ls"})
	if res.Behavior != "deny" {
		t.Errorf("Bash: Behavior = %q, want deny", res.Behavior)
	}
	if res.Message == "" {
		t.Error("deny must carry a non-empty message")
	}
}

// policyForName maps the config string to a policy; unknown defaults to AllowAll.
func TestPolicyForName(t *testing.T) {
	cases := map[string]string{
		"allow_all":         "allow", // AllowAll → allow
		"deny_all":          "deny",
		"accept_edits_only": "deny", // denies Bash
		"":                  "allow",
		"garbage":           "allow",
	}
	for name, wantBehaviorForBash := range cases {
		p := policyForName(name)
		got := p.Decide("Bash", map[string]any{}).Behavior
		if got != wantBehaviorForBash {
			t.Errorf("policyForName(%q).Decide(Bash) = %q, want %q", name, got, wantBehaviorForBash)
		}
	}
}
