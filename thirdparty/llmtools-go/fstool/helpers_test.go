package fstool

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	testNameContextCanceled                  = "context_canceled"
	testNameAllowedRootsBlocksOutsidePath    = "allowedRoots_blocks_outside_path"
	testNameAllowedRootsBlocksOutsideDir     = "allowedRoots_blocks_outside_dir"
	testNameWindowsDriveRelativePathRejected = "windows_drive_relative_path_rejected"
)

const (
	testWhitespace      = "   "
	testPathCurrentDir  = "."
	testPathATxt        = "a.txt"
	testPathXTxt        = "x.txt"
	testPathBMD         = "b.md"
	testPathSubdir      = "subdir"
	testPathFileTxt     = "file.txt"
	testPathFileBin     = "file.bin"
	testPathImagePNG    = "image.png"
	testPathLinkTxt     = "link.txt"
	testPathLinkDir     = "linkdir"
	testDirLink         = "link"
	testPathSampleTxt   = "sample.txt"
	testPathUnknownExt  = ".unknownext"
	testPathDriveRel    = `C:drive-relative.txt`
	testPathAutoTxt     = "auto.txt"
	testPathMissingTxt  = "missing.txt"
	testPathMissing     = "missing"
	testPathMissingRoot = "missing-root"
	testPathSameTxt     = "same.txt"
	testPathTargetTxt   = "target.txt"
	testPathReal        = "real"
	testPathTrash       = "trash"
	testPathExistsTxt   = "exists.txt"
	testPathOWTxt       = "ow.txt"
	testPathBadB64Dat   = "badb64.dat"
	testPathBadUtf8Txt  = "badutf8.txt"
	testPathBinDat      = "bin.dat"
	testPathTrimBin     = "trim.bin"
	testPathStayTxt     = "stay.txt"
	testPathOutTxt      = "out.txt"
	testPathNope        = "nope"
	testPathNopeTxt     = "nope.txt"
	testPathRootFile    = "rootfile"
	testPathNotADir     = "notadir"
	testPathChildTxt    = "child.txt"
	testPathCfooTxt     = "C:foo.txt"
	testPathFooTxt      = "foo.txt"
	testPathBarMD       = "bar.md"
	testPathBazTxt      = "baz.txt"
	testPathEscapeTxt   = "escape.txt"
	testPathOutsideTxt  = "outside.txt"
	testPathNoExt       = "noext"
)

const (
	testGlobMD        = "*.md"
	testGlobInvalid   = "["
	testRegexpFooTxt  = `foo\.txt`
	testRegexpLinkTxt = `link\.txt`
	testQueryTxt      = "txt"
	testQueryGoodbye  = "goodbye"
	testQueryBaz      = "baz"
	testQueryNeedle   = "needle"
	testExtPNG        = "png"
	testExtDotPNG     = ".png"
	testExtTXT        = ".txt"
	testExtUnknown    = testPathUnknownExt
	testExtPDFUpper   = ".PDF"
	testExtPDFLower   = ".pdf"
)

const (
	testContentHello             = "hello"
	testContentHelloWorld        = "hello world"
	testContentGoodbyeWorld      = "goodbye world"
	testContentBaz               = "baz content"
	testContentKeep              = "keep"
	testContentOne               = "one"
	testContentTwo               = "two"
	testContentA                 = "a"
	testContentB                 = "b"
	testContentX                 = "x"
	testContentOK                = "ok"
	testContentStay              = "stay"
	testContentHi                = "hi"
	testContentABC               = "abc"
	testContentNotADir           = "not a dir"
	testContentNeedleOutsideRoot = "needle outside root"
	testContentInvalidBase64     = "!!!"
)

const (
	testTrashDirAuto          = "auto"
	testEncodingBinary        = "binary"
	testEncodingNope          = "nope"
	testEncodingMixedCaseText = "TeXt"
	testEncodingText          = string(MIMEModeText)
	testKindText              = testEncodingText
	testKindFile              = string(ListDirectoryEntryKindFile)
	testKindImage             = "image"
)

