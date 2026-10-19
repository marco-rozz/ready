package fstool

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestNewFSTool_PolicyInitializationAndDefaults(t *testing.T) {
	tmp := t.TempDir()

	tests := []struct {
		name    string
		opts    func(t *testing.T) []FSToolOption
		wantErr bool
	}{
		{
			name: "no_options_uses_process_cwd",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				return nil
			},
		},
		{
			name: "workBaseDir_explicit_ok",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				return []FSToolOption{
					WithWorkBaseDir(tmp),
				}
			},
		},
		{
			name: "allowedRoots_only_defaults_base_to_first_root",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				return []FSToolOption{
					WithAllowedRoots([]string{tmp}),
				}
			},
		},
		{
			name: "allowedRoots_nonexistent_errors",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				return []FSToolOption{
					WithAllowedRoots([]string{filepath.Join(tmp, "missing-root")}),
				}
			},
			wantErr: true,
		},
		{
			name: "allowedRoots_points_to_file_errors",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				f := filepath.Join(tmp, "rootfile")
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatalf("seed file: %v", err)
				}
				return []FSToolOption{
					WithAllowedRoots([]string{f}),
				}
			},
			wantErr: true,
		},
		{
			name: "blockSymlinks_true_rejects_symlink_root_on_unix",
			opts: func(t *testing.T) []FSToolOption {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink roots are not reliable to create on Windows CI")
				}
				realTxt := filepath.Join(tmp, "realroot")
				mustMkdirAll(t, realTxt)
				link := filepath.Join(tmp, "linkroot")
				mustSymlinkOrSkip(t, realTxt, link)

				// When symlinks are blocked, policy initialization verifies roots contain no symlink components.
				return []FSToolOption{
					WithAllowedRoots([]string{link}),
					WithWorkBaseDir(link),
					WithBlockSymlinks(true),
				}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewFSTool(tt.opts(t)...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}
