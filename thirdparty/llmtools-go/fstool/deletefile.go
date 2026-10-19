package fstool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const deleteFileFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/deletefile.DeleteFile"

var deleteFileTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04ca-12ac-7176-8037-d5f0f766a735",
	Slug:          "deletefile",
	Version:       spec.VersionOne,
	DisplayName:   "Delete file",
	Description:   "Safely delete a file by moving it to a trash directory. trashDir=auto will try to use the system trash when possible; otherwise falls back to a local .trash directory.",
	Tags:          []string{"fs"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path of the file to delete."
	},
	"trashDir": {
		"type": "string",
		"default": "auto",
		"description": "Trash destination. Use \"auto\" to attempt system trash detection; If auto detection fails, a default .trash directory is used. Non-existent directory will be created."
	}
},
"required": ["path"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: deleteFileFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type DeleteFileArgs struct {
	Path     string `json:"path"`
	TrashDir string `json:"trashDir,omitempty"` // "auto" default
}

type DeleteFileMethod string

const (
	DeleteFileMethodRename        DeleteFileMethod = "rename"
	DeleteFileMethodCopyAndRemove DeleteFileMethod = "copyAndRemove"
	DeleteFileMethodSymlinkRehome DeleteFileMethod = "symlinkRehome"
)

type DeleteFileOut struct {
	OriginalPath string           `json:"originalPath"`
	TrashedPath  string           `json:"trashedPath"`
	Method       DeleteFileMethod `json:"method"`
}

type trashCandidate struct {
	dir                  string
	allowCrossDeviceCopy bool
}

func deleteFile(
	ctx context.Context,
	args DeleteFileArgs,
	p fspolicy.FSPolicy,
) (*DeleteFileOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src, err := p.ResolvePath(args.Path, "")
	if err != nil {
		return nil, err
	}

	if p.BlockSymlinks() {
		parent := filepath.Dir(src)
		if parent != "" && parent != "." {
			if err := p.VerifyDirResolved(parent); err != nil {
				return nil, err
			}
		}
	}

	st, err := os.Lstat(src)
	if err != nil {
		return nil, err // preserves os.IsNotExist
	}
	if st.IsDir() {
		return nil, fmt.Errorf("path is a directory, not a file: %s", src)
	}

	// Allow regular files and symlinks; refuse other special files.
	if !st.Mode().IsRegular() && (st.Mode()&os.ModeSymlink) == 0 {
		return nil, fmt.Errorf("refusing to delete non-regular file: %s", src)
	}
	if (st.Mode()&os.ModeSymlink) != 0 && p.BlockSymlinks() {
		return nil, fmt.Errorf("%w: refusing to delete symlink file: %s", fspolicy.ErrSymlinkDisallowed, src)
	}

	trashDirIn := strings.TrimSpace(args.TrashDir)
	if trashDirIn == "" {
		trashDirIn = "auto"
	}

	candidates := []trashCandidate{}
	if trashDirIn == "auto" {
		if sys, ok := detectSystemTrashDir(); ok {
			if td, rerr := p.ResolvePath(sys, ""); rerr == nil {
				// "auto" should prefer system trash *when possible*; treat EXDEV as "not possible"
				// so we can fall back to a same-filesystem .trash instead of doing a huge copy.
				candidates = append(candidates, trashCandidate{dir: td, allowCrossDeviceCopy: false})
			}
		}
		// Always provide a same-filesystem-ish fallback near the file.
		local := filepath.Join(filepath.Dir(src), ".trash")
		if td, rerr := p.ResolvePath(local, ""); rerr == nil {
			candidates = append(candidates, trashCandidate{dir: td, allowCrossDeviceCopy: true})
		}
	} else {
		td, err := p.ResolvePath(trashDirIn, "")
		if err != nil {
			return nil, err
		}
		// User explicitly asked for this dir; allow copy fallback on EXDEV.
		candidates = append(candidates, trashCandidate{dir: td, allowCrossDeviceCopy: true})
	}

	var lastErr error
	for _, c := range candidates {
		td := c.dir

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Create trash dir if needed (policy enforces symlink rules when enabled).
		if _, err := p.EnsureDirResolved(td, 0 /*unlimited*/); err != nil {
			lastErr = err
			continue
		}

		trashedPath, method, _, err := moveToTrash(ctx, src, td, st, c.allowCrossDeviceCopy)

		if err == nil {
			return &DeleteFileOut{
				OriginalPath: src,
				TrashedPath:  trashedPath,
				Method:       method,
			}, nil
		}

		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("failed to determine trash directory")
	}
	return nil, lastErr
}

func detectSystemTrashDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", false
	}

	switch runtime.GOOS {
	case toolutil.GOOSDarwin:
		return filepath.Join(home, ".Trash"), true
	case toolutil.GOOSLinux, toolutil.GOOSFreebsd, toolutil.GOOSOpenbsd, toolutil.GOOSNetbsd, toolutil.GOOSDragonfly:
		if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
			return filepath.Join(xdg, "Trash", "files"), true
		}
		return filepath.Join(home, ".local", "share", "Trash", "files"), true
	default:
		return "", false
	}
}