const (
	testMIMEImagePNG               = "image/png"
	testMIMETextPlainCharset       = "text/plain; charset=utf-8"
	testMIMETextPlain              = "text/plain"
	testMIMEApplicationOctetStream = "application/octet-stream"
	testMIMEApplicationPrefix      = "application/"
	testMIMEImagePrefix            = "image/"
)

const (
	testErrInvalidPath                = "invalid path"
	testErrOutsideAllowedRoots        = "outside allowed roots"
	testErrSymlink                    = "symlink"
	testErrDoesNotExist               = "does not exist"
	testErrQueryRequired              = "query is required"
	testErrInvalidBase64              = "invalid base64"
	testErrCannotReadNonTextFile      = "cannot read non-text file"
	testErrNotValidUTF8               = "not valid UTF-8"
	testErrOverwriteFalse             = "overwrite=false"
	testErrEncodingMustBeTextOrBinary = `encoding must be "text" or "binary"`
	testErrDriveRelative              = "drive-relative"
)

func mustNewFSTool(t *testing.T, opts ...FSToolOption) *FSTool {
	t.Helper()
	ft, err := NewFSTool(opts...)
	if err != nil {
		t.Fatalf("NewFSTool: %v", err)
	}
	return ft
}

func canceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return b
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		// Often EPERM on Windows or in restricted CI.
		t.Skipf("symlink not supported/allowed: %v", err)
	}
}

func decodeBase64OrFail(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid base64: %v", err)
	}
	return b
}

// canonForPolicyExpectations mimics the policy's Darwin root alias normalization.
// This keeps expectations stable on macOS where temp dirs often start with /var (symlink)
// but policy returns /private/var paths.
func canonForPolicyExpectations(p string) string {
	p = filepath.Clean(p)
	p = evalTestSymlinksBestEffort(p)
	if runtime.GOOS != toolutil.GOOSDarwin {
		return p
	}
	aliases := map[string]string{
		"/var":  "/private/var",
		"/tmp":  "/private/tmp",
		"/etc":  "/private/etc",
		"/bin":  "/usr/bin",
		"/sbin": "/usr/sbin",
		"/lib":  "/usr/lib",
	}
	sep := string(os.PathSeparator)
	for from, to := range aliases {
		if p == from {
			return to
		}
		if strings.HasPrefix(p, from+sep) {
			return to + p[len(from):]
		}
	}
	return p
}

func evalTestSymlinksBestEffort(p string) string {
	p = filepath.Clean(p)
	tried := p
	remainder := ""

	for range 64 {
		if resolved, err := filepath.EvalSymlinks(tried); err == nil && resolved != "" {
			resolved = filepath.Clean(resolved)
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}

		parent := filepath.Dir(tried)
		if parent == tried {
			return p
		}

		base := filepath.Base(tried)
		if remainder == "" {
			remainder = base
		} else {
			remainder = filepath.Join(base, remainder)
		}
		tried = parent
	}
	return p
}

func equalStringMultisets(a, b []string) bool {
	ma := stringSliceAsSet(a)
	mb := stringSliceAsSet(b)
	if len(ma) != len(mb) {
		return false
	}
	for k, va := range ma {
		if vb, ok := mb[k]; !ok || vb != va {
			return false
		}
	}
	return true
}

func stringSliceAsSet(in []string) map[string]int {
	m := make(map[string]int, len(in))
	for _, s := range in {
		m[s]++
	}
	return m
}

func wantErrContains(substr string) func(error) bool {
	return func(err error) bool {
		return err != nil && strings.Contains(err.Error(), substr)
	}
}

func wantErrIs(target error) func(error) bool {
	return func(err error) bool {
		return errors.Is(err, target)
	}
}

func wantErrAny(err error) bool  { return err != nil }
func wantErrNone(err error) bool { return err == nil }

func ptrInt64(v int64) *int64 { return new(v) }
