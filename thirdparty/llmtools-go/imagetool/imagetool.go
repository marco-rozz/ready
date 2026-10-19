package imagetool

import (
	"context"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	defaultImageReadBytes int64 = 16 * 1024 * 1024
	hardImageReadBytes    int64 = 16 * 1024 * 1024
)

type imageToolConfig struct {
	allowedRoots  []string
	workBaseDir   string
	blockSymlinks bool
}

// ImageTool is an instance-owned image tool runner.
// It centralizes path resolution and sandbox policy:
//   - workBaseDir: base for resolving relative paths
//   - allowedRoots: optional restriction; if empty/nil, allow all
//   - blockSymlinks: blocks symlink traversal (if enforced downstream).
type ImageTool struct {
	mu     sync.RWMutex
	cfg    imageToolConfig
	policy fspolicy.FSPolicy
}

type ImageToolOption func(*ImageTool) error

func WithAllowedRoots(roots []string) ImageToolOption {
	return func(it *ImageTool) error {
		it.cfg.allowedRoots = roots
		return nil
	}
}

func WithWorkBaseDir(base string) ImageToolOption {
	return func(it *ImageTool) error {
		it.cfg.workBaseDir = base
		return nil
	}
}

// WithBlockSymlinks configures whether symlink traversal should be blocked (if supported downstream).
func WithBlockSymlinks(block bool) ImageToolOption {
	return func(it *ImageTool) error {
		it.cfg.blockSymlinks = block
		return nil
	}
}

func NewImageTool(opts ...ImageToolOption) (*ImageTool, error) {
	it := &ImageTool{
		cfg: imageToolConfig{
			allowedRoots:  nil,
			workBaseDir:   "",
			blockSymlinks: false,
		},
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(it); err != nil {
			return nil, err
		}
	}

	pol, err := fspolicy.New(it.cfg.workBaseDir, it.cfg.allowedRoots, it.cfg.blockSymlinks)
	if err != nil {
		return nil, err
	}
	it.policy = pol

	return it, nil
}

func (it *ImageTool) ReadImageTool() spec.Tool { return toolutil.CloneTool(readImageTool) }

func (it *ImageTool) ReadImage(ctx context.Context, args ReadImageArgs) (*ReadImageOut, error) {
	return toolutil.WithRecoveryResp(func() (*ReadImageOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := it.snapshotPolicy()
		return readImage(ctx, args, p)
	})
}

func (it *ImageTool) snapshotPolicy() fspolicy.FSPolicy {
	it.mu.RLock()
	p := it.policy
	it.mu.RUnlock()
	return p
}
