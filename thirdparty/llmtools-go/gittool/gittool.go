package gittool

import (
	"context"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	toolTagGit   = "git"
	toolTagRead  = "read"
	toolTagWrite = "write"

	revisionHead = "HEAD"
)

const (
	defaultContextLines = 3
	defaultDiffMaxBytes = 1024 * 1024
	hardDiffMaxBytes    = 2 * 1024 * 1024
	hardBlobReadBytes   = 4 * 1024 * 1024

	defaultLogMaxCount = 10
	hardLogMaxCount    = 100

	defaultTreeMaxCount = 1000
	hardTreeMaxCount    = 10000

	hardStagePaths         = 500
	hardRevisionLength     = 256
	hardRefNameLength      = 256
	hardTagMessageBytes    = 128 * 1024
	hardCommitMessageBytes = 128 * 1024

	defaultFindMaxDepth   = 5
	hardFindMaxDepth      = 12
	defaultFindMaxRepos   = 100
	hardFindMaxRepos      = 1000
	defaultFindMaxVisited = 20000
	hardFindMaxVisited    = 100000

	hardLineDiffMatrixCells = 2000000

	defaultGrepMaxMatches = 100
	hardGrepMaxMatches    = 1000
	defaultGrepMaxFiles   = 10000
	hardGrepMaxFiles      = 50000

	defaultHistoryMaxCount = 50
	hardHistoryMaxCount    = 500
	defaultHistoryMaxWalk  = 2000
	hardHistoryMaxWalk     = 20000
)

type gitToolConfig struct {
	allowedRoots       []string
	workBaseDir        string
	blockSymlinks      bool
	defaultAuthorName  string
	defaultAuthorEmail string
}

// GitTool is an instance-owned local Git tool runner.
//
// It intentionally does not shell out to git. Implementations use pure Go Git
// operations and operate only on repositories supplied by input arguments.
//
// Path handling follows the same broad pattern as FSTool:
//   - workBaseDir: base for resolving relative repoPath values
//   - allowedRoots: optional path allow-list
//   - blockSymlinks: blocks symlink traversal when supported by the path policy
type GitTool struct {
	mu     sync.RWMutex
	cfg    gitToolConfig
	policy fspolicy.FSPolicy
}

type GitToolOption func(*GitTool) error

// WithAllowedRoots restricts all repository paths to be within one of the provided roots.
// Roots are canonicalized by the shared filesystem policy.
func WithAllowedRoots(roots []string) GitToolOption {
	return func(gt *GitTool) error {
		gt.cfg.allowedRoots = roots
		return nil
	}
}

// WithWorkBaseDir sets the base directory used to resolve relative repoPath values.
func WithWorkBaseDir(base string) GitToolOption {
	return func(gt *GitTool) error {
		gt.cfg.workBaseDir = base
		return nil
	}
}

// WithBlockSymlinks configures whether symlink traversal should be blocked.
func WithBlockSymlinks(block bool) GitToolOption {
	return func(gt *GitTool) error {
		gt.cfg.blockSymlinks = block
		return nil
	}
}

// WithDefaultAuthor sets the fallback author used by Commit when neither tool
// args nor repository config provide an author.
func WithDefaultAuthor(name, email string) GitToolOption {
	return func(gt *GitTool) error {
		gt.cfg.defaultAuthorName = name
		gt.cfg.defaultAuthorEmail = email
		return nil
	}
}

func NewGitTool(opts ...GitToolOption) (*GitTool, error) {
	gt := &GitTool{
		cfg: gitToolConfig{
			allowedRoots:       nil,
			workBaseDir:        "",
			blockSymlinks:      false,
			defaultAuthorName:  "llmtools git tool",
			defaultAuthorEmail: "llmtools-git@example.invalid",
		},
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(gt); err != nil {
			return nil, err
		}
	}

	pol, err := fspolicy.New(gt.cfg.workBaseDir, gt.cfg.allowedRoots, gt.cfg.blockSymlinks)
	if err != nil {
		return nil, err
	}
	gt.policy = pol

	return gt, nil
}

