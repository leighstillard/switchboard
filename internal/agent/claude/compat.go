package claude

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// version is a parsed semantic-ish CLI version (major, minor, patch).
type version = [3]int

// parseVersion extracts major.minor.patch from a `claude --version` string such
// as "2.1.168 (Claude Code)".
func parseVersion(s string) (version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return version{}, fmt.Errorf("claude: empty version string")
	}
	// Take the leading token (before any space).
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, fmt.Errorf("claude: cannot parse version %q", s)
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, fmt.Errorf("claude: cannot parse version %q: %w", s, err)
		}
		v[i] = n
	}
	return v, nil
}

// less reports whether a < b.
func less(a, b version) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// checkVersionRange parses detected/min/max and reports whether the detected
// version is below the floor (caller fails fast if claude is selected) or above
// the optional ceiling (caller warns but proceeds). An empty min/max disables
// that bound.
func checkVersionRange(detected, min, max string) (belowMin, aboveMax bool, err error) {
	dv, err := parseVersion(detected)
	if err != nil {
		return false, false, err
	}
	if min != "" {
		mv, err := parseVersion(min)
		if err != nil {
			return false, false, fmt.Errorf("min_version: %w", err)
		}
		if less(dv, mv) {
			belowMin = true
		}
	}
	if max != "" {
		xv, err := parseVersion(max)
		if err != nil {
			return false, false, fmt.Errorf("max_version: %w", err)
		}
		if less(xv, dv) {
			aboveMax = true
		}
	}
	return belowMin, aboveMax, nil
}

// probeInitShape validates that the first system/init line carries the fields
// the backend reads from it (session_id, model). A malformed init means the CLI
// protocol changed; the backend surfaces TurnError "(init)" rather than a silent
// empty turn.
func probeInitShape(line []byte) error {
	var ev struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return fmt.Errorf("incompatible claude CLI protocol (init): %w", err)
	}
	if ev.Type != "system" || ev.Subtype != "init" {
		return fmt.Errorf("incompatible claude CLI protocol (init): first event is %q/%q, want system/init", ev.Type, ev.Subtype)
	}
	if ev.SessionID == "" {
		return fmt.Errorf("incompatible claude CLI protocol (init): missing session_id")
	}
	if ev.Model == "" {
		return fmt.Errorf("incompatible claude CLI protocol (init): missing model")
	}
	return nil
}
