package webtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	html2md "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/markusmobius/go-trafilatura/v2"
	"golang.org/x/net/html"
)

var ErrNoContentExtracted = errors.New("no content extracted")

// extractReadableMarkdownFromHTML extracts main readable HTML content and
// converts it to compact Markdown suitable for LLM context.
func extractReadableMarkdownFromHTML(ctx context.Context, rawURL string, htmlBytes []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(htmlBytes) == 0 {
		return "", ErrNoContentExtracted
	}

	cleanedHTML, err := extractMainContentHTMLWithTrafilatura(ctx, rawURL, htmlBytes)
	if err == nil {
		cleanedHTML = strings.TrimSpace(cleanedHTML)
		if cleanedHTML != "" {
			markdown, mdErr := html2md.ConvertString(cleanedHTML)
			if mdErr == nil {
				markdown = strings.TrimSpace(markdown)
				if markdown != "" {
					return markdown, nil
				}
			}
		}
	}

	plain, plainErr := htmlToPlainText(ctx, htmlBytes)
	if plainErr == nil {
		plain = strings.TrimSpace(plain)
		if plain != "" {
			return plain, nil
		}
	}

	if err != nil {
		return "", fmt.Errorf("readable html extraction failed: %w", err)
	}
	if plainErr != nil {
		return "", fmt.Errorf("plain html extraction failed: %w", plainErr)
	}
	return "", ErrNoContentExtracted
}

func extractMainContentHTMLWithTrafilatura(ctx context.Context, rawURL string, htmlBytes []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var parsedURL *url.URL
	if u, err := url.Parse(rawURL); err == nil {
		parsedURL = u
	}

	opts := trafilatura.Options{
		IncludeImages:  false,
		OriginalURL:    parsedURL,
		EnableFallback: true,
	}

	result, err := trafilatura.Extract(bytes.NewReader(htmlBytes), opts)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", ErrNoContentExtracted
	}

	doc := trafilatura.CreateReadableDocument(result)
	if doc == nil {
		return "", ErrNoContentExtracted
	}

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "", ErrNoContentExtracted
	}
	return out, nil
}

// htmlToPlainText is a conservative fallback extractor. It intentionally skips
// scripts/styles and normalizes whitespace.
func htmlToPlainText(ctx context.Context, htmlBytes []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(htmlBytes) == 0 {
		return "", ErrNoContentExtracted
	}

	doc, err := html.Parse(bytes.NewReader(htmlBytes))
	if err != nil {
		return "", err
	}

	var tokens []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil || ctx.Err() != nil {
			return
		}

		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" || tag == "noscript" || tag == "template" {
				return
			}
			if tag == "br" {
				tokens = append(tokens, "\n")
			}
			if isBlockTag(tag) {
				tokens = append(tokens, "\n")
			}
		}

		if n.Type == html.TextNode {
			text := collapseSpaces(n.Data)
			if text != "" {
				tokens = append(tokens, text)
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}

		if n.Type == html.ElementNode && isBlockTag(strings.ToLower(n.Data)) {
			tokens = append(tokens, "\n")
		}
	}
	walk(doc)

	if err := ctx.Err(); err != nil {
		return "", err
	}

	out := normalizeTextTokens(tokens)
	if out == "" {
		return "", ErrNoContentExtracted
	}
	return out, nil
}

func isBlockTag(tag string) bool {
	switch tag {
	case "address",
		"article",
		"aside",
		"blockquote",
		"body",
		"dd",
		"details",
		"dialog",
		"div",
		"dl",
		"dt",
		"fieldset",
		"figcaption",
		"figure",
		"footer",
		"form",
		"h1",
		"h2",
		"h3",
		"h4",
		"h5",
		"h6",
		"header",
		"hgroup",
		"hr",
		"li",
		"main",
		"nav",
		"ol",
		"p",
		"pre",
		"section",
		"table",
		"tr",
		"ul":
		return true
	default:
		return false
	}
}

func collapseSpaces(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

func normalizeTextTokens(tokens []string) string {
	var b strings.Builder
	newlines := 0

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		if token == "\n" {
			if b.Len() > 0 && newlines < 2 {
				b.WriteByte('\n')
				newlines++
			}
			continue
		}

		if b.Len() > 0 && newlines == 0 {
			b.WriteByte(' ')
		}
		b.WriteString(token)
		newlines = 0
	}

	return strings.TrimSpace(b.String())
}
