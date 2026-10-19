package ioutil

import (
	"errors"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	extTestTxt                 = "txt"
	extTestBlankSpace          = "   "
	extTestInvalidPathName     = "invalid path"
	extTestEmptyPathName       = "empty path"
	extTestNonExistentPathName = "non-existent path"

	hardMIMESniffBytes int64 = 4096
)

func TestGetNormalizedExt(t *testing.T) {
	tests := []struct {
		in   string
		want FileExt
	}{
		{extTestTxt, ExtTxt},
		{".TXT", ExtTxt},
		{"  .Md  ", ExtMd},
		{"", FileExt("")},
		{extTestBlankSpace, FileExt("")},
	}

	for _, tc := range tests {
		t.Run("in="+tc.in, func(t *testing.T) {
			if got := GetNormalizedExt(tc.in); got != tc.want {
				t.Fatalf("GetNormalizedExt(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetBaseMIME(t *testing.T) {
	tests := []struct {
		in   MIMEType
		want MIMEType
	}{
		{MIMEEmpty, ""},
		{" Text/Plain; Charset=UTF-8 ", "text/plain"},
		{MIMEApplicationJSON, MIMEApplicationJSON},
		{MIMEImagePNG, MIMEImagePNG},
	}

	for _, tc := range tests {
		t.Run(string(tc.in), func(t *testing.T) {
			if got := GetBaseMIME(tc.in); got != string(tc.want) {
				t.Fatalf("GetBaseMIME(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGetModeForMIME(t *testing.T) {
	tests := []struct {
		in   MIMEType
		want ExtensionMode
	}{
		{MIMEEmpty, ExtensionModeDefault},
		{MIMEApplicationOctetStream, ExtensionModeDefault},
		{MIMETextPlain, ExtensionModeText},
		{"text/x-python", ExtensionModeText},            // text/* heuristic
		{"image/x-icon", ExtensionModeImage},            // image/* heuristic
		{"application/vnd.foo+json", ExtensionModeText}, // +json heuristic
		{"application/vnd.foo+xml", ExtensionModeText},  // +xml heuristic
		{"application/x-unknown", ExtensionModeDefault}, // default fallback
	}

	for _, tc := range tests {
		t.Run(string(tc.in), func(t *testing.T) {
			if got := GetModeForMIME(tc.in); got != tc.want {
				t.Fatalf("GetModeForMIME(%q)=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMIMEFromExtensionString_InternalAndStdlibFallback(t *testing.T) {
	// Force a deterministic stdlib fallback mapping.
	ext := ".llmtestmime"
	mt := "application/x-llmtestmime"
	if err := mime.AddExtensionType(ext, mt); err != nil {
		t.Fatalf("mime.AddExtensionType: %v", err)
	}

	tests := []struct {
		name      string
		ext       string
		want      MIMEType
		wantErrIs error
	}{
		{
			name:      "empty ext invalid",
			ext:       "",
			want:      MIMEEmpty,
			wantErrIs: ErrInvalidPath,
		},
		{
			name:      "blank ext invalid",
			ext:       extTestBlankSpace,
			want:      MIMEEmpty,
			wantErrIs: ErrInvalidPath,
		},
		{
			name: "known internal mapping (no dot, mixed case)",
			ext:  "PnG",
			want: MIMEImagePNG,
		},
		{
			name: "stdlib fallback mapping used",
			ext:  "llmtestmime",
			want: MIMEType(mt),
		},
		{
			name:      "unknown extension returns octet-stream + ErrUnknownExtension",
			ext:       ".definitelynotreal",
			want:      MIMEApplicationOctetStream,
			wantErrIs: ErrUnknownExtension,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MIMEFromExtensionString(tc.ext)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%q)", got)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MIMEFromExtensionString(%q)=%q want=%q", tc.ext, got, tc.want)
			}
		})
	}
}

func TestMIMEForLocalFile_ExtensionVsSniff(t *testing.T) {
	dir := t.TempDir()

	mdPath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(mdPath, []byte("# hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	noExtPNG := filepath.Join(dir, "imagefile")
	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if err := os.WriteFile(noExtPNG, pngHeader, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	unknownExtText := filepath.Join(dir, "x.unknownext")
	if err := os.WriteFile(unknownExtText, []byte("just some text\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fakeTxtIsPng := filepath.Join(dir, "fake"+"."+extTestTxt)
	if err := os.WriteFile(fakeTxtIsPng, pngHeader, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name         string
		path         string
		wantMethod   MIMEDetectMethod
		wantMode     ExtensionMode
		wantMIME     MIMEType
		wantErr      bool
		wantNotExist bool
	}{
		{
			name:    extTestInvalidPathName,
			path:    extTestBlankSpace,
			wantErr: true,
		},
		{
			name:       "extension mapping used (md)",
			path:       mdPath,
			wantMethod: MIMEDetectMethodExtension,
			wantMode:   ExtensionModeText,
			wantMIME:   MIMETextMarkdown,
		},
		{
			name:       "sniff used when no extension",
			path:       noExtPNG,
			wantMethod: MIMEDetectMethodSniff,
			wantMode:   ExtensionModeImage,
			wantMIME:   MIMEImagePNG,
		},
		{
			name:       "sniff used when extension unknown",
			path:       unknownExtText,
			wantMethod: MIMEDetectMethodSniff,
			wantMode:   ExtensionModeText,
			wantMIME:   MIMETextPlain,
		},
		{
			name:       "extension wins even if content is png (by design)",
			path:       fakeTxtIsPng,
			wantMethod: MIMEDetectMethodExtension,
			wantMode:   ExtensionModeText,
			wantMIME:   MIMETextPlain,
		},
		{
			name:       "missing file with known extension still returns extension-based result (no IO)",
			path:       filepath.Join(dir, "does-not-exist.md"),
			wantMethod: MIMEDetectMethodExtension,
			wantMode:   ExtensionModeText,
			wantMIME:   MIMETextMarkdown,
		},
		{
			name:         "missing file with unknown extension tries sniff and returns not-exist",
			path:         filepath.Join(dir, "does-not-exist.unknownext"),
			wantErr:      true,
			wantNotExist: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mt, mode, method, err := MIMEForLocalFile(tc.path, hardMIMESniffBytes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (mt=%q mode=%q method=%q)", mt, mode, method)
				}
				if tc.wantNotExist && !os.IsNotExist(err) {
					t.Fatalf("expected not-exist error, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if method != tc.wantMethod {
				t.Fatalf("method=%q want=%q", method, tc.wantMethod)
			}
			if mode != tc.wantMode {
				t.Fatalf("mode=%q want=%q", mode, tc.wantMode)
			}
			if mt != tc.wantMIME {
				t.Fatalf("mime=%q want=%q", mt, tc.wantMIME)
			}
		})
	}
}

func TestSniffFileMIME(t *testing.T) {
	dir := t.TempDir()

	emptyPath := filepath.Join(dir, "empty"+"."+extTestTxt)
	mustWriteBytes(t, emptyPath, []byte{})

	textPath := filepath.Join(dir, "text"+"."+extTestTxt)
	writeFile(t, textPath, "Hello, world!\n")

	utf8Path := filepath.Join(dir, "utf8"+"."+extTestTxt)
	writeFile(t, utf8Path, "Привет, мир!\n") // UTF-8 text

	binaryPath := filepath.Join(dir, "binary.png")
	// Minimal PNG header; DetectContentType should recognize this as image/png.
	pngHeader := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	mustWriteBytes(t, binaryPath, pngHeader)

	nonExistentPath := filepath.Join(dir, "no_such_file")

	tests := []struct {
		name            string
		path            string
		wantMIME        MIMEType
		wantIsText      bool
		wantErr         bool
		wantErrContains string
		wantIsNotExist  bool
	}{
		{
			name:            extTestEmptyPathName,
			path:            "",
			wantErr:         true,
			wantErrContains: "invalid path",
		},
		{
			name:           extTestNonExistentPathName,
			path:           nonExistentPath,
			wantErr:        true,
			wantIsNotExist: true,
		},
		{
			name:       "empty file treated as text/plain",
			path:       emptyPath,
			wantMIME:   MIMETextPlain,
			wantIsText: true,
		},
		{
			name:       "ASCII text file",
			path:       textPath,
			wantMIME:   MIMETextPlain,
			wantIsText: true,
		},
		{
			name:       "UTF-8 text file",
			path:       utf8Path,
			wantMIME:   MIMETextPlain,
			wantIsText: true,
		},
		{
			name:       "binary PNG file",
			path:       binaryPath,
			wantMIME:   MIMEImagePNG,
			wantIsText: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, mode, err := SniffFileMIME(tc.path, hardMIMESniffBytes)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
				if tc.wantIsNotExist && !os.IsNotExist(err) {
					t.Fatalf("expected a not-exist error, got: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantMIME != "" && m != tc.wantMIME {
				t.Errorf("MIME = %q, want %q", m, tc.wantMIME)
			}
			isText := mode == ExtensionModeText
			if isText != tc.wantIsText {
				t.Errorf("isText = %v, want %v", isText, tc.wantIsText)
			}
		})
	}
}

func TestIsProbablyTextSample(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{
			name: "empty slice is text",
			data: nil,
			want: true,
		},
		{
			name: "simple ASCII text",
			data: []byte("Hello, world!"),
			want: true,
		},
		{
			name: "text with allowed control characters",
			data: []byte("line1\nline2\tend\r"),
			want: true,
		},
		{
			name: "contains NUL byte",
			data: []byte{'a', 0x00, 'b'},
			want: false,
		},
		{
			name: "too many control characters",
			data: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, // many control chars
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isProbablyTextSample(tc.data)
			if got != tc.want {
				t.Errorf("isProbablyTextSample(%v) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}
