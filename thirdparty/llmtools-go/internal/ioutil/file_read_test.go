package ioutil

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fileReadTestBlankSpace          = "   "
	fileReadTestHello               = "hello"
	fileReadTestEncodingErrMsg      = `encoding must be "text" or "binary"`
	fileReadTestEmptyPathName       = "empty path"
	fileReadTestNonExistentPathName = "non-existent path"
	fileReadTestSizeErrSubstr       = "exceeds maximum allowed size"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()

	textPath := filepath.Join(dir, "sample.txt")
	textContent := "Hello, world!\n"
	writeFile(t, textPath, textContent)

	nonExistentPath := filepath.Join(dir, "does_not_exist.txt")
	binaryContent := base64.StdEncoding.EncodeToString([]byte(textContent))

	tests := []struct {
		name            string
		path            string
		encoding        ReadEncoding
		want            string
		wantErr         bool
		wantErrContains string
		wantIsNotExist  bool
	}{
		{
			name:     "text encoding returns raw content",
			path:     textPath,
			encoding: ReadEncodingText,
			want:     textContent,
		},
		{
			name:     "binary encoding returns base64",
			path:     textPath,
			encoding: ReadEncodingBinary,
			want:     binaryContent,
		},
		{
			name:            "invalid encoding",
			path:            textPath,
			encoding:        ReadEncoding("invalid"),
			wantErr:         true,
			wantErrContains: fileReadTestEncodingErrMsg,
		},
		{
			name:            "empty encoding (zero value) is invalid",
			path:            textPath,
			encoding:        ReadEncoding(""),
			wantErr:         true,
			wantErrContains: fileReadTestEncodingErrMsg,
		},
		{
			name:            fileReadTestEmptyPathName,
			path:            "",
			encoding:        ReadEncodingText,
			wantErr:         true,
			wantErrContains: ErrInvalidPath.Error(),
		},
		{
			name:            "whitespace-only path",
			path:            fileReadTestBlankSpace,
			encoding:        ReadEncodingText,
			wantErr:         true,
			wantErrContains: ErrInvalidPath.Error(),
		},
		{
			name:            "nul byte path",
			path:            "a\x00b",
			encoding:        ReadEncodingText,
			wantErr:         true,
			wantErrContains: ErrInvalidPath.Error(),
		},
		{
			name:           fileReadTestNonExistentPathName,
			path:           nonExistentPath,
			encoding:       ReadEncodingText,
			wantErr:        true,
			wantIsNotExist: true,
		},
	}

	const hardFileReadBytes int64 = 32 * 1024 * 1024

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFile(tc.path, tc.encoding, hardFileReadBytes)

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
			if got != tc.want {
				t.Errorf("ReadFile(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestReadFile_MaxBytes(t *testing.T) {
	dir := t.TempDir()

	p := filepath.Join(dir, "data.bin")
	data := []byte(fileReadTestHello) // 5 bytes
	mustWriteBytes(t, p, data)

	tests := []struct {
		name        string
		maxBytes    int64
		encoding    ReadEncoding
		want        string
		wantErr     bool
		errContains string
	}{
		{
			name:     "maxBytes zero means unlimited",
			maxBytes: 0,
			encoding: ReadEncodingText,
			want:     fileReadTestHello,
		},
		{
			name:     "maxBytes exact size ok",
			maxBytes: 5,
			encoding: ReadEncodingText,
			want:     fileReadTestHello,
		},
		{
			name:        "maxBytes smaller than size errors",
			maxBytes:    4,
			encoding:    ReadEncodingText,
			wantErr:     true,
			errContains: fileReadTestSizeErrSubstr,
		},
		{
			name:     "binary encoding base64",
			maxBytes: 5,
			encoding: ReadEncodingBinary,
			want:     base64.StdEncoding.EncodeToString(data),
		},
		{
			name:     "negative maxBytes treated as unlimited",
			maxBytes: -1,
			encoding: ReadEncodingText,
			want:     fileReadTestHello,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFile(p, tc.encoding, tc.maxBytes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%q)", got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}
