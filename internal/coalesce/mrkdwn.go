// Package coalesce provides Markdown-to-Slack-mrkdwn conversion.
package coalesce

import (
	"regexp"
	"strings"
)

// Precompiled patterns for Markdown → Slack mrkdwn conversion.
var (
	// Headers: # Header → *Header*
	reHeader = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

	// Bold+italic: ***text*** → *_text_*
	reBoldItalic = regexp.MustCompile(`\*{3}(.+?)\*{3}`)

	// Bold: **text** → *text*
	reBold = regexp.MustCompile(`\*{2}(.+?)\*{2}`)

	// Italic (single asterisk): *text* → _text_
	reItalicAsterisk = regexp.MustCompile(`(?:^|(?P<pre>[^*]))\*(?P<inner>[^*\n]+?)\*(?:(?P<post>[^*])|$)`)

	// Strikethrough: ~~text~~ → ~text~
	reStrikethrough = regexp.MustCompile(`~~(.+?)~~`)

	// Links: [text](url) → <url|text>
	reLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

	// Images: ![alt](url) → <url|alt>
	reImage = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

	// Horizontal rules: --- or *** or ___ → ───────
	reHRule = regexp.MustCompile(`(?m)^[-*_]{3,}\s*$`)
)

const (
	boldOpen  = "\x01BOPEN\x01"
	boldClose = "\x01BCLOSE\x01"
)

// MarkdownToMrkdwn converts standard Markdown to Slack's mrkdwn format.
//
// Handles: headers, bold, italic, bold+italic, strikethrough, links, images,
// code blocks (preserved), inline code (preserved), horizontal rules.
func MarkdownToMrkdwn(md string) string {
	if md == "" {
		return ""
	}

	// Protect code blocks and inline code from conversion.
	result, codeBlocks, inlineCodes := extractCode(md)

	// Images before links (images contain link syntax).
	result = reImage.ReplaceAllString(result, "<$2|$1>")

	// Links: [text](url) → <url|text>
	result = reLink.ReplaceAllString(result, "<$2|$1>")

	// Headers: # text → *text* (using placeholders to avoid italic conversion)
	result = reHeader.ReplaceAllString(result, boldOpen+"$1"+boldClose)

	// Bold+italic: ***text*** → *_text_* (placeholder for bold part)
	result = reBoldItalic.ReplaceAllString(result, boldOpen+"_${1}_"+boldClose)

	// Bold: **text** → placeholder (to protect from italic conversion)
	result = reBold.ReplaceAllString(result, boldOpen+"$1"+boldClose)

	// Italic (single *): *text* → _text_
	result = convertSingleAsteriskItalics(result)

	// Now replace bold placeholders with actual *
	result = strings.ReplaceAll(result, boldOpen, "*")
	result = strings.ReplaceAll(result, boldClose, "*")

	// Strikethrough: ~~text~~ → ~text~
	result = reStrikethrough.ReplaceAllString(result, "~$1~")

	// Horizontal rules.
	result = reHRule.ReplaceAllString(result, "───────")

	// Restore code blocks and inline code.
	result = restoreCode(result, codeBlocks, inlineCodes)

	return result
}

// extractCode removes fenced code blocks and inline code, replacing them
// with placeholders. Returns the modified string and the extracted segments.
func extractCode(s string) (string, []string, []string) {
	var codeBlocks []string
	var inlineCodes []string

	// Fenced code blocks (``` ... ```)
	reCodeBlock := regexp.MustCompile("(?s)```.*?\n.*?```")
	result := reCodeBlock.ReplaceAllStringFunc(s, func(match string) string {
		idx := len(codeBlocks)
		codeBlocks = append(codeBlocks, match)
		return "\x00CB" + string(rune(idx+0x100)) + "\x00"
	})

	// Inline code (`...`)
	reInlineCode := regexp.MustCompile("`[^`\n]+`")
	result = reInlineCode.ReplaceAllStringFunc(result, func(match string) string {
		idx := len(inlineCodes)
		inlineCodes = append(inlineCodes, match)
		return "\x00IC" + string(rune(idx+0x100)) + "\x00"
	})

	return result, codeBlocks, inlineCodes
}

// restoreCode puts code blocks and inline code back.
func restoreCode(s string, codeBlocks, inlineCodes []string) string {
	for i, block := range codeBlocks {
		placeholder := "\x00CB" + string(rune(i+0x100)) + "\x00"
		s = strings.Replace(s, placeholder, block, 1)
	}
	for i, code := range inlineCodes {
		placeholder := "\x00IC" + string(rune(i+0x100)) + "\x00"
		s = strings.Replace(s, placeholder, code, 1)
	}
	return s
}

// convertSingleAsteriskItalics converts remaining *text* (single asterisk)
// to _text_ for Slack mrkdwn italic. Skips bold placeholders and list items.
func convertSingleAsteriskItalics(s string) string {
	var sb strings.Builder
	runes := []rune(s)
	n := len(runes)

	for i := 0; i < n; i++ {
		if runes[i] != '*' {
			sb.WriteRune(runes[i])
			continue
		}

		// Skip if at start of line (list item).
		if i == 0 || runes[i-1] == '\n' {
			sb.WriteRune(runes[i])
			continue
		}

		// Look for closing single asterisk on the same line.
		end := -1
		for j := i + 1; j < n; j++ {
			if runes[j] == '\n' {
				break
			}
			if runes[j] == '*' {
				end = j
				break
			}
		}

		if end > i+1 {
			sb.WriteRune('_')
			for k := i + 1; k < end; k++ {
				sb.WriteRune(runes[k])
			}
			sb.WriteRune('_')
			i = end
		} else {
			sb.WriteRune(runes[i])
		}
	}
	return sb.String()
}
