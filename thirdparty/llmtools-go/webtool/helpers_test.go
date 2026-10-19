package webtool

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	testUserAgent = "webtool-test-agent/1.0"

	testRequestHeadersPath         = "/request-headers"
	testPlainPath                  = "/plain.txt"
	testLongTextPath               = "/long.txt"
	testHTMLPath                   = "/article.html"
	testEmptyTextPath              = "/empty.txt"
	testLatin1Path                 = "/latin1.txt"
	testImagePath                  = "/image.png"
	testGenericExtensionImagePath  = "/asset.png"
	testBinaryPath                 = "/unknown-binary.bin"
	testUTF8OctetStreamPath        = "/utf8-octet"
	testInvalidTextPath            = "/invalid-text.txt"
	testDownloadPath               = "/download"
	testGoodPDFPath                = "/good.pdf"
	testMalformedPDFPath           = "/broken.pdf"
	testStatusPath                 = "/status"
	testContentLengthTooLargePath  = "/content-length-too-large"
	testStreamingTooLargePath      = "/streaming-too-large"
	testRedirectPath               = "/redirect"
	testFinalPath                  = "/final"
	testRedirectLoopPath           = "/loop"
	testSVGPath                    = "/vector.svg"
	testNoContentTypeValidTextPath = "/no-content-type-valid-text"

	testPlainBody = "Plain text response.\nSecond line.\n"
	testLongBody  = "αβγδεζη"
	testFinalBody = "final destination"
	testPDFText   = "Hello Fetch PDF"
)

var (
	testPNGData = []byte{
		0x89, 0x50, 0x4e, 0x47,
		0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52,
	}

	testBinaryBody      = []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	testInvalidTextBody = []byte{0xff, 0xfe, 0x00}
)

func testConfig(overrides ...func(*webToolConfig)) webToolConfig {
	cfg := webToolConfig{
		userAgent:           testUserAgent,
		timeout:             5 * time.Second,
		maxFetchBytes:       1 << 20,
		maxRedirects:        defaultMaxRedirects,
		allowPrivateNetwork: true,
		proxyURL:            nil,
	}

	for _, override := range overrides {
		if override != nil {
			override(&cfg)
		}
	}
	return cfg
}

func newTestHTTPClient(t *testing.T, cfg webToolConfig) *http.Client {
	t.Helper()

	client, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatalf("newHTTPClient() error = %v", err)
	}
	return client
}

func newFetchTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc(testRequestHeadersPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		_, _ = fmt.Fprintf(
			w,
			"ua=%s\naccept=%s\npath=%s\nrawquery=%s\n",
			r.UserAgent(),
			r.Header.Get("Accept"),
			r.URL.Path,
			r.URL.RawQuery,
		)
	})

	mux.HandleFunc(testPlainPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(testPlainBody))
	})

	mux.HandleFunc(testLongTextPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(testLongBody))
	})

	mux.HandleFunc(testHTMLPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
	<head>
		<title>Readable Test Page</title>
		<style>.hidden { display: none; }</style>
		<script>shouldNotAppearInReadableText()</script>
	</head>
	<body>
		<nav>Navigation that is not the article.</nav>
		<main>
			<article>
				<h1>Readable Heading</h1>
				<p>Important paragraph for extraction.</p>
				<p>Another paragraph with <strong>bold text</strong>.</p>
			</article>
		</main>
	</body>
</html>`))
	})

	mux.HandleFunc(testEmptyTextPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(testLatin1Path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		_, _ = w.Write([]byte{'c', 'a', 'f', 0xe9, '\n'})
	})

	mux.HandleFunc(testImagePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="pixel:name.png"`)
		_, _ = w.Write(testPNGData)
	})

	mux.HandleFunc(testGenericExtensionImagePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(testPNGData)
	})

	mux.HandleFunc(testBinaryPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(testBinaryBody)
	})

	mux.HandleFunc(testUTF8OctetStreamPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("valid UTF-8 body served as octet-stream"))
	})

	mux.HandleFunc(testInvalidTextPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(testInvalidTextBody)
	})

	mux.HandleFunc(testDownloadPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="report:name.bin"`)
		_, _ = w.Write([]byte("download bytes"))
	})

	mux.HandleFunc(testGoodPDFPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", string(ioutil.MIMEApplicationPDF))
		_, _ = w.Write(buildTestMinimalPDF(testPDFText))
	})

	mux.HandleFunc(testMalformedPDFPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", string(ioutil.MIMEApplicationPDF))
		_, _ = w.Write([]byte("%PDF-1.4\nthis is not a complete pdf\n%%EOF\n"))
	})

	mux.HandleFunc(testStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "teapot", http.StatusTeapot)
	})

	mux.HandleFunc(testContentLengthTooLargePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(testStreamingTooLargePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)

		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("abc"))
			flusher.Flush()
			_, _ = w.Write([]byte("def"))
			flusher.Flush()
			return
		}

		_, _ = w.Write([]byte("abcdef"))
	})

	mux.HandleFunc(testRedirectPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, testFinalPath, http.StatusFound)
	})

	mux.HandleFunc(testFinalPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(testFinalBody))
	})

	mux.HandleFunc(testRedirectLoopPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, testRedirectLoopPath, http.StatusFound)
	})

	mux.HandleFunc(testSVGPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><text>Hello SVG</text></svg>`))
	})

	mux.HandleFunc(testNoContentTypeValidTextPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("valid text without explicit content type"))
	})

	return httptest.NewServer(mux)
}

