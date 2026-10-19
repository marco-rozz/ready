package webtool

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/ioutil"
)

func TestNewWebToolOptions(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		wt, err := NewWebTool()
		if err != nil {
			t.Fatalf("NewWebTool() error = %v", err)
		}

		cfg, client := wt.snapshot()
		if cfg.userAgent != defaultUserAgent {
			t.Fatalf("default userAgent = %q, want %q", cfg.userAgent, defaultUserAgent)
		}
		if cfg.timeout != defaultHTTPTimeout {
			t.Fatalf("default timeout = %v, want %v", cfg.timeout, defaultHTTPTimeout)
		}
		if cfg.maxRedirects != defaultMaxRedirects {
			t.Fatalf("default maxRedirects = %d, want %d", cfg.maxRedirects, defaultMaxRedirects)
		}
		if cfg.allowPrivateNetwork {
			t.Fatal("default allowPrivateNetwork = true, want false")
		}
		if cfg.proxyURL != nil {
			t.Fatalf("default proxyURL = %v, want nil", cfg.proxyURL)
		}
		if client == nil {
			t.Fatal("client is nil")
		}
		if client.Timeout != defaultHTTPTimeout {
			t.Fatalf("client.Timeout = %v, want %v", client.Timeout, defaultHTTPTimeout)
		}
	})

	t.Run("valid options", func(t *testing.T) {
		wt, err := NewWebTool(
			nil,
			WithUserAgent("custom-agent/2.0"),
			WithHTTPTimeout(0),
			WithMaxFetchBytes(128),
			WithMaxRedirects(0),
			WithAllowPrivateNetwork(true),
			WithProxyURL("http://proxy.example.test:8080"),
		)
		if err != nil {
			t.Fatalf("NewWebTool(valid options) error = %v", err)
		}

		cfg, client := wt.snapshot()
		if cfg.userAgent != "custom-agent/2.0" {
			t.Fatalf("userAgent = %q, want custom-agent/2.0", cfg.userAgent)
		}
		if cfg.timeout != 0 {
			t.Fatalf("timeout = %v, want 0", cfg.timeout)
		}
		if cfg.maxFetchBytes != 128 {
			t.Fatalf("maxFetchBytes = %d, want 128", cfg.maxFetchBytes)
		}
		if cfg.maxRedirects != 0 {
			t.Fatalf("maxRedirects = %d, want 0", cfg.maxRedirects)
		}
		if !cfg.allowPrivateNetwork {
			t.Fatal("allowPrivateNetwork = false, want true")
		}
		if cfg.proxyURL == nil || cfg.proxyURL.String() != "http://proxy.example.test:8080" {
			t.Fatalf("proxyURL = %v, want http://proxy.example.test:8080", cfg.proxyURL)
		}
		if client == nil {
			t.Fatal("client is nil")
		}
		if client.Timeout != 0 {
			t.Fatalf("client.Timeout = %v, want 0", client.Timeout)
		}
	})

	invalidTests := []struct {
		name      string
		option    WebToolOption
		errSubstr string
	}{
		{name: "empty user agent", option: WithUserAgent("   "), errSubstr: "user-agent cannot be empty"},
		{
			name:      "too long user agent",
			option:    WithUserAgent(strings.Repeat("a", hardUserAgentLength+1)),
			errSubstr: "user-agent too long",
		},
		{name: "user agent CRLF", option: WithUserAgent("bad\r\nagent"), errSubstr: "user-agent cannot contain CR/LF"},
		{name: "negative timeout", option: WithHTTPTimeout(-time.Second), errSubstr: "http timeout cannot be negative"},
		{name: "max fetch zero", option: WithMaxFetchBytes(0), errSubstr: "max fetch bytes must be positive"},
		{
			name:      "max fetch too large",
			option:    WithMaxFetchBytes(hardFetchBytes + 1),
			errSubstr: "max fetch bytes too large",
		},
		{name: "negative redirects", option: WithMaxRedirects(-1), errSubstr: "max redirects cannot be negative"},
		{
			name:      "redirects too large",
			option:    WithMaxRedirects(hardRedirects + 1),
			errSubstr: "max redirects too large",
		},
		{name: "proxy parse error", option: WithProxyURL("://bad"), errSubstr: "invalid proxy url"},
		{
			name:      "proxy unsupported scheme",
			option:    WithProxyURL("socks5://proxy.example.test:1080"),
			errSubstr: "proxy url scheme must be http or https",
		},
		{
			name:      "proxy missing host",
			option:    WithProxyURL("http:///missing-host"),
			errSubstr: "proxy url must include host",
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewWebTool(tt.option)
			if err == nil {
				t.Fatalf("NewWebTool(%s) error = nil, want error", tt.name)
			}
			assertContains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestFetchURLToolReturnsClone(t *testing.T) {
	wt, err := NewWebTool()
	if err != nil {
		t.Fatalf("NewWebTool() error = %v", err)
	}

	tool1 := wt.FetchURLTool()
	tool2 := wt.FetchURLTool()

	if tool1.ID != fetchURLTool.ID {
		t.Fatalf("tool ID = %q, want %q", tool1.ID, fetchURLTool.ID)
	}
	if tool1.Slug != "fetchurl" {
		t.Fatalf("tool Slug = %q, want fetchurl", tool1.Slug)
	}
	if tool1.GoImpl.FuncID != fetchURLFuncID {
		t.Fatalf("tool GoImpl.FuncID = %q, want %q", tool1.GoImpl.FuncID, fetchURLFuncID)
	}

	tool1.DisplayName = "mutated"
	if len(tool1.Tags) > 0 {
		tool1.Tags[0] = "mutated"
	}

	if tool2.DisplayName == "mutated" {
		t.Fatal("FetchURLTool() did not clone DisplayName")
	}
	if len(tool2.Tags) > 0 && tool2.Tags[0] == "mutated" {
		t.Fatal("FetchURLTool() did not clone Tags")
	}
}

func TestWebToolFetchURLPublicMethod(t *testing.T) {
	server := newFetchTestServer(t)
	defer server.Close()

	t.Run("nil receiver", func(t *testing.T) {
		var wt *WebTool
		outputs, err := wt.FetchURL(t.Context(), FetchURLArgs{URL: server.URL + testPlainPath})
		if err == nil {
			t.Fatalf("FetchURL(nil receiver) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "nil web tool")
	})

	t.Run("default security blocks local private server", func(t *testing.T) {
		wt, err := NewWebTool()
		if err != nil {
			t.Fatalf("NewWebTool() error = %v", err)
		}

		outputs, err := wt.FetchURL(t.Context(), FetchURLArgs{URL: server.URL + testPlainPath})
		if err == nil {
			t.Fatalf("FetchURL(default security to local server) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "blocked ip address")
	})

	t.Run("context canceled", func(t *testing.T) {
		wt, err := NewWebTool(WithAllowPrivateNetwork(true))
		if err != nil {
			t.Fatalf("NewWebTool() error = %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testPlainPath})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("FetchURL(canceled) error = %v, want context.Canceled; outputs = %#v", err, outputs)
		}
	})
}

func TestWebToolFetchURLFunctionalLocalServer(t *testing.T) {
	server := newFetchTestServer(t)
	defer server.Close()

	wt, err := NewWebTool(
		WithAllowPrivateNetwork(true),
		WithHTTPTimeout(5*time.Second),
		WithMaxFetchBytes(1<<20),
	)
	if err != nil {
		t.Fatalf("NewWebTool() error = %v", err)
	}

	ctx := t.Context()

	t.Run("plain text auto", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testPlainPath})
		if err != nil {
			t.Fatalf("FetchURL(plain) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "Contents of "+server.URL+testPlainPath+":")
		assertContains(t, text, "Plain text response.")
		assertContains(t, text, "Second line.")
	})

	t.Run("plain text pagination by rune", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:       server.URL + testLongTextPath,
			MaxLength: 3,
		})
		if err != nil {
			t.Fatalf("FetchURL(long first page) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "αβγ")
		assertNotContains(t, text, "δεζη")
		assertContains(t, text, "[truncated: call fetchurl with startIndex=3]")

		outputs, err = wt.FetchURL(ctx, FetchURLArgs{
			URL:        server.URL + testLongTextPath,
			StartIndex: 3,
			MaxLength:  2,
		})
		if err != nil {
			t.Fatalf("FetchURL(long second page) error = %v", err)
		}

		text = requireTextOutput(t, outputs)
		assertContains(t, text, "δε")
		assertNotContains(t, text, "αβγ")

		outputs, err = wt.FetchURL(ctx, FetchURLArgs{
			URL:        server.URL + testLongTextPath,
			StartIndex: 99,
			MaxLength:  2,
		})
		if err != nil {
			t.Fatalf("FetchURL(long exhausted) error = %v", err)
		}

		text = requireTextOutput(t, outputs)
		assertContains(t, text, "No more content available for "+server.URL+testLongTextPath+".")
	})

	t.Run("html readable auto", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testHTMLPath})
		if err != nil {
			t.Fatalf("FetchURL(html readable) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "Readable Heading")
		assertContains(t, text, "Important paragraph for extraction")
		assertNotContains(t, text, "shouldNotAppearInReadableText")
	})

	t.Run("html raw", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL: server.URL + testHTMLPath,
			Raw: true,
		})
		if err != nil {
			t.Fatalf("FetchURL(html raw) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "<h1>Readable Heading</h1>")
		assertContains(t, text, "shouldNotAppearInReadableText()")
	})

	t.Run("latin1 text is decoded", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testLatin1Path})
		if err != nil {
			t.Fatalf("FetchURL(latin1) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "café")
	})

	t.Run("image auto returns image output", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testImagePath})
		if err != nil {
			t.Fatalf("FetchURL(image auto) error = %v", err)
		}

		image := requireImageOutput(t, outputs)
		if image.ImageName != "pixel-name.png" {
			t.Fatalf("image.ImageName = %q, want pixel-name.png", image.ImageName)
		}
		if image.ImageMIME != "image/png" {
			t.Fatalf("image.ImageMIME = %q, want image/png", image.ImageMIME)
		}
		if !bytes.Equal(requireDecodedBase64(t, image.ImageData), testPNGData) {
			t.Fatal("decoded image data mismatch")
		}
	})

	t.Run("image forced text errors", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testImagePath,
			Encoding: "text",
		})
		if err == nil {
			t.Fatalf("FetchURL(image text) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "is an image")
	})

	t.Run("plain text forced binary returns file output", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testPlainPath,
			Encoding: "binary",
		})
		if err != nil {
			t.Fatalf("FetchURL(plain binary) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "plain.txt" {
			t.Fatalf("file.FileName = %q, want plain.txt", file.FileName)
		}
		if file.FileMIME != "text/plain" {
			t.Fatalf("file.FileMIME = %q, want text/plain", file.FileMIME)
		}
		if got := string(requireDecodedBase64(t, file.FileData)); got != testPlainBody {
			t.Fatalf("decoded file data = %q, want %q", got, testPlainBody)
		}
	})

	t.Run("octet stream with valid utf8 auto returns text", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testUTF8OctetStreamPath})
		if err != nil {
			t.Fatalf("FetchURL(utf8 octet) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "valid UTF-8 body served as octet-stream")
	})

	t.Run("binary octet stream auto returns file", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testBinaryPath})
		if err != nil {
			t.Fatalf("FetchURL(binary octet) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "unknown-binary.bin" {
			t.Fatalf("file.FileName = %q, want unknown-binary.bin", file.FileName)
		}
		if file.FileMIME != "application/octet-stream" {
			t.Fatalf("file.FileMIME = %q, want application/octet-stream", file.FileMIME)
		}
		if !bytes.Equal(requireDecodedBase64(t, file.FileData), testBinaryBody) {
			t.Fatal("decoded binary file data mismatch")
		}
	})

	t.Run("binary octet stream forced text errors", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testBinaryPath,
			Encoding: "text",
		})
		if err == nil {
			t.Fatalf("FetchURL(binary text) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "is not text")
	})

	t.Run("invalid text auto falls back to file", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testInvalidTextPath})
		if err != nil {
			t.Fatalf("FetchURL(invalid text auto) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileMIME != "text/plain" {
			t.Fatalf("file.FileMIME = %q, want text/plain", file.FileMIME)
		}
		if !bytes.Equal(requireDecodedBase64(t, file.FileData), testInvalidTextBody) {
			t.Fatal("decoded invalid text fallback data mismatch")
		}
	})

	t.Run("invalid text forced text errors", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testInvalidTextPath,
			Encoding: "text",
		})
		if err == nil {
			t.Fatalf("FetchURL(invalid text forced text) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "response is not valid UTF-8 text")
	})

	t.Run("valid pdf auto extracts text", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testGoodPDFPath})
		if err != nil {
			t.Fatalf("FetchURL(valid pdf auto) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, testPDFText)
	})

	t.Run("malformed pdf auto falls back to file", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testMalformedPDFPath})
		if err != nil {
			t.Fatalf("FetchURL(malformed pdf auto) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "broken.pdf" {
			t.Fatalf("file.FileName = %q, want broken.pdf", file.FileName)
		}
		if file.FileMIME != string(ioutil.MIMEApplicationPDF) {
			t.Fatalf("file.FileMIME = %q, want application/pdf", file.FileMIME)
		}
	})

	t.Run("malformed pdf forced text errors", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testMalformedPDFPath,
			Encoding: "text",
		})
		if err == nil {
			t.Fatalf("FetchURL(malformed pdf text) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "extract pdf text")
	})

	t.Run("empty text auto falls back to file", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testEmptyTextPath})
		if err != nil {
			t.Fatalf("FetchURL(empty text auto) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "empty.txt" {
			t.Fatalf("file.FileName = %q, want empty.txt", file.FileName)
		}
		if file.FileMIME != "text/plain" {
			t.Fatalf("file.FileMIME = %q, want text/plain", file.FileMIME)
		}
		if got := requireDecodedBase64(t, file.FileData); len(got) != 0 {
			t.Fatalf("decoded empty fallback len = %d, want 0", len(got))
		}
	})

	t.Run("empty text forced text errors", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{
			URL:      server.URL + testEmptyTextPath,
			Encoding: "text",
		})
		if err == nil {
			t.Fatalf("FetchURL(empty text forced text) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "empty response body")
	})

	t.Run("svg is treated as text-like not image", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testSVGPath})
		if err != nil {
			t.Fatalf("FetchURL(svg) error = %v", err)
		}

		text := requireTextOutput(t, outputs)
		assertContains(t, text, "<svg")
		assertContains(t, text, "Hello SVG")
	})

	t.Run("generic content type infers image from extension", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testGenericExtensionImagePath})
		if err != nil {
			t.Fatalf("FetchURL(generic extension image) error = %v", err)
		}

		image := requireImageOutput(t, outputs)
		if image.ImageName != "asset.png" {
			t.Fatalf("image.ImageName = %q, want asset.png", image.ImageName)
		}
		if image.ImageMIME != "image/png" {
			t.Fatalf("image.ImageMIME = %q, want image/png", image.ImageMIME)
		}
	})

	t.Run("server status error", func(t *testing.T) {
		outputs, err := wt.FetchURL(ctx, FetchURLArgs{URL: server.URL + testStatusPath})
		if err == nil {
			t.Fatalf("FetchURL(status) error = nil, want error; outputs = %#v", outputs)
		}
		assertContains(t, err.Error(), "http status 418")
	})
}
