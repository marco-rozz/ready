package pdfutil

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const errEmptyPDFStr = "empty PDF text after extraction"

func TestExtractPDFTextSafe_TableDriven(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()

	happyPath := writeTempFile(t, dir, "hello.pdf", buildMinimalPDF("Hello PDF"))
	emptyTextPath := writeTempFile(t, dir, "empty.pdf", buildMinimalPDF(""))
	notPDFPath := writeTempFile(t, dir, "not.pdf", []byte("definitely not a pdf"))

	tests := []struct {
		name      string
		path      string
		maxBytes  int
		wantText  string
		wantErr   bool
		errSubstr string
		checkFn   func(t *testing.T, got string)
	}{
		{
			name:     "happy",
			path:     happyPath,
			maxBytes: 1 << 20,
			wantText: "Hello PDF",
		},
		{
			name:     "byte limit truncates but still returns non-empty",
			path:     happyPath,
			maxBytes: 4,
			wantErr:  false,
			checkFn: func(t *testing.T, got string) {
				t.Helper()
				if got == "" {
					t.Fatalf("expected non-empty text")
				}
				if len(got) > 4 {
					t.Fatalf("expected len(got)<=4, got %d (%q)", len(got), got)
				}
				if !strings.HasPrefix("Hello PDF", got) { //nolint:gocritic // The argOrder is correct.
					t.Fatalf("expected %q to be prefix of %q", got, "Hello PDF")
				}
			},
		},
		{
			name:      "empty text pdf => specific empty error",
			path:      emptyTextPath,
			maxBytes:  1 << 20,
			wantErr:   true,
			errSubstr: errEmptyPDFStr,
		},
		{
			name:     "missing file => open error",
			path:     filepath.Join(dir, "missing.pdf"),
			maxBytes: 1 << 20,
			wantErr:  true,
		},
		{
			name:     "not a pdf => open/parse error",
			path:     notPDFPath,
			maxBytes: 1 << 20,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractPDFTextSafe(ctx, tt.path, tt.maxBytes)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; text=%q", got)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("expected err to contain %q, got %q", tt.errSubstr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantText != "" && got != tt.wantText {
				t.Fatalf("text mismatch: got %q want %q", got, tt.wantText)
			}
			if tt.checkFn != nil {
				tt.checkFn(t, got)
			}
		})
	}
}

//nolint:godot // Commented test.
// This test is optional, but useful for diagnosing fixture/library changes.
// It asserts we can round-trip a known-good PDF payload (base64) if you prefer not to generate PDFs.
//
// NOTE: Disabled by default; uncomment to use.
// func TestExtractPDFTextSafe_Base64Fixture(t *testing.T) {
// 	t.Parallel()
// 	ctx := context.Background()
// 	dir := t.TempDir()
//
// 	// Replace with a known-good, real PDF if needed.
// 	const b64 = "JVBERi0xLjQKJS4uLg==" // placeholder
// 	pdfBytes, _ := base64.StdEncoding.DecodeString(b64)
// 	path := writeTempFile(t, dir, "fixture.pdf", pdfBytes)
//
// 	_, err := ExtractPDFTextSafe(ctx, path, 1<<20)
// 	if err == nil {
// 		t.Fatalf("expected error with placeholder fixture")
// 	}
// }

func TestBuildMinimalPDF_Sanity(t *testing.T) {
	// Sanity check our generated PDFs have a PDF header and EOF marker.
	p := buildMinimalPDF("Hello")
	if !strings.HasPrefix(string(p), "%PDF-") {
		t.Fatalf("expected PDF header, got %q", string(p[:min(16, len(p))]))
	}
	if !strings.Contains(string(p), "%%EOF") {
		t.Fatalf("expected %%EOF marker")
	}

	// Avoid unused import in case you uncomment the base64 test above.
	_ = base64.StdEncoding
	_ = errors.Is
}

// buildMinimalPDF returns a small, valid PDF (with xref) that ledongthuc/pdf can parse.
// If text == "", it emits a page with no shown text (BT/ET only), so extraction should be empty.
func buildMinimalPDF(text string) []byte {
	escape := func(s string) string {
		// Minimal escaping for PDF literal strings.
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

	// We generate a simple 5-object PDF:
	// 1: Catalog
	// 2: Pages
	// 3: Page
	// 4: Contents (stream)
	// 5: Font
	//
	// And then a correct xref section.
	var b []byte
	write := func(s string) { b = append(b, []byte(s)...) }

	offsets := make([]int, 6) // 0..5 (we use 1..5)
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
	writeObj(4, "4 0 obj\n<< /Length "+itoa(len([]byte(content)))+" >>\nstream\n"+content+"endstream\nendobj\n")
	writeObj(5, "5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xrefStart := len(b)
	write("xref\n0 6\n")
	write("0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		// Xref entries are 10-digit, zero-padded byte offsets.
		write(pad10(offsets[i]) + " 00000 n \n")
	}
	write("trailer\n<< /Size 6 /Root 1 0 R >>\n")
	write("startxref\n")
	write(itoa(xrefStart) + "\n")
	write("%%EOF\n")

	return b
}

func pad10(n int) string {
	s := itoa(n)
	if len(s) >= 10 {
		return s
	}
	return strings.Repeat("0", 10-len(s)) + s
}

func itoa(n int) string {
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

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
