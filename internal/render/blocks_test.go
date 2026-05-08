package render

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Issue 1: RenderAlert must use cases.Title (not deprecated strings.Title).
// This test validates the output is correct (title-cased level).
// ---------------------------------------------------------------------------

func TestRenderAlert_TitleCasesLevel(t *testing.T) {
	cases := []struct {
		level AlertLevel
		want  string // expected title-cased level in the output
	}{
		{AlertSuccess, "Success"},
		{AlertWarning, "Warning"},
		{AlertError, "Error"},
	}

	for _, tc := range cases {
		blocks := RenderAlert(tc.level, "test message")
		if len(blocks) == 0 {
			t.Fatalf("RenderAlert(%s) returned no blocks", tc.level)
		}
		textObj, ok := blocks[0]["text"].(map[string]interface{})
		if !ok {
			t.Fatalf("RenderAlert(%s) missing text object", tc.level)
		}
		text, ok := textObj["text"].(string)
		if !ok {
			t.Fatalf("RenderAlert(%s) text not a string", tc.level)
		}
		// The text should contain "*<TitleCased>*"
		expected := "*" + tc.want + "*"
		if !containsStr(text, expected) {
			t.Errorf("RenderAlert(%s) text = %q, want it to contain %q", tc.level, text, expected)
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstr(s, substr))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
