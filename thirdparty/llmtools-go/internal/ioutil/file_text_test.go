package ioutil

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

const (
	textFileTestSizeErrSubstr = "exceeds maximum allowed size"
)

func TestTextFile_Render(t *testing.T) {
	tests := []struct {
		name string
		tf   TextFile
		want string
	}{
		{
			name: "no lines no final newline => empty",
			tf:   TextFile{Newline: NewlineLF, HasFinalNewline: false, Lines: nil},
			want: "",
		},
		{
			name: "no lines with final newline => newline only",
			tf:   TextFile{Newline: NewlineLF, HasFinalNewline: true, Lines: nil},
			want: "\n",
		},
		{
			name: "lf with final newline",
			tf:   TextFile{Newline: NewlineLF, HasFinalNewline: true, Lines: []string{"a", "b"}},
			want: "a\nb\n",
		},
		{
			name: "crlf without final newline",
			tf:   TextFile{Newline: NewlineCRLF, HasFinalNewline: false, Lines: []string{"a", "b"}},
			want: "a\r\nb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tf.Render(); got != tc.want {
				t.Fatalf("Render()=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestReadTextFileUTF8_Behavior(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		mustWriteBytes(t, p, b)
		return p
	}

	pEmpty := write("empty.txt", []byte{})
	pOnlyNL := write("onlynl.txt", []byte("\n"))
	pLF := write("lf.txt", []byte("a\nb\n"))
	pCRLF := write("crlf.txt", []byte("a\r\nb\r\n"))
	pStrayCR := write("straycr.txt", []byte("a\rb\r"))
	pBadUTF8 := write("badutf8.txt", []byte{0xff, 0xfe, 0xfd})

	pTooBig := write("big.txt", []byte("0123456789")) // 10 bytes

	tests := []struct {
		name        string
		path        string
		maxBytes    int64
		wantErr     error
		errContains string

		wantNewline NewlineKind
		wantFinal   bool
		wantLines   []string
	}{
		{
			name:        "empty file => lines nil, no final newline, lf default",
			path:        pEmpty,
			maxBytes:    0,
			wantNewline: NewlineLF,
			wantFinal:   false,
			wantLines:   nil,
		},
		{
			name:        "file containing only newline => one empty line + final newline",
			path:        pOnlyNL,
			maxBytes:    0,
			wantNewline: NewlineLF,
			wantFinal:   true,
			wantLines:   []string{""},
		},
		{
			name:        "lf preserves lf and final newline",
			path:        pLF,
			maxBytes:    0,
			wantNewline: NewlineLF,
			wantFinal:   true,
			wantLines:   []string{"a", "b"},
		},
		{
			name:        "crlf preserves crlf and final newline",
			path:        pCRLF,
			maxBytes:    0,
			wantNewline: NewlineCRLF,
			wantFinal:   true,
			wantLines:   []string{"a", "b"},
		},
		{
			name:        "stray CR normalized as LF-kind and treated as final newline",
			path:        pStrayCR,
			maxBytes:    0,
			wantNewline: NewlineLF,
			wantFinal:   true,
			wantLines:   []string{"a", "b"},
		},
		{
			name:     "invalid utf8 errors",
			path:     pBadUTF8,
			maxBytes: 0,
			wantErr:  ErrNotUTF8Text,
		},
		{
			name:        "maxBytes enforced by stat precheck",
			path:        pTooBig,
			maxBytes:    5,
			wantErr:     ErrFileExceedsMaxSize,
			errContains: textFileTestSizeErrSubstr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			tf, err := ReadTextFileUTF8(policy, tc.path, tc.maxBytes)

			if tc.wantErr != nil || tc.errContains != "" {
				if err == nil {
					t.Fatalf("expected error, got nil (tf=%+v)", tf)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("error=%v want=%v", err, tc.wantErr)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tf.Newline != tc.wantNewline {
				t.Fatalf("Newline=%q want=%q", tf.Newline, tc.wantNewline)
			}
			if tf.HasFinalNewline != tc.wantFinal {
				t.Fatalf("HasFinalNewline=%v want=%v", tf.HasFinalNewline, tc.wantFinal)
			}
			if !slices.Equal(tf.Lines, tc.wantLines) {
				t.Fatalf("Lines=%#v want=%#v", tf.Lines, tc.wantLines)
			}
			if tf.ModTimeUTC == nil {
				t.Fatalf("ModTimeUTC is nil")
			}
			if tf.ModTimeUTC.Location() != time.UTC {
				t.Fatalf("ModTimeUTC not in UTC: %v", tf.ModTimeUTC.Location())
			}
		})
	}
}
