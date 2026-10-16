// Package mdrender is a pure glamour v2 wrapper for the `ready chat` client.
//
// It owns markdown rendering (glamour TermRenderer construction with a
// client-resolved style and word-wrap width), markdown sanitisation (closing
// open code fences before render), and the client-side redaction guard
// (internal/privacy.Sanitizer.Redact applied to the raw markdown text before
// every glamour.Render — never to rendered ANSI).
//
// The package has NO dependency on the terminal-UI framework (Bubble Tea) so
// that it can be reused by both the progressive TUI path (internal/tui) and
// the one-shot `--exec`/stdout paths in cmd/chat.go. Callers resolve the
// glamour style (dark/light/notty/raw) themselves: glamour v2 dropped
// WithAutoStyle, so mdrender performs no terminal queries of its own.
//
// On a glamour render error, Render falls back to the already-redacted and
// sanitised raw text — never the un-redacted input, never a blank viewport.
package mdrender