func moveToTrash(
	ctx context.Context,
	src, trashDir string,
	srcInfo os.FileInfo,
	allowCrossDeviceCopy bool,
) (trashedPath string, method DeleteFileMethod, bytesWritten int64, err error) {
	base := filepath.Base(src)
	if base == "" || base == string(os.PathSeparator) || base == "." {
		return "", "", 0, ioutil.ErrInvalidPath
	}

	for range hardTrashUniquePathAttempts {
		dest, err := ioutil.UniquePathInDir(trashDir, base, hardTrashUniquePathAttempts)
		if err != nil {
			return "", "", 0, err
		}

		// On Unix, reserve with a placeholder so rename can't race-overwrite an entry.
		// On Windows, a placeholder breaks rename (rename fails if dest exists), so skip it.
		reserved := false
		if runtime.GOOS != toolutil.GOOSWindows {
			f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					continue
				}
				return "", "", 0, err
			}
			_ = f.Close()
			reserved = true
		}

		// Try rename first.
		if err := os.Rename(src, dest); err == nil {
			return dest, DeleteFileMethodRename, 0, nil
		} else {
			// Windows: if we lost a race and dest now exists, retry with a new name.
			if runtime.GOOS == toolutil.GOOSWindows {
				if _, stErr := os.Lstat(dest); stErr == nil {
					continue
				}
			}

			// If not EXDEV, fail (but clean placeholder on Unix).
			if !isCrossDeviceRenameErr(err) {
				if reserved {
					_ = os.Remove(dest)
				}
				return "", "", 0, err
			}
			// EXDEV: only do copy fallback when allowed; otherwise let caller try next candidate.
			if !allowCrossDeviceCopy {
				if reserved {
					_ = os.Remove(dest)
				}
				return "", "", 0, err
			}

			// Symlink: recreate link in trash then remove original link.
			if (srcInfo.Mode() & os.ModeSymlink) != 0 {
				if reserved {
					_ = os.Remove(dest)
				}
				target, rerr := os.Readlink(src)
				if rerr != nil {
					return "", "", 0, rerr
				}
				if serr := os.Symlink(target, dest); serr != nil {
					// If it now exists, retry with a new dest.
					if errors.Is(serr, os.ErrExist) {
						continue
					}
					if _, stErr := os.Lstat(dest); stErr == nil {
						continue
					}
					return "", "", 0, serr
				}
				if rmErr := os.Remove(src); rmErr != nil {
					_ = os.Remove(dest)
					return "", "", 0, rmErr
				}
				return dest, DeleteFileMethodSymlinkRehome, 0, nil
			}

			// Regular file: copy then remove.
			var n int64
			var cerr error
			if reserved {
				// Copy into the already-reserved placeholder to avoid a remove+race+recreate window.
				n, cerr = ioutil.CopyFileToExistingCtx(ctx, src, dest)
			} else {
				n, cerr = ioutil.CopyFileCtx(ctx, src, dest, 0o600)
			}
			if cerr != nil {
				_ = os.Remove(dest)
				// If destination now exists (race), retry with a new name.
				if errors.Is(cerr, os.ErrExist) {
					continue
				}
				return "", "", 0, cerr
			}
			if rmErr := os.Remove(src); rmErr != nil {
				_ = os.Remove(dest)
				return "", "", 0, rmErr
			}
			return dest, DeleteFileMethodCopyAndRemove, n, nil
		}
	}
	return "", "", 0, fmt.Errorf("could not allocate a unique trash path for %q", base)
}

// isCrossDeviceRenameErr - In go 1.25+: syscall.EXDEV exists on Windows too, and os.LinkError unwraps to the errno.
func isCrossDeviceRenameErr(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
