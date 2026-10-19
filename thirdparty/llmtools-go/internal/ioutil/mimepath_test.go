package ioutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMIMEForPathCoverage(t *testing.T) {
	root := t.TempDir()

	mdPath := filepath.Join(root, "doc.md")
	mustWriteBytes(t, mdPath, []byte("# hello\n"))

	pngPath := filepath.Join(root, "image")
	mustWriteBytes(t, pngPath, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	unknownTextPath := filepath.Join(root, "notes.unknown")
	mustWriteBytes(t, unknownTextPath, []byte("plain text\n"))

	missingMDPath := filepath.Join(root, "missing.md")
	missingUnknownPath := filepath.Join(root, "missing.unknown")

	tests := []struct {
		name            string
		path            string
		wantAbs         string
		wantMIME        MIMEType
		wantMode        ExtensionMode
		wantMethod      MIMEDetectMethod
		wantErrIs       error
		wantErrContains string
		wantNotExist    bool
	}{
		{
			name:            "invalid path is rejected",
			path:            "   ",
			wantErrIs:       ErrInvalidPath,
			wantErrContains: ErrInvalidPath.Error(),
		},
		{
			name:       "known extension returns metadata without IO",
			path:       missingMDPath,
			wantAbs:    missingMDPath,
			wantMIME:   MIMETextMarkdown,
			wantMode:   ExtensionModeText,
			wantMethod: MIMEDetectMethodExtension,
		},
		{
			name:       "png without extension falls back to sniff",
			path:       pngPath,
			wantAbs:    pngPath,
			wantMIME:   MIMEImagePNG,
			wantMode:   ExtensionModeImage,
			wantMethod: MIMEDetectMethodSniff,
		},
		{
			name:       "unknown extension with text content falls back to sniff",
			path:       unknownTextPath,
			wantAbs:    unknownTextPath,
			wantMIME:   MIMETextPlain,
			wantMode:   ExtensionModeText,
			wantMethod: MIMEDetectMethodSniff,
		},
		{
			name:         "missing unknown extension tries sniff and returns not-exist",
			path:         missingUnknownPath,
			wantNotExist: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := mustTestPolicy(t)
			abs, mt, mode, method, err := MIMEForPath(policy, tc.path, hardMIMESniffBytes)

			wantAbs := tc.wantAbs
			if wantAbs != "" {
				resolvedWantAbs, resolveErr := policy.ResolvePath(wantAbs, "")
				if resolveErr != nil {
					t.Fatalf("failed to resolve expected path %q: %v", wantAbs, resolveErr)
				}
				wantAbs = resolvedWantAbs
			}
			if tc.wantErrIs != nil || tc.wantNotExist || tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error, got nil (abs=%q mt=%q mode=%q method=%q)", abs, mt, mode, method)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
				if tc.wantNotExist && (!os.IsNotExist(err) && !strings.Contains(err.Error(), "stat file error")) {
					t.Fatalf("expected not-exist error, got: %v", err)
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if abs != wantAbs {
				t.Fatalf("abs=%q want=%q", abs, wantAbs)
			}
			if mt != tc.wantMIME {
				t.Fatalf("mime=%q want=%q", mt, tc.wantMIME)
			}
			if mode != tc.wantMode {
				t.Fatalf("mode=%q want=%q", mode, tc.wantMode)
			}
			if method != tc.wantMethod {
				t.Fatalf("method=%q want=%q", method, tc.wantMethod)
			}
		})
	}
}