func (gt *GitTool) StatusTool() spec.Tool       { return toolutil.CloneTool(statusTool) }
func (gt *GitTool) DiffTool() spec.Tool         { return toolutil.CloneTool(diffTool) }
func (gt *GitTool) LogTool() spec.Tool          { return toolutil.CloneTool(logTool) }
func (gt *GitTool) ShowTool() spec.Tool         { return toolutil.CloneTool(showTool) }
func (gt *GitTool) BranchesTool() spec.Tool     { return toolutil.CloneTool(branchesTool) }
func (gt *GitTool) TagsTool() spec.Tool         { return toolutil.CloneTool(tagsTool) }
func (gt *GitTool) CreateTagTool() spec.Tool    { return toolutil.CloneTool(createTagTool) }
func (gt *GitTool) DeleteTagTool() spec.Tool    { return toolutil.CloneTool(deleteTagTool) }
func (gt *GitTool) ChangedFilesTool() spec.Tool { return toolutil.CloneTool(changedFilesTool) }
func (gt *GitTool) ListTreeTool() spec.Tool     { return toolutil.CloneTool(listTreeTool) }
func (gt *GitTool) ReadBlobTool() spec.Tool     { return toolutil.CloneTool(readBlobTool) }
func (gt *GitTool) FindReposTool() spec.Tool    { return toolutil.CloneTool(findReposTool) }
func (gt *GitTool) AddTool() spec.Tool          { return toolutil.CloneTool(addTool) }
func (gt *GitTool) ResetTool() spec.Tool        { return toolutil.CloneTool(resetTool) }
func (gt *GitTool) CommitTool() spec.Tool       { return toolutil.CloneTool(commitTool) }
func (gt *GitTool) CreateBranchTool() spec.Tool { return toolutil.CloneTool(createBranchTool) }
func (gt *GitTool) CheckoutTool() spec.Tool     { return toolutil.CloneTool(checkoutTool) }
func (gt *GitTool) InitTool() spec.Tool         { return toolutil.CloneTool(initTool) }
func (gt *GitTool) BlameTool() spec.Tool        { return toolutil.CloneTool(blameTool) }
func (gt *GitTool) FileHistoryTool() spec.Tool  { return toolutil.CloneTool(fileHistoryTool) }
func (gt *GitTool) RepoInfoTool() spec.Tool     { return toolutil.CloneTool(repoInfoTool) }
func (gt *GitTool) GrepTool() spec.Tool         { return toolutil.CloneTool(grepTool) }

func (gt *GitTool) Status(ctx context.Context, args StatusArgs) (*StatusOut, error) {
	return toolutil.WithRecoveryResp(func() (*StatusOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return status(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Diff(ctx context.Context, args DiffArgs) (*DiffOut, error) {
	return toolutil.WithRecoveryResp(func() (*DiffOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return diff(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Log(ctx context.Context, args LogArgs) (*LogOut, error) {
	return toolutil.WithRecoveryResp(func() (*LogOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return logCommits(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Show(ctx context.Context, args ShowArgs) (*ShowOut, error) {
	return toolutil.WithRecoveryResp(func() (*ShowOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return show(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Branches(ctx context.Context, args BranchesArgs) (*BranchesOut, error) {
	return toolutil.WithRecoveryResp(func() (*BranchesOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return branches(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Tags(ctx context.Context, args TagsArgs) (*TagsOut, error) {
	return toolutil.WithRecoveryResp(func() (*TagsOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return tags(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) CreateTag(ctx context.Context, args CreateTagArgs) (*CreateTagOut, error) {
	return toolutil.WithRecoveryResp(func() (*CreateTagOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return createTag(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) DeleteTag(ctx context.Context, args DeleteTagArgs) (*DeleteTagOut, error) {
	return toolutil.WithRecoveryResp(func() (*DeleteTagOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return deleteTag(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) ChangedFiles(ctx context.Context, args ChangedFilesArgs) (*ChangedFilesOut, error) {
	return toolutil.WithRecoveryResp(func() (*ChangedFilesOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return changedFiles(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) ListTree(ctx context.Context, args ListTreeArgs) (*ListTreeOut, error) {
	return toolutil.WithRecoveryResp(func() (*ListTreeOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return listTree(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) ReadBlob(ctx context.Context, args ReadBlobArgs) (*ReadBlobOut, error) {
	return toolutil.WithRecoveryResp(func() (*ReadBlobOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return readBlob(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) FindRepos(ctx context.Context, args FindReposArgs) (*FindReposOut, error) {
	return toolutil.WithRecoveryResp(func() (*FindReposOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return findRepos(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Add(ctx context.Context, args AddArgs) (*AddOut, error) {
	return toolutil.WithRecoveryResp(func() (*AddOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return add(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Reset(ctx context.Context, args ResetArgs) (*ResetOut, error) {
	return toolutil.WithRecoveryResp(func() (*ResetOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return reset(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Commit(ctx context.Context, args CommitArgs) (*CommitOut, error) {
	return toolutil.WithRecoveryResp(func() (*CommitOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return commit(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) CreateBranch(ctx context.Context, args CreateBranchArgs) (*CreateBranchOut, error) {
	return toolutil.WithRecoveryResp(func() (*CreateBranchOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return createBranch(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Checkout(ctx context.Context, args CheckoutArgs) (*CheckoutOut, error) {
	return toolutil.WithRecoveryResp(func() (*CheckoutOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return checkout(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Init(ctx context.Context, args InitArgs) (*InitOut, error) {
	return toolutil.WithRecoveryResp(func() (*InitOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return initRepo(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Blame(ctx context.Context, args BlameArgs) (*BlameOut, error) {
	return toolutil.WithRecoveryResp(func() (*BlameOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return blame(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) FileHistory(ctx context.Context, args FileHistoryArgs) (*FileHistoryOut, error) {
	return toolutil.WithRecoveryResp(func() (*FileHistoryOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return fileHistory(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) RepoInfo(ctx context.Context, args RepoInfoArgs) (*RepoInfoOut, error) {
	return toolutil.WithRecoveryResp(func() (*RepoInfoOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return repoInfo(ctx, gt.snapshot(), args)
	})
}

func (gt *GitTool) Grep(ctx context.Context, args GrepArgs) (*GrepOut, error) {
	return toolutil.WithRecoveryResp(func() (*GrepOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		return grep(ctx, gt.snapshot(), args)
	})
}
