package claude

import "testing"

func TestParseVersion(t *testing.T) {
	cases := map[string][3]int{
		"2.1.168 (Claude Code)": {2, 1, 168},
		"2.1.96 (Claude Code)":  {2, 1, 96},
		"2.1.0":                 {2, 1, 0},
		"10.20.30 (x)":          {10, 20, 30},
	}
	for in, want := range cases {
		got, err := parseVersion(in)
		if err != nil {
			t.Errorf("parseVersion(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseVersionInvalid(t *testing.T) {
	for _, in := range []string{"", "garbage", "v2", "2.x.0"} {
		if _, err := parseVersion(in); err == nil {
			t.Errorf("parseVersion(%q) expected error", in)
		}
	}
}

func TestCheckVersionRange(t *testing.T) {
	cases := []struct {
		detected, min, max string
		wantErr            bool // below min → error
		wantWarn           bool // above max → warn
	}{
		{"2.1.168", "2.1.0", "", false, false},
		{"2.1.0", "2.1.0", "", false, false},   // equal to min is OK
		{"2.0.9", "2.1.0", "", true, false},    // below min
		{"1.9.9", "2.1.0", "", true, false},    // below min (major)
		{"2.2.0", "2.1.0", "2.1.200", false, true}, // above max
		{"2.1.50", "2.1.0", "2.1.200", false, false},
		{"3.0.0", "2.1.0", "", false, false},   // no max → no warn
	}
	for _, c := range cases {
		belowMin, aboveMax, err := checkVersionRange(c.detected, c.min, c.max)
		if err != nil {
			t.Errorf("checkVersionRange(%q,%q,%q) unexpected error: %v", c.detected, c.min, c.max, err)
			continue
		}
		if belowMin != c.wantErr {
			t.Errorf("checkVersionRange(%q,%q,%q) belowMin=%v want %v", c.detected, c.min, c.max, belowMin, c.wantErr)
		}
		if aboveMax != c.wantWarn {
			t.Errorf("checkVersionRange(%q,%q,%q) aboveMax=%v want %v", c.detected, c.min, c.max, aboveMax, c.wantWarn)
		}
	}
}

// The init-shape probe validates the first system/init carries session_id+model.
func TestProbeInitShape(t *testing.T) {
	ok := `{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-4-20250514"}`
	if err := probeInitShape([]byte(ok)); err != nil {
		t.Errorf("valid init rejected: %v", err)
	}
	for _, bad := range []string{
		`{"type":"system","subtype":"init","model":"m"}`,       // no session_id
		`{"type":"system","subtype":"init","session_id":"s1"}`, // no model
		`{"type":"system","subtype":"other"}`,                  // not init
		`not json`,
	} {
		if err := probeInitShape([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
