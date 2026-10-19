package webtool

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

func TestNormalizeFetchEncoding(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    FetchURLEncoding
		wantErr bool
	}{
		{name: "empty defaults to auto", input: "", want: FetchEncodingAuto},
		{name: "spaces default to auto", input: "   ", want: FetchEncodingAuto},
		{name: "auto lowercase", input: "auto", want: FetchEncodingAuto},
		{name: "auto mixed case", input: " Auto ", want: FetchEncodingAuto},
		{name: "text", input: "TEXT", want: FetchEncodingText},
		{name: "binary", input: " binary ", want: FetchEncodingBinary},
		{name: "invalid", input: "base64", wantErr: true},
		{name: "invalid with whitespace", input: " text/html ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeFetchEncoding(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeFetchEncoding(%q) error = nil, want error", tt.input)
				}
				assertContains(t, err.Error(), `encoding must be "auto", "text", or "binary"`)
				return
			}

			if err != nil {
				t.Fatalf("normalizeFetchEncoding(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeFetchEncoding(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeMaxLength(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  int
	}{
		{name: "zero uses default", input: 0, want: defaultFetchMaxLength},
		{name: "negative uses default", input: -1, want: defaultFetchMaxLength},
		{name: "one", input: 1, want: 1},
		{name: "normal", input: 1234, want: 1234},
		{name: "max", input: hardFetchMaxLength, want: hardFetchMaxLength},
		{name: "above max clamps", input: hardFetchMaxLength + 1, want: hardFetchMaxLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeMaxLength(tt.input); got != tt.want {
				t.Fatalf("normalizeMaxLength(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeTextBytes(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		contentType string
		want        string
		wantErr     bool
		errSubstr   string
	}{
		{
			name:        "empty returns empty string",
			data:        nil,
			contentType: "text/plain",
			want:        "",
		},
		{
			name:        "valid utf8",
			data:        []byte("hello αβ"),
			contentType: "text/plain; charset=utf-8",
			want:        "hello αβ",
		},
		{
			name:        "ascii treated as utf8",
			data:        []byte("plain ascii"),
			contentType: "text/plain; charset=us-ascii",
			want:        "plain ascii",
		},
		{
			name:        "latin1 charset is decoded",
			data:        []byte{'c', 'a', 'f', 0xe9},
			contentType: "text/plain; charset=iso-8859-1",
			want:        "café",
		},
		{
			name:        "invalid utf8 without nul is made valid",
			data:        []byte{0xff, 'a'},
			contentType: "text/plain",
			want:        "\uFFFDa",
		},
		{
			name:        "unknown charset falls back to valid utf8 conversion",
			data:        []byte{0xff, 'b'},
			contentType: "text/plain; charset=not-a-real-charset",
			want:        "\uFFFDb",
		},
		{
			name:        "invalid utf8 with nul is rejected as binary",
			data:        []byte{0xff, 0x00, 'x'},
			contentType: "text/plain",
			wantErr:     true,
			errSubstr:   "response is not valid UTF-8 text",
		},
		{
			name:        "malformed content type still falls back",
			data:        []byte("ok"),
			contentType: "text/plain; charset",
			want:        "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeTextBytes(tt.data, tt.contentType)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeTextBytes() error = nil, want error")
				}
				if tt.errSubstr != "" {
					assertContains(t, err.Error(), tt.errSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("decodeTextBytes() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("decodeTextBytes() = %q, want %q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("decodeTextBytes() returned invalid UTF-8: %q", got)
			}
		})
	}
}

func TestIsUTF8Charset(t *testing.T) {
	tests := []struct {
		label string
		want  bool
	}{
		{label: "utf-8", want: true},
		{label: "UTF8", want: true},
		{label: " us-ascii ", want: true},
		{label: "ascii", want: true},
		{label: "iso-8859-1", want: false},
		{label: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if got := isUTF8Charset(tt.label); got != tt.want {
				t.Fatalf("isUTF8Charset(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

func TestTextFromResponse(t *testing.T) {
	ctx := t.Context()

	t.Run("plain text trims whitespace", func(t *testing.T) {
		got, err := textFromResponse(
			ctx,
			"https://example.test/plain.txt",
			"text/plain",
			"text/plain",
			[]byte("  hello\n"),
			false,
		)
		if err != nil {
			t.Fatalf("textFromResponse() error = %v", err)
		}
		if got != "hello" {
			t.Fatalf("textFromResponse() = %q, want %q", got, "hello")
		}
	})

	t.Run("plain text empty body", func(t *testing.T) {
		_, err := textFromResponse(ctx, "https://example.test/empty.txt", "text/plain", "text/plain", nil, false)
		if err == nil {
			t.Fatal("textFromResponse(empty body) error = nil, want error")
		}
		assertContains(t, err.Error(), "empty response body")
	})

	t.Run("plain text whitespace only", func(t *testing.T) {
		_, err := textFromResponse(
			ctx,
			"https://example.test/blank.txt",
			"text/plain",
			"text/plain",
			[]byte(" \n\t "),
			false,
		)
		if err == nil {
			t.Fatal("textFromResponse(whitespace) error = nil, want error")
		}
		assertContains(t, err.Error(), "empty text response")
	})

	t.Run("latin1 text", func(t *testing.T) {
		got, err := textFromResponse(
			ctx,
			"https://example.test/latin1.txt",
			"text/plain; charset=iso-8859-1",
			"text/plain",
			[]byte{'c', 'a', 'f', 0xe9},
			false,
		)
		if err != nil {
			t.Fatalf("textFromResponse(latin1) error = %v", err)
		}
		if got != "café" {
			t.Fatalf("textFromResponse(latin1) = %q, want café", got)
		}
	})

	t.Run("invalid text with nul", func(t *testing.T) {
		_, err := textFromResponse(
			ctx,
			"https://example.test/bad.txt",
			"text/plain",
			"text/plain",
			[]byte{0xff, 0x00},
			false,
		)
		if err == nil {
			t.Fatal("textFromResponse(invalid) error = nil, want error")
		}
		assertContains(t, err.Error(), "response is not valid UTF-8 text")
	})

	t.Run("html raw returns decoded source", func(t *testing.T) {
		html := []byte(`<html><body><h1>Raw Heading</h1><script>kept in raw</script></body></html>`)
		got, err := textFromResponse(
			ctx,
			"https://example.test/raw.html",
			string(ioutil.MIMETextHTML),
			string(ioutil.MIMEBaseTextHTML),
			html,
			true,
		)
		if err != nil {
			t.Fatalf("textFromResponse(html raw) error = %v", err)
		}
		assertContains(t, got, "<h1>Raw Heading</h1>")
		assertContains(t, got, "kept in raw")
	})

	t.Run("html readable extracts visible content", func(t *testing.T) {
		html := []byte(
			`<!doctype html><html><head><script>hiddenScript()</script></head><body><main><h1>Visible Heading</h1><p>Visible paragraph.</p></main></body></html>`,
		)
		got, err := textFromResponse(
			ctx,
			"https://example.test/readable.html",
			string(ioutil.MIMEBaseTextHTML),
			string(ioutil.MIMEBaseTextHTML),
			html,
			false,
		)
		if err != nil {
			t.Fatalf("textFromResponse(html readable) error = %v", err)
		}
		assertContains(t, got, "Visible Heading")
		assertContains(t, got, "Visible paragraph")
		assertNotContains(t, got, "hiddenScript")
	})
}

func TestTextOutputsAndRuneSlicing(t *testing.T) {
	t.Run("text output slices by rune and reports continuation", func(t *testing.T) {
		outputs := textOutputs("https://example.test/doc", "aβcd🙂ef", 1, 3)
		text := requireTextOutput(t, outputs)

		assertContains(t, text, "Contents of https://example.test/doc:")
		assertContains(t, text, "βcd")
		assertContains(t, text, "[truncated: call fetchurl with startIndex=4]")
		assertNotContains(t, text, "🙂ef")
	})

	t.Run("start beyond end reports no more content", func(t *testing.T) {
		outputs := textOutputs("https://example.test/doc", "abc", 99, 10)
		text := requireTextOutput(t, outputs)

		assertContains(t, text, "No more content available for https://example.test/doc.")
	})

	t.Run("sliceStringByRune handles negative start and unicode", func(t *testing.T) {
		part, total, nextStart, hasMore := sliceStringByRune("αβγ", -2, 2)
		if part != "αβ" {
			t.Fatalf("part = %q, want %q", part, "αβ")
		}
		if total != 3 {
			t.Fatalf("total = %d, want 3", total)
		}
		if nextStart != 2 {
			t.Fatalf("nextStart = %d, want 2", nextStart)
		}
		if !hasMore {
			t.Fatal("hasMore = false, want true")
		}
	})

	t.Run("sliceStringByRune uses default max length when max is non-positive", func(t *testing.T) {
		part, total, nextStart, hasMore := sliceStringByRune("abc", 0, 0)
		if part != "abc" || total != 3 || nextStart != 3 || hasMore {
			t.Fatalf(
				"sliceStringByRune(default max) = (%q, %d, %d, %v), want (%q, 3, 3, false)",
				part,
				total,
				nextStart,
				hasMore,
				"abc",
			)
		}
	})

	t.Run("byteIndexForRune", func(t *testing.T) {
		s := "aβ🙂z"
		if got := byteIndexForRune(s, 0); got != 0 {
			t.Fatalf("byteIndexForRune(target=0) = %d, want 0", got)
		}
		if got, want := byteIndexForRune(s, 2), len("aβ"); got != want {
			t.Fatalf("byteIndexForRune(target=2) = %d, want %d", got, want)
		}
		if got, want := byteIndexForRune(s, 99), len(s); got != want {
			t.Fatalf("byteIndexForRune(target=99) = %d, want %d", got, want)
		}
	})
}

func TestBinaryOutputs(t *testing.T) {
	t.Run("image output uses image kind and sanitized content disposition name", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Disposition", `attachment; filename="pixel:name.png"`)

		outputs, err := binaryOutputs("https://example.test/assets/pixel.png", "image/png", header, testPNGData)
		if err != nil {
			t.Fatalf("binaryOutputs(image) error = %v", err)
		}

		image := requireImageOutput(t, outputs)
		if image.Detail != spec.ImageDetailAuto {
			t.Fatalf("image.Detail = %q, want %q", image.Detail, spec.ImageDetailAuto)
		}
		if image.ImageName != "pixel-name.png" {
			t.Fatalf("image.ImageName = %q, want %q", image.ImageName, "pixel-name.png")
		}
		if image.ImageMIME != "image/png" {
			t.Fatalf("image.ImageMIME = %q, want image/png", image.ImageMIME)
		}
		if got := image.ImageData; got != base64.StdEncoding.EncodeToString(testPNGData) {
			t.Fatalf("image.ImageData = %q, want base64 of test PNG", got)
		}
		if !bytes.Equal(requireDecodedBase64(t, image.ImageData), testPNGData) {
			t.Fatal("decoded image data mismatch")
		}
	})

	t.Run("file output uses fallback name from content type", func(t *testing.T) {
		outputs, err := binaryOutputs(
			"https://example.test",
			string(ioutil.MIMEApplicationJSON),
			nil,
			[]byte(`{"ok":true}`),
		)
		if err != nil {
			t.Fatalf("binaryOutputs(file) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "download.json" {
			t.Fatalf("file.FileName = %q, want download.json", file.FileName)
		}
		if file.FileMIME != string(ioutil.MIMEApplicationJSON) {
			t.Fatalf("file.FileMIME = %q, want application/json", file.FileMIME)
		}
		if string(requireDecodedBase64(t, file.FileData)) != `{"ok":true}` {
			t.Fatalf("decoded file data mismatch")
		}
	})

	t.Run("empty content type falls back to octet stream", func(t *testing.T) {
		outputs, err := binaryOutputs("https://example.test/data.bin", "", nil, []byte("abc"))
		if err != nil {
			t.Fatalf("binaryOutputs(empty content type) error = %v", err)
		}

		file := requireFileOutput(t, outputs)
		if file.FileName != "data.bin" {
			t.Fatalf("file.FileName = %q, want data.bin", file.FileName)
		}
		if file.FileMIME != "application/octet-stream" {
			t.Fatalf("file.FileMIME = %q, want application/octet-stream", file.FileMIME)
		}
	})
}

func TestInferContentType(t *testing.T) {
	tests := []struct {
		name        string
		headerCT    string
		rawURL      string
		data        []byte
		want        string
		wantGeneric bool
	}{
		{
			name:     "specific header wins",
			headerCT: "Text/HTML; charset=utf-8",
			rawURL:   "https://example.test/file.bin",
			data:     nil,
			want:     string(ioutil.MIMEBaseTextHTML),
		},
		{
			name:     "generic header falls back to extension",
			headerCT: "application/octet-stream",
			rawURL:   "https://example.test/data.json?download=1",
			data:     nil,
			want:     string(ioutil.MIMEApplicationJSON),
		},
		{
			name:     "extension is lowercased",
			headerCT: "",
			rawURL:   "https://example.test/IMAGE.PNG",
			data:     nil,
			want:     "image/png",
		},
		{
			name:     "sniffs png",
			headerCT: "",
			rawURL:   "https://example.test/download",
			data:     testPNGData,
			want:     "image/png",
		},
		{
			name:     "sniffs text",
			headerCT: "",
			rawURL:   "https://example.test/download",
			data:     []byte("hello"),
			want:     "text/plain",
		},
		{
			name:     "generic fallback",
			headerCT: "application/octet-stream",
			rawURL:   "https://example.test/download",
			data:     nil,
			want:     "application/octet-stream",
		},
		{
			name:     "binary-looking header is generic and extension wins",
			headerCT: "application/binary",
			rawURL:   "https://example.test/document.pdf",
			data:     nil,
			want:     "application/pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferContentType(tt.headerCT, tt.rawURL, tt.data); got != tt.want {
				t.Fatalf(
					"inferContentType(%q, %q, dataLen=%d) = %q, want %q",
					tt.headerCT,
					tt.rawURL,
					len(tt.data),
					got,
					tt.want,
				)
			}
		})
	}
}

func TestContentTypeHelpers(t *testing.T) {
	if got := normalizeContentType(" Text/HTML; charset=UTF-8 "); got != string(ioutil.MIMEBaseTextHTML) {
		t.Fatalf("normalizeContentType() = %q, want text/html", got)
	}
	if got := normalizeContentType("application/json; charset"); got != string(ioutil.MIMEApplicationJSON) {
		t.Fatalf("normalizeContentType(malformed params) = %q, want application/json", got)
	}
	if !isGenericBinaryContentType("") {
		t.Fatal("isGenericBinaryContentType(empty) = false, want true")
	}
	if !isGenericBinaryContentType("application/octet-stream") {
		t.Fatal("isGenericBinaryContentType(octet-stream) = false, want true")
	}
	if isGenericBinaryContentType(string(ioutil.MIMEApplicationJSON)) {
		t.Fatal("isGenericBinaryContentType(json) = true, want false")
	}
	if !isPDFContentType("application/pdf; charset=binary", "https://example.test/x") {
		t.Fatal("isPDFContentType(application/pdf) = false, want true")
	}
	if !isPDFContentType("application/octet-stream", "https://example.test/file.PDF?x=1") {
		t.Fatal("isPDFContentType(.PDF URL) = false, want true")
	}
	if !isImageContentType("image/png") {
		t.Fatal("isImageContentType(image/png) = false, want true")
	}
	if isImageContentType("image/svg+xml") {
		t.Fatal("isImageContentType(svg) = true, want false")
	}
	if !isHTMLContentType("application/xhtml+xml") {
		t.Fatal("isHTMLContentType(xhtml) = false, want true")
	}
	if !isHTMLContentType("application/vnd.custom-html") {
		t.Fatal("isHTMLContentType(custom html) = false, want true")
	}
	if !isTextLikeContentType("text/csv") {
		t.Fatal("isTextLikeContentType(text/csv) = false, want true")
	}
	if !isTextLikeContentType("application/ld+json") {
		t.Fatal("isTextLikeContentType(+json) = false, want true")
	}
	if !isTextLikeContentType("application/xml") {
		t.Fatal("isTextLikeContentType(xml) = false, want true")
	}
	if !isTextLikeContentType("image/svg+xml") {
		t.Fatal("isTextLikeContentType(svg) = false, want true")
	}
	if isTextLikeContentType("application/pdf") {
		t.Fatal("isTextLikeContentType(pdf) = true, want false")
	}
}

func TestContentLookingHelpers(t *testing.T) {
	if !looksLikeHTML([]byte("  <!doctype html><html><body>x</body></html>")) {
		t.Fatal("looksLikeHTML(document) = false, want true")
	}
	if !looksLikeHTML([]byte("<title>Only title</title>")) {
		t.Fatal("looksLikeHTML(title) = false, want true")
	}
	if looksLikeHTML([]byte("plain text")) {
		t.Fatal("looksLikeHTML(plain) = true, want false")
	}
	if looksLikeHTML(nil) {
		t.Fatal("looksLikeHTML(nil) = true, want false")
	}
	if !looksLikeUTF8Text([]byte("hello αβ")) {
		t.Fatal("looksLikeUTF8Text(valid) = false, want true")
	}
	if looksLikeUTF8Text([]byte{'a', 0x00, 'b'}) {
		t.Fatal("looksLikeUTF8Text(nul) = true, want false")
	}
	if looksLikeUTF8Text([]byte{0xff}) {
		t.Fatal("looksLikeUTF8Text(invalid) = true, want false")
	}
	if !hasNULByte([]byte{'a', 0, 'b'}) {
		t.Fatal("hasNULByte() = false, want true")
	}
	if hasNULByte([]byte("abc")) {
		t.Fatal("hasNULByte(text) = true, want false")
	}
}

func TestURLAndFilenameHelpers(t *testing.T) {
	t.Run("extensionFromURL", func(t *testing.T) {
		if got := extensionFromURL("https://example.test/a/b.File.TXT?name=.pdf#frag"); got != ".txt" {
			t.Fatalf("extensionFromURL(url) = %q, want .txt", got)
		}
		if got := extensionFromURL("local.NAME.JSON"); got != ".json" {
			t.Fatalf("extensionFromURL(raw path fallback) = %q, want .json", got)
		}
	})

	t.Run("filename from content disposition", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Disposition", `attachment; filename="bad:path\name/ok.txt"`)
		got := filenameFromURL("https://example.test/files/fallback.txt", "text/plain", header)
		if got != "bad-path-name-ok.txt" {
			t.Fatalf("filenameFromURL(content disposition) = %q, want bad-path-name-ok.txt", got)
		}
	})

	t.Run("filename from escaped URL path", func(t *testing.T) {
		got := filenameFromURL("https://example.test/files/report%202024.txt?download=1", "text/plain", nil)
		if got != "report 2024.txt" {
			t.Fatalf("filenameFromURL(url path) = %q, want report 2024.txt", got)
		}
	})

	t.Run("filename fallbacks", func(t *testing.T) {
		if got := filenameFromURL("https://example.test", "image/png", nil); got != "image.png" {
			t.Fatalf("image fallback = %q, want image.png", got)
		}
		if got := filenameFromURL("https://example.test", "application/pdf", nil); got != "document.pdf" {
			t.Fatalf("pdf fallback = %q, want document.pdf", got)
		}
		if got := filenameFromURL(
			"https://example.test",
			string(ioutil.MIMEApplicationJSON),
			nil,
		); got != "download.json" {
			t.Fatalf("generic fallback = %q, want download.json", got)
		}
	})

	t.Run("sanitizeFilename", func(t *testing.T) {
		if got := sanitizeFilename("  ../bad:path\\file/name.txt.\n"); got != "-bad-path-file-name.txt" {
			t.Fatalf("sanitizeFilename() = %q, want -bad-path-file-name.txt", got)
		}
		if got := sanitizeFilename("a\x00b\x7fc"); got != "abc" {
			t.Fatalf("sanitizeFilename(control chars) = %q, want abc", got)
		}
		if got := sanitizeFilename(strings.Repeat("a", 200)); len(got) != 160 {
			t.Fatalf("sanitizeFilename(long) len = %d, want 160", len(got))
		}
		if got := sanitizeFilename(" . \t "); got != "" {
			t.Fatalf("sanitizeFilename(blank after trim) = %q, want empty", got)
		}
	})

	t.Run("extensionForContentType", func(t *testing.T) {
		if got := extensionForContentType("image/png; charset=utf-8"); got != ".png" {
			t.Fatalf("extensionForContentType(image/png) = %q, want .png", got)
		}
		if got := extensionForContentType("unknown/unknown"); got != "" {
			t.Fatalf("extensionForContentType(unknown) = %q, want empty", got)
		}
	})
}

func TestFetchURLInputValidationBeforeAndDuringFetch(t *testing.T) {
	server := newFetchTestServer(t)
	defer server.Close()

	cfg := testConfig()
	client := newTestHTTPClient(t, cfg)
	ctx := t.Context()

	tests := []struct {
		name      string
		args      FetchURLArgs
		client    *http.Client
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "invalid encoding",
			args:      FetchURLArgs{URL: server.URL + testPlainPath, Encoding: "base64"},
			client:    client,
			wantErr:   true,
			errSubstr: "encoding must be",
		},
		{
			name:      "negative start index",
			args:      FetchURLArgs{URL: server.URL + testPlainPath, StartIndex: -1},
			client:    client,
			wantErr:   true,
			errSubstr: "startIndex must be >= 0",
		},
		{
			name:      "nil client",
			args:      FetchURLArgs{URL: server.URL + testPlainPath},
			client:    nil,
			wantErr:   true,
			errSubstr: "nil http client",
		},
		{
			name:      "relative URL",
			args:      FetchURLArgs{URL: "/relative"},
			client:    client,
			wantErr:   true,
			errSubstr: "must be absolute",
		},
		{
			name:      "forced text for non-text binary",
			args:      FetchURLArgs{URL: server.URL + testBinaryPath, Encoding: "text"},
			client:    client,
			wantErr:   true,
			errSubstr: "is not text",
		},
		{
			name:   "valid fetch",
			args:   FetchURLArgs{URL: server.URL + testPlainPath},
			client: client,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputs, err := fetchURL(ctx, tt.args, cfg, tt.client)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("fetchURL() error = nil, want error; outputs = %#v", outputs)
				}
				if tt.errSubstr != "" {
					assertContains(t, err.Error(), tt.errSubstr)
				}
				return
			}

			if err != nil {
				t.Fatalf("fetchURL() error = %v", err)
			}
			_ = requireTextOutput(t, outputs)
		})
	}
}
