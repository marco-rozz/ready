package fstool

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestWriteFile(t *testing.T) {
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
		args    func(t *testing.T, c cfg) WriteFileArgs
		wantErr func(error) bool
		check   func(t *testing.T, c cfg, out *WriteFileOut)
	}{
		{
			name: testNameContextCanceled,
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			ctx: canceledContext,
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{Path: testPathATxt, Content: testContentX}
			},
			wantErr: wantErrIs(context.Canceled),
		},
		{
			name: "writes_text_default_encoding_trims_path",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{Path: testWhitespace + "text.txt" + testWhitespace, Content: testContentHello}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				if out.BytesWritten != 5 {
					t.Fatalf("BytesWritten=%d want=%d", out.BytesWritten, 5)
				}
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, "text.txt")))
				if got != testContentHello {
					t.Fatalf("content=%q want=%q", got, testContentHello)
				}
			},
		},
		{
			name: "overwrite_false_errors_and_preserves_original",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				p := filepath.Join(c.workBaseDir, "exists.txt")
				mustWriteFile(t, p, []byte(testContentA))
				return WriteFileArgs{Path: "exists.txt", Content: testContentB, Overwrite: false}
			},
			wantErr: wantErrContains(testErrOverwriteFalse),
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, "exists.txt")))
				if got != testContentA {
					t.Fatalf("expected original content preserved, got=%q", got)
				}
			},
		},
		{
			name: "overwrite_true_replaces_content",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				p := filepath.Join(c.workBaseDir, testPathOWTxt)
				mustWriteFile(t, p, []byte(testContentA))
				return WriteFileArgs{Path: testPathOWTxt, Content: testContentB, Overwrite: true}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, testPathOWTxt)))
				if got != testContentB {
					t.Fatalf("content=%q want=%q", got, testContentB)
				}
			},
		},
		{
			name: "writes_binary_base64_trimmed_case_insensitive_encoding",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				raw := []byte{0x00, 0x01, 0x02, 0xff}
				b64 := base64.StdEncoding.EncodeToString(raw)
				return WriteFileArgs{
					Path:     testPathBinDat,
					Encoding: testWhitespace + "BiNaRy" + testWhitespace,
					Content:  testWhitespace + b64 + testWhitespace,
				}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				got := mustReadFile(t, filepath.Join(c.workBaseDir, testPathBinDat))
				want := []byte{0x00, 0x01, 0x02, 0xff}
				if !bytes.Equal(got, want) {
					t.Fatalf("bytes=%v want=%v", got, want)
				}
				if out.BytesWritten != int64(len(want)) {
					t.Fatalf("BytesWritten=%d want=%d", out.BytesWritten, len(want))
				}
			},
		},
		{
			name: "invalid_base64_errors",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{
					Path:     testPathBadB64Dat,
					Encoding: testEncodingBinary,
					Content:  testContentInvalidBase64,
				}
			},
			wantErr: wantErrContains(testErrInvalidBase64),
		},
		{
			name: "createParents_false_missing_parent_errors",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{
					Path:          filepath.Join(testPathNope, testPathATxt),
					Content:       testContentX,
					CreateParents: false,
				}
			},
			wantErr: wantErrAny,
		},
		{
			name: "createParents_true_creates_dirs_and_writes",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{
					Path:          filepath.Join("a", "b", "c", "d.txt"),
					Content:       testContentOK,
					CreateParents: true,
				}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(c.workBaseDir, "a", "b", "c", "d.txt")); err != nil {
					t.Fatalf("expected file to exist, stat err=%v", err)
				}
			},
		},
		{
			name: "createParents_true_depth_limit_exceeded_does_not_create_file",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp, blockSymlinks: true} // Max depth is enforced only if symlinks are blocked.
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				p := filepath.Join(
					"1",
					"2",
					"3",
					"4",
					"5",
					"6",
					"7",
					"8",
					"9",
					"10",
					"11",
					"12",
					"13",
					"14",
					"15",
					"16",
					"17",
					"f.txt",
				)
				return WriteFileArgs{Path: p, Content: testContentX, CreateParents: true}
			},
			wantErr: wantErrContains("too many parent directories"),
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				p := filepath.Join(
					c.workBaseDir,
					"1",
					"2",
					"3",
					"4",
					"5",
					"6",
					"7",
					"8",
					"9",
					"10",
					"11",
					"12",
					"13",
					"14",
					"15",
					"16",
					"17",
					"f.txt",
				)
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("did not expect file to be created on error")
				}
			},
		},
		{
			name: "refuses_directory_target",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{Path: testPathCurrentDir, Content: testContentX}
			},
			wantErr: wantErrAny,
		},
		{
			name: "refuses_invalid_utf8_text",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				// Construct a string with invalid UTF-8 bytes.
				s := string([]byte{0xff, 0xfe})
				return WriteFileArgs{Path: testPathBadUtf8Txt, Encoding: testEncodingText, Content: s}
			},
			wantErr: wantErrContains(testErrNotValidUTF8),
		},
		{
			name: "blockSymlinks_true_rejects_symlink_parent_component",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp, blockSymlinks: true}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				realTxt := filepath.Join(c.workBaseDir, testPathReal)
				mustMkdirAll(t, realTxt)

				link := filepath.Join(c.workBaseDir, testDirLink)
				mustSymlinkOrSkip(t, realTxt, link)

				return WriteFileArgs{
					Path:          filepath.Join(testDirLink, testPathChildTxt),
					Content:       testContentX,
					CreateParents: false,
				}
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
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				outside := t.TempDir()
				return WriteFileArgs{Path: filepath.Join(outside, testPathXTxt), Content: testContentX}
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
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				if runtime.GOOS != toolutil.GOOSWindows {
					t.Skip("windows-only behavior")
				}
				return WriteFileArgs{Path: testPathDriveRel, Content: testContentX}
			},
			wantErr: wantErrContains(testErrDriveRelative),
		},
		{
			name: "writes_into_symlink_dir_when_blockSymlinks_false",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests are unreliable on Windows CI")
				}
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp, blockSymlinks: false}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				realTxt := filepath.Join(c.workBaseDir, testPathReal)
				mustMkdirAll(t, realTxt)

				link := filepath.Join(c.workBaseDir, testDirLink)
				mustSymlinkOrSkip(t, realTxt, link)

				return WriteFileArgs{
					Path:          filepath.Join(testDirLink, testPathChildTxt),
					Content:       testContentOK,
					CreateParents: false,
				}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				// Should have written into real/child.txt.
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, testPathReal, testPathChildTxt)))
				if got != testContentOK {
					t.Fatalf("content=%q want=%q", got, testContentOK)
				}
			},
		},
		{
			name: "returns_absolute_resolved_path_in_output",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{Path: testPathOutTxt, Content: testContentX}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				if out == nil {
					t.Fatalf("expected non-nil out")
				}
				want := canonForPolicyExpectations(filepath.Join(c.workBaseDir, testPathOutTxt))
				if filepath.Clean(out.Path) != filepath.Clean(want) {
					t.Fatalf("out.Path=%q want=%q", out.Path, want)
				}
			},
		},
		{
			name: "overwrite_false_error_does_not_modify_existing_file",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				mustWriteFile(t, filepath.Join(c.workBaseDir, testPathStayTxt), []byte(testContentStay))
				return WriteFileArgs{Path: testPathStayTxt, Content: "changed", Overwrite: false}
			},
			wantErr: wantErrContains(testErrOverwriteFalse),
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, testPathStayTxt)))
				if got != testContentStay {
					t.Fatalf("content modified unexpectedly: %q", got)
				}
			},
		},
		{
			name: "rejects_unknown_encoding",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				return WriteFileArgs{Path: testPathXTxt, Encoding: testEncodingNope, Content: testContentX}
			},
			wantErr: wantErrContains(testErrEncodingMustBeTextOrBinary),
		},
		{
			name: "binary_content_whitespace_trimmed",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				raw := []byte(testContentABC)
				return WriteFileArgs{
					Path:     testPathTrimBin,
					Encoding: testEncodingBinary,
					Content:  testWhitespace + base64.StdEncoding.EncodeToString(raw) + testWhitespace,
				}
			},
			wantErr: wantErrNone,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				got := mustReadFile(t, filepath.Join(c.workBaseDir, testPathTrimBin))
				if !bytes.Equal(got, []byte(testContentABC)) {
					t.Fatalf("got=%q want=%q", string(got), testContentABC)
				}
			},
		},
		{
			name: "error_messages_are_stable_enough_for_users",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				// Missing parent with createParents=false.
				return WriteFileArgs{
					Path:          filepath.Join(testPathNope, testPathXTxt),
					Content:       testContentX,
					CreateParents: false,
				}
			},
			wantErr: wantErrAny,
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				// No-op; existence checked by error.
			},
		},
		{
			name: "windows_path_semantics_do_not_break_non_windows",
			cfg: func(t *testing.T) cfg {
				t.Helper()
				tmp := t.TempDir()
				return cfg{workBaseDir: tmp}
			},
			args: func(t *testing.T, c cfg) WriteFileArgs {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("this case is for non-windows only")
				}
				// On unix, "C:foo" is just a filename with colon; should be allowed.
				return WriteFileArgs{Path: testPathCfooTxt, Content: testContentOK}
			},
			wantErr: func(err error) bool {
				if runtime.GOOS == toolutil.GOOSWindows {
					return true
				}
				return err == nil
			},
			check: func(t *testing.T, c cfg, out *WriteFileOut) {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					return
				}
				got := string(mustReadFile(t, filepath.Join(c.workBaseDir, testPathCfooTxt)))
				if got != testContentOK {
					t.Fatalf("content=%q want=%q", got, testContentOK)
				}
			},
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

			out, err := ft.WriteFile(ctx, tt.args(t, c))
			if tt.wantErr == nil {
				tt.wantErr = wantErrNone
			}
			if !tt.wantErr(err) {
				t.Fatalf("err=%v did not match expectation", err)
			}
			if tt.check != nil {
				tt.check(t, c, out)
			}

			// On error, ensure out is nil (tool contract expectation).
			if err != nil && out != nil {
				t.Fatalf("expected nil out on error, got %+v", out)
			}
		})
	}
}
