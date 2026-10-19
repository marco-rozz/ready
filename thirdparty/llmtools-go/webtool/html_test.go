package webtool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExtractReadableMarkdownFromHTML(t *testing.T) {
	t.Parallel()

	t.Run("empty html", func(t *testing.T) {
		t.Parallel()

		_, err := extractReadableMarkdownFromHTML(t.Context(), "https://example.test/empty", nil)
		if !errors.Is(err, ErrNoContentExtracted) {
			t.Fatalf("extractReadableMarkdownFromHTML(empty) error = %v, want ErrNoContentExtracted", err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := extractReadableMarkdownFromHTML(
			ctx,
			"https://example.test/canceled",
			[]byte("<html><body>hello</body></html>"),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("extractReadableMarkdownFromHTML(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("simple article extracts content", func(t *testing.T) {
		t.Parallel()

		body := strings.Repeat("This is the main article sentence. ", 20)
		html := []byte(`<!doctype html>
<html>
	<head><title>Test Article</title><script>hidden()</script></head>
	<body>
		<header>Site header</header>
		<article>
			<h1>Main Article Heading</h1>
			<p>` + body + `</p>
		</article>
	</body>
</html>`)

		got, err := extractReadableMarkdownFromHTML(t.Context(), "https://example.test/article", html)
		if err != nil {
			t.Fatalf("extractReadableMarkdownFromHTML() error = %v", err)
		}
		assertContains(t, got, "Main Article Heading")
		assertContains(t, got, "main article sentence")
		assertNotContains(t, got, "hidden()")
	})
}

func TestExtractMainContentHTMLWithTrafilatura(t *testing.T) {
	t.Parallel()

	t.Run("context canceled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := extractMainContentHTMLWithTrafilatura(
			ctx,
			"https://example.test/canceled",
			[]byte("<html><body>hello</body></html>"),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("extractMainContentHTMLWithTrafilatura(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("extracts non-empty readable document", func(t *testing.T) {
		t.Parallel()

		body := strings.Repeat("Readable extraction paragraph. ", 20)
		html := []byte(`<html><body><article><h1>Readable Document</h1><p>` + body + `</p></article></body></html>`)

		got, err := extractMainContentHTMLWithTrafilatura(t.Context(), "https://example.test/readable", html)
		if err != nil {
			t.Fatalf("extractMainContentHTMLWithTrafilatura() error = %v", err)
		}
		assertContains(t, got, "Readable Document")
		assertContains(t, got, "Readable extraction paragraph")
	})
}

func TestHTMLToPlainText(t *testing.T) {
	t.Parallel()

	t.Run("empty html", func(t *testing.T) {
		t.Parallel()

		_, err := htmlToPlainText(t.Context(), nil)
		if !errors.Is(err, ErrNoContentExtracted) {
			t.Fatalf("htmlToPlainText(empty) error = %v, want ErrNoContentExtracted", err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := htmlToPlainText(ctx, []byte("<html><body>hello</body></html>"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("htmlToPlainText(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("extracts visible text and skips unsafe or hidden containers", func(t *testing.T) {
		t.Parallel()

		html := []byte(`<!doctype html>
<html>
	<head>
		<title>Title Text</title>
		<style>.x { color: red; }</style>
		<script>alert("hidden script")</script>
	</head>
	<body>
		<h1>Heading</h1>
		<p>Line one<br>line two</p>
		<ul><li>One</li><li>Two</li></ul>
		<noscript>hidden noscript</noscript>
		<template>hidden template</template>
	</body>
</html>`)

		got, err := htmlToPlainText(t.Context(), html)
		if err != nil {
			t.Fatalf("htmlToPlainText() error = %v", err)
		}

		assertContains(t, got, "Title Text")
		assertContains(t, got, "Heading")
		assertContains(t, got, "Line one line two")
		assertContains(t, got, "One Two")
		assertNotContains(t, got, "hidden script")
		assertNotContains(t, got, "color: red")
		assertNotContains(t, got, "hidden noscript")
		assertNotContains(t, got, "hidden template")
	})

	t.Run("malformed html still extracts text", func(t *testing.T) {
		t.Parallel()

		got, err := htmlToPlainText(t.Context(), []byte(`<html><body><p>Open paragraph <b>bold`))
		if err != nil {
			t.Fatalf("htmlToPlainText(malformed) error = %v", err)
		}
		assertContains(t, got, "Open paragraph bold")
	})
}

func TestHTMLTextTokenHelpers(t *testing.T) {
	t.Parallel()

	t.Run("isBlockTag", func(t *testing.T) {
		t.Parallel()

		if !isBlockTag("article") {
			t.Fatal("isBlockTag(article) = false, want true")
		}
		if !isBlockTag("p") {
			t.Fatal("isBlockTag(p) = false, want true")
		}
		if isBlockTag("span") {
			t.Fatal("isBlockTag(span) = true, want false")
		}
		if isBlockTag("") {
			t.Fatal("isBlockTag(empty) = true, want false")
		}
	})

	t.Run("collapseSpaces", func(t *testing.T) {
		t.Parallel()

		got := collapseSpaces(" \talpha\n beta\r\n gamma\u00a0delta ")
		want := "alpha beta gamma delta"
		if got != want {
			t.Fatalf("collapseSpaces() = %q, want %q", got, want)
		}
		if got := collapseSpaces(" \t\n "); got != "" {
			t.Fatalf("collapseSpaces(blank) = %q, want empty", got)
		}
	})

	t.Run("normalizeTextTokens", func(t *testing.T) {
		t.Parallel()

		got := normalizeTextTokens([]string{"", " hello ", "world", "\n", "\n", "again", "  "})
		want := "hello world again"
		if got != want {
			t.Fatalf("normalizeTextTokens() = %q, want %q", got, want)
		}
		if got := normalizeTextTokens(nil); got != "" {
			t.Fatalf("normalizeTextTokens(nil) = %q, want empty", got)
		}
	})
}
