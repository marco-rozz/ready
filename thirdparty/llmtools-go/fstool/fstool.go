package fstool

import (
	"context"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	hardFileReadBytes int64 = 32 * 1024 * 1024

	hardFileWriteBytes int64 = 32 * 1024 * 1024

	defaultListDirectoryMaxEntries = 1024
	hardListDirectoryMaxEntries    = 64000

	defaultSearchFilesMaxResults = 1024
	hardSearchFilesMaxResults    = 64000

	hardSearchContentBytes int64 = 1 * 1024 * 1024

	hardMIMESniffBytes int64 = 4096

	hardWriteFileParentCreations = 16
	hardTrashUniquePathAttempts  = 12
)

type fsToolConfig struct {
	allowedRoots  []string
	workBaseDir   string
	blockSymlinks bool
}

// FSTool is an instance-owned filesystem tool runner.
// It centralizes path resolution and sandbox policy:
//   - workBaseDir: base for resolving relative paths
//   - allowedRoots: optional restriction; if empty, allow all
//   - blockSymlinks: blocks symlink traversal (enforced downstream).
type FSTool struct {
	mu     sync.RWMutex
	cfg    fsToolConfig
	policy fspolicy.FSPolicy
}

type FSToolOption func(*FSTool) error

// WithAllowedRoots restricts all filesystem paths to be within one of the provided roots.
// Roots are canonicalized (clean+abs+best-effort symlink eval) and must exist as directories.
func WithAllowedRoots(roots []string) FSToolOption {
	return func(ft *FSTool) error {
		ft.cfg.allowedRoots = roots
		return nil
	}
}

// WithWorkBaseDir sets the base directory used to resolve relative input paths.
// If empty/whitespace, NewFSTool will pick an effective default (via InitPathPolicy).
func WithWorkBaseDir(base string) FSToolOption {
	return func(ft *FSTool) error {
		ft.cfg.workBaseDir = base
		return nil
	}
}

// WithBlockSymlinks configures whether symlink traversal should be blocked (if supported downstream).
func WithBlockSymlinks(block bool) FSToolOption {
	return func(ft *FSTool) error {
		ft.cfg.blockSymlinks = block
		return nil
	}
}

func NewFSTool(opts ...FSToolOption) (*FSTool, error) {
	ft := &FSTool{
		cfg: fsToolConfig{
			allowedRoots:  nil,
			workBaseDir:   "",
			blockSymlinks: false,
		},
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(ft); err != nil {
			return nil, err
		}
	}

	pol, err := fspolicy.New(ft.cfg.workBaseDir, ft.cfg.allowedRoots, ft.cfg.blockSymlinks)
	if err != nil {
		return nil, err
	}
	ft.policy = pol

	return ft, nil
}

func (ft *FSTool) DeleteFileTool() spec.Tool       { return toolutil.CloneTool(deleteFileTool) }
func (ft *FSTool) ListDirectoryTool() spec.Tool    { return toolutil.CloneTool(listDirectoryTool) }
func (ft *FSTool) MIMEForExtensionTool() spec.Tool { return toolutil.CloneTool(mimeForExtensionTool) }
func (ft *FSTool) MIMEForPathTool() spec.Tool      { return toolutil.CloneTool(mimeForPathTool) }
func (ft *FSTool) ReadFileTool() spec.Tool         { return toolutil.CloneTool(readFileTool) }
func (ft *FSTool) SearchFilesTool() spec.Tool      { return toolutil.CloneTool(searchFilesTool) }
func (ft *FSTool) StatPathTool() spec.Tool         { return toolutil.CloneTool(statPathTool) }
func (ft *FSTool) WriteFileTool() spec.Tool        { return toolutil.CloneTool(writeFileTool) }

func (ft *FSTool) DeleteFile(ctx context.Context, args DeleteFileArgs) (*DeleteFileOut, error) {
	return toolutil.WithRecoveryResp(func() (*DeleteFileOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return deleteFile(ctx, args, p)
	})
}

func (ft *FSTool) ListDirectory(ctx context.Context, args ListDirectoryArgs) (*ListDirectoryOut, error) {
	return toolutil.WithRecoveryResp(func() (*ListDirectoryOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return listDirectory(ctx, args, p)
	})
}

func (ft *FSTool) MIMEForExtension(ctx context.Context, args MIMEForExtensionArgs) (*MIMEForExtensionOut, error) {
	return toolutil.WithRecoveryResp(func() (*MIMEForExtensionOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return mimeForExtension(ctx, args, p)
	})
}

func (ft *FSTool) MIMEForPath(ctx context.Context, args MIMEForPathArgs) (*MIMEForPathOut, error) {
	return toolutil.WithRecoveryResp(func() (*MIMEForPathOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return mimeForPath(ctx, args, p)
	})
}

func (ft *FSTool) ReadFile(
	ctx context.Context,
	args ReadFileArgs,
) ([]spec.ToolOutputUnion, error) {
	return toolutil.WithRecoveryResp(func() ([]spec.ToolOutputUnion, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return readFile(ctx, args, p)
	})
}

func (ft *FSTool) SearchFiles(ctx context.Context, args SearchFilesArgs) (*SearchFilesOut, error) {
	return toolutil.WithRecoveryResp(func() (*SearchFilesOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return searchFiles(ctx, args, p)
	})
}

func (ft *FSTool) StatPath(ctx context.Context, args StatPathArgs) (*StatPathOut, error) {
	return toolutil.WithRecoveryResp(func() (*StatPathOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return statPath(ctx, args, p)
	})
}

func (ft *FSTool) WriteFile(ctx context.Context, args WriteFileArgs) (*WriteFileOut, error) {
	return toolutil.WithRecoveryResp(func() (*WriteFileOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := ft.snapshotPolicy()
		return writeFile(ctx, args, p)
	})
}

func (ft *FSTool) snapshotPolicy() fspolicy.FSPolicy {
	ft.mu.RLock()
	p := ft.policy
	ft.mu.RUnlock()
	return p
}