func requireTextOutput(t *testing.T, outputs []spec.ToolOutputUnion) string {
	t.Helper()

	output := requireSingleOutput(t, outputs)
	if output.Kind != spec.ToolOutputKindText {
		t.Fatalf("output.Kind = %q, want %q; output = %#v", output.Kind, spec.ToolOutputKindText, output)
	}
	if output.TextItem == nil {
		t.Fatalf("output.TextItem is nil; output = %#v", output)
	}
	return output.TextItem.Text
}

func requireImageOutput(t *testing.T, outputs []spec.ToolOutputUnion) *spec.ToolOutputImage {
	t.Helper()

	output := requireSingleOutput(t, outputs)
	if output.Kind != spec.ToolOutputKindImage {
		t.Fatalf("output.Kind = %q, want %q; output = %#v", output.Kind, spec.ToolOutputKindImage, output)
	}
	if output.ImageItem == nil {
		t.Fatalf("output.ImageItem is nil; output = %#v", output)
	}
	return output.ImageItem
}

func requireFileOutput(t *testing.T, outputs []spec.ToolOutputUnion) *spec.ToolOutputFile {
	t.Helper()

	output := requireSingleOutput(t, outputs)
	if output.Kind != spec.ToolOutputKindFile {
		t.Fatalf("output.Kind = %q, want %q; output = %#v", output.Kind, spec.ToolOutputKindFile, output)
	}
	if output.FileItem == nil {
		t.Fatalf("output.FileItem is nil; output = %#v", output)
	}
	return output.FileItem
}

func requireSingleOutput(t *testing.T, outputs []spec.ToolOutputUnion) spec.ToolOutputUnion {
	t.Helper()

	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1; outputs = %#v", len(outputs), outputs)
	}
	return outputs[0]
}

func requireDecodedBase64(t *testing.T, encoded string) []byte {
	t.Helper()

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode error = %v; encoded = %q", err, encoded)
	}
	return data
}

func assertContains(t *testing.T, got, wantSubstr string) {
	t.Helper()

	if !strings.Contains(got, wantSubstr) {
		t.Fatalf("expected %q to contain %q", got, wantSubstr)
	}
}

func assertNotContains(t *testing.T, got, unwantedSubstr string) {
	t.Helper()

	if strings.Contains(got, unwantedSubstr) {
		t.Fatalf("expected %q not to contain %q", got, unwantedSubstr)
	}
}

func buildTestMinimalPDF(text string) []byte {
	escape := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, "(", `\(`)
		s = strings.ReplaceAll(s, ")", `\)`)
		return s
	}

	var content string
	if strings.TrimSpace(text) == "" {
		content = "BT\nET\n"
	} else {
		content = "BT\n/F1 24 Tf\n72 120 Td\n(" + escape(text) + ") Tj\nET\n"
	}

	var b []byte
	write := func(s string) {
		b = append(b, []byte(s)...)
	}

	offsets := make([]int, 6)
	write("%PDF-1.4\n")

	writeObj := func(i int, s string) {
		offsets[i] = len(b)
		write(s)
	}

	writeObj(1, "1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	writeObj(2, "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	writeObj(
		3,
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n",
	)
	writeObj(4, "4 0 obj\n<< /Length "+testItoa(len([]byte(content)))+" >>\nstream\n"+content+"endstream\nendobj\n")
	writeObj(5, "5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xrefStart := len(b)
	write("xref\n0 6\n")
	write("0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		write(testPad10(offsets[i]) + " 00000 n \n")
	}
	write("trailer\n<< /Size 6 /Root 1 0 R >>\n")
	write("startxref\n")
	write(testItoa(xrefStart) + "\n")
	write("%%EOF\n")

	return b
}

func testPad10(n int) string {
	s := testItoa(n)
	if len(s) >= 10 {
		return s
	}
	return strings.Repeat("0", 10-len(s)) + s
}

func testItoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [32]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + (n % 10))
		n /= 10
	}
	return string(buf[i:])
}
