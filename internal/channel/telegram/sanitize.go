// Package telegram — sanitize.go provides HTML sanitization for Telegram's
// restricted HTML subset.  Unrecognised tags are stripped, and bare `<`, `>`
// and `&` outside valid tags are entity-escaped so the Telegram API never
// rejects a message due to malformed markup.
package telegram

import (
	"regexp"
	"strings"
)

// allowedTagPattern matches the opening and closing forms of every HTML tag
// that Telegram's Bot API accepts in HTML parse mode (as of Bot API 7.x):
//
//	b, strong, i, em, u, ins, s, strike, del, tg-spoiler, span, a,
//	tg-emoji, code, pre, blockquote.
//
// Attributes (href, class, …) are kept as-is because Telegram itself
// validates them.  Self-closing tags are not used by Telegram — we only
// match opening/closing pairs.
var allowedTagPattern = regexp.MustCompile(
	`(?i)</?(?:b|strong|i|em|u|ins|s|strike|del|tg-spoiler|span|a|tg-emoji|code|pre|blockquote)(?:\s[^>]*)?>`,
)

// sanitizeHTML rewrites text so that it is safe to send with ParseMode=HTML.
//
// Strategy:
//  1. Find every occurrence of an allowed Telegram HTML tag and keep it intact.
//  2. Entity-escape `<`, `>` and `&` in all text segments between those tags.
//
// The result is always accepted by Telegram because only recognised tags
// survive, and all other markup-sensitive characters are escaped.
func sanitizeHTML(text string) string {
	indices := allowedTagPattern.FindAllStringIndex(text, -1)
	if len(indices) == 0 {
		return escapeHTMLEntities(text)
	}

	var b strings.Builder
	b.Grow(len(text) + len(text)/10) // small headroom for entity expansion

	cursor := 0

	for _, loc := range indices {
		// Escape the segment before this tag.
		if cursor < loc[0] {
			b.WriteString(escapeHTMLEntities(text[cursor:loc[0]]))
		}

		// Write the allowed tag unchanged.
		b.WriteString(text[loc[0]:loc[1]])
		cursor = loc[1]
	}

	// Escape the trailing segment after the last tag.
	if cursor < len(text) {
		b.WriteString(escapeHTMLEntities(text[cursor:]))
	}

	return b.String()
}

// escapeHTMLEntities replaces the three characters that conflict with HTML
// syntax: `&` → `&amp;`, `<` → `&lt;`, `>` → `&gt;`.
// The order matters: `&` must be replaced first.
func escapeHTMLEntities(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")

	return s
}

// stripHTMLTags removes all HTML tags from text, returning plain text suitable
// for a fallback send without parse mode.
func stripHTMLTags(text string) string {
	return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, "")
}
