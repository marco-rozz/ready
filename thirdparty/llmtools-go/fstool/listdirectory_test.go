package fstool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestListDirectory(t *testing.T) {
	type policyCfg struct {
		workBaseDir   string
		allowedRoots  []string
		blockSymlinks bool
	}

	makeTool := func(t *testing.T, cfg policyCfg) *FSTool {
		t.Helper()
		opts := []FSToolOption{
			WithWorkBaseDir(cfg.workBaseDir),
			WithBlockSymlinks(cfg.blockSymlinks),
		}
		if cfg.allowedRoots != nil {
			opts = append(opts, WithAllowedRoots(cfg.allowedRoots))
		}
		return mustNewFSTool(t, opts...)
	}

	tests := []struct {
		name    string
		cfg     func(t *testing.T) policyCfg
		ctx     func(t *testing.T) context.Context
		args    func(t *testing.T, cfg policyCfg) ListDirectoryArgs
		want    []string
		wantErr func(error) bool
	}{
		{
			name: testNameContextCanceled,
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				return policyCfg{workBaseDir: tmp}
			},
			ctx: canceledContext,
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: cfg.workBaseDir}
			},
			wantErr: wantErrIs(context.Canceled),
		},
		{
			name: "lists_all_entries_sorted",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathATxt), []byte(testContentA))
				mustWriteFile(t, filepath.Join(tmp, testPathBMD), []byte(testContentB))
				mustMkdirAll(t, filepath.Join(tmp, testPathSubdir))
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: cfg.workBaseDir}
			},
			want:    []string{testPathATxt, testPathBMD, testPathSubdir},
			wantErr: wantErrNone,
		},
		{
			name: "pattern_filters",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathATxt), []byte(testContentA))
				mustWriteFile(t, filepath.Join(tmp, testPathBMD), []byte(testContentB))
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: cfg.workBaseDir, NameGlob: testGlobMD}
			},
			want:    []string{testPathBMD},
			wantErr: wantErrNone,
		},
		{
			name: "invalid_glob_pattern_errors",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathATxt), []byte(testContentA))
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: cfg.workBaseDir, NameGlob: testGlobInvalid}
			},
			wantErr: wantErrAny,
		},
		{
			name: "default_path_is_base_dir",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathATxt), []byte(testContentA))
				mustWriteFile(t, filepath.Join(tmp, testPathBMD), []byte(testContentB))
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{} // default "."
			},
			want:    []string{testPathATxt, testPathBMD},
			wantErr: wantErrNone,
		},
		{
			name: "path_is_file_errors",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				p := filepath.Join(tmp, testPathATxt)
				mustWriteFile(t, p, []byte(testContentA))
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: filepath.Join(cfg.workBaseDir, testPathATxt)}
			},
			wantErr: wantErrAny,
		},
		{
			name: "blockSymlinks_true_rejects_symlink_component",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				realTxt := filepath.Join(tmp, testPathReal)
				mustMkdirAll(t, realTxt)
				mustWriteFile(t, filepath.Join(realTxt, testPathATxt), []byte(testContentA))

				link := filepath.Join(tmp, testPathLinkDir)
				mustSymlinkOrSkip(t, realTxt, link)

				return policyCfg{workBaseDir: tmp, blockSymlinks: true}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: filepath.Join(cfg.workBaseDir, testPathLinkDir)}
			},
			wantErr: wantErrContains(testErrSymlink),
		},
		{
			name: "blockSymlinks_false_allows_symlink_component",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				realTxt := filepath.Join(tmp, testPathReal)
				mustMkdirAll(t, realTxt)
				mustWriteFile(t, filepath.Join(realTxt, testPathATxt), []byte(testContentA))

				link := filepath.Join(tmp, testPathLinkDir)
				mustSymlinkOrSkip(t, realTxt, link)

				return policyCfg{workBaseDir: tmp, blockSymlinks: false}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: filepath.Join(cfg.workBaseDir, testPathLinkDir)}
			},
			want:    []string{testPathATxt},
			wantErr: wantErrNone,
		},
		{
			name: "allowedRoots_blocks_outside_dir",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				root := t.TempDir()
				outside := t.TempDir()
				_ = outside
				return policyCfg{workBaseDir: root, allowedRoots: []string{root}}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				outside := t.TempDir()
				return ListDirectoryArgs{Path: outside}
			},
			wantErr: wantErrContains(testErrOutsideAllowedRoots),
		},
		{
			name: "nonexistent_dir_errors_isnotexist",
			cfg: func(t *testing.T) policyCfg {
				t.Helper()
				tmp := t.TempDir()
				return policyCfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, cfg policyCfg) ListDirectoryArgs {
				t.Helper()
				return ListDirectoryArgs{Path: filepath.Join(cfg.workBaseDir, testPathMissing)}
			},
			wantErr: func(err error) bool {
				return err != nil && errors.Is(err, os.ErrNotExist)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg(t)
			ft := makeTool(t, cfg)

			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(t)
			}

			out, err := ft.ListDirectory(ctx, tt.args(t, cfg))
			if tt.wantErr == nil {
				tt.wantErr = wantErrNone
			}
			if !tt.wantErr(err) {
				t.Fatalf("err=%v did not match expectation", err)
			}
			if err != nil {
				return
			}

			if out == nil {
				t.Fatalf("expected non-nil out")
			}

			if tt.want != nil {
				entries := make([]string, 0, len(out.Items))
				for _, item := range out.Items {
					entries = append(entries, item.Name)
				}
				if !equalStringMultisets(entries, tt.want) {
					t.Fatalf("Entries=%v want=%v", entries, tt.want)
				}
			}
		})
	}
}
