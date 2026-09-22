// Package render provides Block Kit rendering for switchboard directives.
package render

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Directive extraction
// ---------------------------------------------------------------------------

// directiveRe matches fenced ```switchboard ... ``` blocks.
// It captures the JSON content between the fences.
var directiveRe = regexp.MustCompile("(?s)```switchboard\\s*\n(.*?)```")

// KnownDirectives is the set of recognised render directive types for v1.1.
var KnownDirectives = map[string]bool{
	"plan":    true,
	"brief":   true,
	"poll":    true,
	"tickets": true,
	"todos":   true,
	"attach":  true,
}

// MaxSupportedVersion is the highest directive version we support.
const MaxSupportedVersion = 1

// Directive is the parsed representation of a render directive.
type Directive struct {
	Render  string          `json:"render"`
	Version int             `json:"version,omitempty"`
	Raw     json.RawMessage `json:"-"` // full JSON for type-specific parsing
}

// DirectiveResult holds the output of processing a text stream for directives.
type DirectiveResult struct {
	// CleanText is the text with valid directives removed.
	CleanText string
	// Blocks is the rendered Block Kit JSON from valid directives.
	Blocks []map[string]interface{}
	// FallbackText is the plain-text summary for block fallback.
	FallbackText string
	// Attachments is the list of absolute file paths from valid "attach" directives.
	Attachments []string
}

// ExtractDirectives scans text for ```switchboard fenced blocks, validates them,
// renders valid ones to Block Kit, and returns cleaned text + blocks.
// Invalid or unrecognised directives are left in the text as visible code blocks.
func ExtractDirectives(text string, strict bool) DirectiveResult {
	result := DirectiveResult{}

	matches := directiveRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		result.CleanText = text
		return result
	}

	var cleanParts []string
	var fallbacks []string
	lastEnd := 0

	for _, match := range matches {
		// match[0]:match[1] is the full ```switchboard...``` block
		// match[2]:match[3] is the captured JSON content
		fullStart, fullEnd := match[0], match[1]
		jsonStart, jsonEnd := match[2], match[3]

		// Add text before this directive
		cleanParts = append(cleanParts, text[lastEnd:fullStart])

		jsonContent := strings.TrimSpace(text[jsonStart:jsonEnd])

		// "attach" directives don't render Block Kit; handle them separately.
		if path, ok, err := parseAttachDirective(jsonContent); ok {
			if err != nil {
				if strict {
					slog.Warn("render: dropping invalid directive", "error", err)
				} else {
					cleanParts = append(cleanParts, text[fullStart:fullEnd])
				}
				lastEnd = fullEnd
				continue
			}
			result.Attachments = append(result.Attachments, path)
			lastEnd = fullEnd
			continue
		}

		// Try to parse as a directive
		blocks, fallback, err := processDirective(jsonContent)
		if err != nil {
			// Invalid directive: leave as visible code block
			if strict {
				slog.Warn("render: dropping invalid directive", "error", err)
			} else {
				// Leave the original fenced block in the text
				cleanParts = append(cleanParts, text[fullStart:fullEnd])
			}
			lastEnd = fullEnd
			continue
		}

		// Valid directive: remove from text, add blocks
		result.Blocks = append(result.Blocks, blocks...)
		if fallback != "" {
			fallbacks = append(fallbacks, fallback)
		}
		lastEnd = fullEnd
	}

	// Add remaining text after last directive
	cleanParts = append(cleanParts, text[lastEnd:])
	result.CleanText = strings.Join(cleanParts, "")
	result.FallbackText = strings.Join(fallbacks, " | ")

	return result
}

// parseAttachDirective checks whether jsonContent is an "attach" directive.
// ok is true iff the directive's "render" field is "attach" (regardless of
// validity); err is non-nil when path is missing or not absolute.
func parseAttachDirective(jsonContent string) (path string, ok bool, err error) {
	var ad struct {
		Render string `json:"render"`
		Path   string `json:"path"`
	}
	if json.Unmarshal([]byte(jsonContent), &ad) != nil || ad.Render != "attach" {
		return "", false, nil
	}
	if ad.Path == "" || !filepath.IsAbs(ad.Path) {
		return "", true, fmt.Errorf("attach directive path must be absolute: %q", ad.Path)
	}
	return ad.Path, true, nil
}

// processDirective parses and renders a single directive JSON.
// Returns (blocks, fallbackText, error).
func processDirective(jsonContent string) ([]map[string]interface{}, string, error) {
	// Parse the base directive fields
	var dir Directive
	if err := json.Unmarshal([]byte(jsonContent), &dir); err != nil {
		return nil, "", fmt.Errorf("invalid JSON: %w", err)
	}

	if dir.Render == "" {
		return nil, "", fmt.Errorf("missing 'render' field")
	}

	// Check if directive type is known
	if !KnownDirectives[dir.Render] {
		return nil, "", fmt.Errorf("unknown directive type: %q", dir.Render)
	}

	// Check version
	version := dir.Version
	if version == 0 {
		version = 1 // default
	}
	if version > MaxSupportedVersion {
		return nil, "", fmt.Errorf("unsupported version %d for directive %q (max: %d)",
			version, dir.Render, MaxSupportedVersion)
	}

	// Route to the appropriate renderer
	switch dir.Render {
	case "plan":
		return renderPlanDirective([]byte(jsonContent))
	case "brief":
		return renderBriefDirective([]byte(jsonContent))
	case "poll":
		return renderPollDirective([]byte(jsonContent))
	case "tickets":
		return renderTicketsDirective([]byte(jsonContent))
	case "todos":
		return renderTodosDirective([]byte(jsonContent))
	default:
		return nil, "", fmt.Errorf("unhandled directive: %q", dir.Render)
	}
}

// HasDirectives returns true if the text contains any ```switchboard blocks.
func HasDirectives(text string) bool {
	return directiveRe.MatchString(text)
}
