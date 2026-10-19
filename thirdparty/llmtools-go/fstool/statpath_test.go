package fstool

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestStatPath(t *testing.T) {
	type cfg struct {
		workBaseDir   string
		allowedRoots  []string
		blockSymlinks bool
	}

	makeTool := func(t *testing.T, c cfg) *FSTool {
		t.Helper()
		opts := []FSToolOption{WithWorkBaseDir(c.workBaseDir), WithBlockSymlinks(c.blockSymlinks)}
		if c.allowedRoots != nil {
			opts = append(opts, WithAllowedRoots(c.allowedRoots))
		}
		return mustNewFSTool(t, opts...)
	}

	tests := []struct {
		name    string
		cfg     func(t *testing.T) cfg
		ctx     func(t *testing.T) context.Context
		args    func(t *testing.T, c cfg) StatPathArgs
		wantErr func(error) bool

		wantExists  *bool
		wantIsDir   *bool
		wantName    string
		wantSize    *int64
		wantModTime bool
	}{
		{
			name: testNameContextCanceled,
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathSampleTxt), []byte(testContentHi))
				return cfg{workBaseDir: tmp}
			},
			ctx: canceledContext,
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathSampleTxt}
			},
			wantErr: wantErrIs(context.Canceled),
		},
		{
			name: "existing_file",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathSampleTxt), []byte(testContentHi))
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathSampleTxt}
			},
			wantErr:     wantErrNone,
			wantExists:  new(true),
			wantIsDir:   new(false),
			wantName:    testPathSampleTxt,
			wantSize:    ptrInt64(2),
			wantModTime: true,
		},
		{
			name: "existing_dir",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathCurrentDir}
			},
			wantErr:    wantErrNone,
			wantExists: new(true),
			wantIsDir:  new(true),
		},
		{
			name: "missing_path_exists_false_no_error",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathMissingTxt}
			},
			wantErr:    wantErrNone,
			wantExists: new(false),
			wantIsDir:  new(false),
		},
		{
			name: "empty_path_errors",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testWhitespace}
			},
			wantErr: wantErrContains(testErrInvalidPath),
		},
		{
			name: "symlink_path_followed_when_blockSymlinks_false",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathTargetTxt), []byte(testContentHi))
				mustSymlinkOrSkip(t, filepath.Join(tmp, testPathTargetTxt), filepath.Join(tmp, testPathLinkTxt))
				return cfg{workBaseDir: tmp, blockSymlinks: false}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathLinkTxt}
			},
			wantErr:    wantErrNone,
			wantExists: new(true),
			wantIsDir:  new(false),
		},
		{
			name: "symlink_path_refused_when_blockSymlinks_true",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				mustWriteFile(t, filepath.Join(tmp, testPathTargetTxt), []byte(testContentHi))
				mustSymlinkOrSkip(t, filepath.Join(tmp, testPathTargetTxt), filepath.Join(tmp, testPathLinkTxt))
				return cfg{workBaseDir: tmp, blockSymlinks: true}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				return StatPathArgs{Path: testPathLinkTxt}
			},
			wantErr: wantErrContains(testErrSymlink),
		},
		{
			name: testNameAllowedRootsBlocksOutsidePath,
			cfg: func(t *testing.T) cfg {
				t.Helper()
				root := t.TempDir()
				return cfg{workBaseDir: root, allowedRoots: []string{root}}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				outside := t.TempDir()
				return StatPathArgs{Path: outside}
			},
			wantErr: wantErrContains(testErrOutsideAllowedRoots),
		},
		{
			name: testNameWindowsDriveRelativePathRejected,
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) StatPathArgs {
				t.Helper()
				if runtime.GOOS != toolutil.GOOSWindows {
					t.Skip("windows-only behavior")
				}
				return StatPathArgs{Path: testPathDriveRel}
			},
			wantErr: wantErrContains(testErrDriveRelative),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.cfg(t)
			ft := makeTool(t, c)

			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(t)
			}

			out, err := ft.StatPath(ctx, tt.args(t, c))
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

			if tt.wantExists != nil && out.Exists != *tt.wantExists {
				t.Fatalf("Exists=%v want=%v", out.Exists, *tt.wantExists)
			}
			if tt.wantIsDir != nil && out.IsDir != *tt.wantIsDir {
				t.Fatalf("IsDir=%v want=%v", out.IsDir, *tt.wantIsDir)
			}
			if tt.wantName != "" && out.Name != tt.wantName {
				t.Fatalf("Name=%q want=%q", out.Name, tt.wantName)
			}
			if tt.wantSize != nil && out.SizeBytes != *tt.wantSize {
				t.Fatalf("SizeBytes=%d want=%d", out.SizeBytes, *tt.wantSize)
			}
			if tt.wantModTime && out.ModTime == nil {
				t.Fatalf("expected ModTime set")
			}
		})
	}
}
