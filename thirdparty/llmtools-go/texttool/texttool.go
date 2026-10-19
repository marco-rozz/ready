package texttool

import (
	"context"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	hardTextProcessingBytes int64 = 16 * 1024 * 1024

	defaultReadTextRangeLineCount = 1000
	hardReadTextRangeOutputLines  = 16000

	hardFindTextContextLines       = 2000
	hardFindTextMaxMatches         = 500
	hardFindTextTotalReturnedLines = 4000

	hardUnifiedDiffBytes                   = 4 * 1024 * 1024
	hardUnifiedDiffFiles                   = 128
	hardUnifiedDiffHunks                   = 2048
	hardUnifiedDiffTargets                 = 256
	hardUnifiedDiffCandidates              = 2048
	hardCandidatePathsPerFile              = 32
	hardUnifiedDiffDiagnosticCandidates    = 16
	hardUnifiedDiffParserDiagnostics       = 64
	hardUnifiedDiffNewParentCreations      = 8
	hardUnifiedDiffHunkNearbyLineTolerance = 16

	textBlockEditLineHintTolerance                = 8
	textBlockEditMaxAmbiguityDiagnosticCandidates = 5
	textBlockEditAmbiguityDiagnosticContextLines  = 1
	maybeStartLineTolerance                       = 3
	maxAmbiguityDiagnosticCandidates              = 5
	ambiguityDiagnosticContextLines               = 1
	toolTagText                                   = "text"
)

type textToolConfig struct {
	allowedRoots  []string
	workBaseDir   string
	blockSymlinks bool
}

// TextTool is an instance-owned text tool runner.
// It centralizes path resolution and sandbox policy:
//   - workBaseDir: base for resolving relative paths
//   - allowedRoots: optional restriction; if empty/nil, allow all
//   - blockSymlinks: blocks symlink traversal (if enforced downstream).
type TextTool struct {
	mu     sync.RWMutex
	cfg    textToolConfig
	policy fspolicy.FSPolicy
}

type TextToolOption func(*TextTool) error

func WithAllowedRoots(roots []string) TextToolOption {
	return func(tt *TextTool) error {
		tt.cfg.allowedRoots = roots
		return nil
	}
}

func WithWorkBaseDir(base string) TextToolOption {
	return func(tt *TextTool) error {
		tt.cfg.workBaseDir = base
		return nil
	}
}

// WithBlockSymlinks configures whether symlink traversal should be blocked (if supported downstream).
func WithBlockSymlinks(block bool) TextToolOption {
	return func(tt *TextTool) error {
		tt.cfg.blockSymlinks = block
		return nil
	}
}

func NewTextTool(opts ...TextToolOption) (*TextTool, error) {
	tt := &TextTool{
		cfg: textToolConfig{
			allowedRoots:  nil,
			workBaseDir:   "",
			blockSymlinks: false,
		},
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(tt); err != nil {
			return nil, err
		}
	}

	pol, err := fspolicy.New(tt.cfg.workBaseDir, tt.cfg.allowedRoots, tt.cfg.blockSymlinks)
	if err != nil {
		return nil, err
	}
	tt.policy = pol
	return tt, nil
}

func (tt *TextTool) DeleteTextTool() spec.Tool       { return toolutil.CloneTool(deleteTextTool) }
func (tt *TextTool) FindTextTool() spec.Tool         { return toolutil.CloneTool(findTextTool) }
func (tt *TextTool) InsertTextTool() spec.Tool       { return toolutil.CloneTool(insertTextTool) }
func (tt *TextTool) ReadTextRangeTool() spec.Tool    { return toolutil.CloneTool(readTextRangeTool) }
func (tt *TextTool) ReplaceTextTool() spec.Tool      { return toolutil.CloneTool(replaceTextTool) }
func (tt *TextTool) ApplyUnifiedDiffTool() spec.Tool { return toolutil.CloneTool(applyUnifiedDiffTool) }

func (tt *TextTool) DeleteText(ctx context.Context, args DeleteTextArgs) (*DeleteTextOut, error) {
	return toolutil.WithRecoveryResp(func() (*DeleteTextOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := tt.snapshotPolicy()
		return deleteText(ctx, args, p)
	})
}

func (tt *TextTool) FindText(ctx context.Context, args FindTextArgs) (*FindTextOut, error) {
	return toolutil.WithRecoveryResp(func() (*FindTextOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := tt.snapshotPolicy()
		return findText(ctx, args, p)
	})
}

func (tt *TextTool) InsertText(ctx context.Context, args InsertTextArgs) (*InsertTextOut, error) {
	return toolutil.WithRecoveryResp(func() (*InsertTextOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := tt.snapshotPolicy()
		return insertText(ctx, args, p)
	})
}

func (tt *TextTool) ReadTextRange(ctx context.Context, args ReadTextRangeArgs) (*ReadTextRangeOut, error) {
	return toolutil.WithRecoveryResp(func() (*ReadTextRangeOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := tt.snapshotPolicy()
		return readTextRange(ctx, args, p)
	})
}

func (tt *TextTool) ReplaceText(ctx context.Context, args ReplaceTextArgs) (*ReplaceTextOut, error) {
	return toolutil.WithRecoveryResp(func() (*ReplaceTextOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := tt.snapshotPolicy()
		return replaceText(ctx, args, p)
	})
}

func (tt *TextTool) ApplyUnifiedDiff(ctx context.Context, args ApplyUnifiedDiffArgs) (*ApplyUnifiedDiffOut, error) {
	return toolutil.WithRecoveryResp(func() (*ApplyUnifiedDiffOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return tt.applyUnifiedDiff(ctx, args)
	})
}

func (tt *TextTool) snapshotPolicy() fspolicy.FSPolicy {
	tt.mu.RLock()
	p := tt.policy
	tt.mu.RUnlock()
	return p
}
