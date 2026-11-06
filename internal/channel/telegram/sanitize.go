// Package telegram — sanitize.go provides HTML sanitization for Telegram's
// restricted HTML subset.  Unrecognised tags are stripped, and bare `<`, `>`
// and `&` outside valid tags are entity-escaped so the Telegram API never
// rejects a message due to malformed markup.  balanceTags completes the
// guarantee: the whitelisted tags themselves are emitted balanced (streaming
// chunk boundaries and the 4096-char truncation can split an open/close
// pair), which is the failure mode behind Telegram's "Can't find end tag"
// rejections.
package telegram

import (
	"regexp"
	"strings"
)

// allowedTagPattern matches the opening and closing forms of every HTML tag
// that Telegram's Bot API accepts in HTML parse mode (officially listed
// under "HTML style" in the Bot API docs):
//
//	b, strong, i, em, u, ins, s, strike, del, tg-spoiler,
//	span class="tg-spoiler", a, tg-emoji, code, pre, blockquote
//	(also in the expandable form: <blockquote expandable>).
//
// A bare <span> is NOT accepted by Telegram — only the tg-spoiler form —
// so it does not match and gets entity-escaped like any other unknown tag.
// Attributes (href, class, …) are kept as-is because Telegram itself
// validates them.  Self-closing tags are not used by Telegram — we only
// match opening/closing pairs.
var allowedTagPattern = regexp.MustCompile(
	`(?i)</?(?:b|strong|i|em|u|ins|s|strike|del|tg-spoiler|a|tg-emoji|code|pre|blockquote)(?:\s[^>]*)?>` +
		`|<span\s+class=(?:"tg-spoiler"|tg-spoiler)(?:\s[^>]*)?>` +
		`|</span>`,
)

// sanitizeHTML rewrites text so that it is safe to send with ParseMode=HTML.
//
// Strategy:
//  1. Find every occurrence of an allowed Telegram HTML tag and keep it intact.
//  2. Entity-escape `<`, `>` and `&` in all text segments between those tags.
//  3. Balance the kept tags (balanceTags), so the result is accepted by
//     Telegram even when the input carried unbalanced whitelisted tags.
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

	return balanceTags(b.String())
}

// balanceTags rewrites s so that its whitelisted HTML tags are balanced —
// the shape Telegram's HTML parser requires.  Three repairs, all arising
// when a complete (globally balanced) text is cut for delivery: a streaming
// chunk boundary or the 4096-char truncation can leave an opening tag
// unclosed, orphan its closing tag, or cut a tag or an entity in half.
//
//   - a trailing incomplete tag or entity (truncation artifact, e.g. "<b" or
//     "&am") is removed whole;
//   - a closing tag whose opener is not on the stack is dropped;
//   - opening tags still unclosed at the end get their closing tags appended
//     in reverse order, so the text keeps its styling.
//
// On already-balanced input it is a no-op.  It must run AFTER sanitization
// (it assumes bare `<`, `>` and `&` were already escaped) and is applied at
// every emission point: each split chunk of a final reply, and each
// streaming message or edit after its truncation.
func balanceTags(s string) string {
	// Truncation artifact: a kept tag cut in half (a final "<" with no ">"
	// after it) would be sent as literal text and possibly mis-parsed.
	if i := strings.LastIndex(s, "<"); i >= 0 && !strings.Contains(s[i:], ">") {
		s = s[:i]
	}

	// Truncation artifact: an entity cut in half (a final "&" whose ";" was
	// cut away, e.g. "&am").  In sanitized text every "&" belongs to an
	// entity, so a trailing "&" without a ";" is always incomplete.
	if i := strings.LastIndex(s, "&"); i >= 0 && !strings.Contains(s[i:], ";") {
		s = s[:i]
	}

	var open []string // tag names of unclosed whitelisted openers, in order
	var b strings.Builder
	b.Grow(len(s) + 16)

	cursor := 0
	for _, loc := range allowedTagPattern.FindAllStringIndex(s, -1) {
		tag := s[loc[0]:loc[1]]
		b.WriteString(s[cursor:loc[0]])
		if tag[1] == '/' {
			if n := len(open); n > 0 && open[n-1] == tagName(tag) {
				open = open[:n-1]
				b.WriteString(tag)
			}
			// Unmatched closer: dropped — Telegram rejects stray end tags.
		} else {
			open = append(open, tagName(tag))
			b.WriteString(tag)
		}
		cursor = loc[1]
	}
	b.WriteString(s[cursor:])

	for i := len(open) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(open[i])
		b.WriteString(">")
	}

	return b.String()
}

// tagName extracts the lowercased tag name from a whitelisted tag like
// `<b>`, `</B>`, or `<span class="tg-spoiler">`.
func tagName(tag string) string {
	tag = strings.TrimPrefix(tag, "<")
	tag = strings.TrimPrefix(tag, "/")
	if i := strings.IndexAny(tag, " \t\n/>"); i >= 0 {
		tag = tag[:i]
	}

	return strings.ToLower(tag)
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
