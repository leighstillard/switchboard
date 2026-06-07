package claude

// PermissionResult is the decision a policy returns for a tool-use request.
// It maps directly onto the claude `control_response` shape.
type PermissionResult struct {
	Behavior     string         // "allow" | "deny"
	UpdatedInput map[string]any // for allow: the (possibly modified) tool input
	Message      string         // for deny: reason surfaced to the model
}

// defaultDenyMessage is substituted when a deny decision has no message, so the
// model gets actionable feedback instead of a silent reject (mirrors cc-connect).
const defaultDenyMessage = "The user denied this tool use. Stop and wait for the user's instructions."

// PermissionPolicy decides whether a tool use is permitted. The claude backend
// invokes it for each `control_request` and writes the corresponding
// `control_response` to the subprocess stdin.
type PermissionPolicy interface {
	Decide(toolName string, input map[string]any) PermissionResult
}

// AllowAll approves every tool use, echoing the input unchanged. This matches
// jcode's existing autonomous posture (no human in the loop mid-turn).
type AllowAll struct{}

func (AllowAll) Decide(_ string, input map[string]any) PermissionResult {
	if input == nil {
		input = map[string]any{}
	}
	return PermissionResult{Behavior: "allow", UpdatedInput: input}
}

// DenyAll rejects every tool use.
type DenyAll struct{}

func (DenyAll) Decide(_ string, _ map[string]any) PermissionResult {
	return PermissionResult{Behavior: "deny", Message: defaultDenyMessage}
}

// AcceptEditsOnly allows the file-editing tools and denies everything else.
type AcceptEditsOnly struct{}

func (AcceptEditsOnly) Decide(toolName string, input map[string]any) PermissionResult {
	if isEditTool(toolName) {
		if input == nil {
			input = map[string]any{}
		}
		return PermissionResult{Behavior: "allow", UpdatedInput: input}
	}
	return PermissionResult{Behavior: "deny", Message: defaultDenyMessage}
}

func isEditTool(toolName string) bool {
	switch toolName {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return true
	default:
		return false
	}
}

// policyForName resolves a config policy string to a PermissionPolicy. Unknown
// or empty values default to AllowAll (the documented default posture).
func policyForName(name string) PermissionPolicy {
	switch name {
	case "deny_all":
		return DenyAll{}
	case "accept_edits_only":
		return AcceptEditsOnly{}
	default:
		return AllowAll{}
	}
}
