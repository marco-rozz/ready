package exectool

import (
	"context"
	"maps"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	defaultBootstrapDetectionTimeout = 3 * time.Second
	defaultBootstrapEnvTimeout       = 8 * time.Second

	defaultExecutionTimeout = 60 * time.Second
	hardExecutionTimeout    = 10 * time.Minute

	minExecutionOutputBytes        = 1024
	defaultExecutionMaxOutputBytes = 256 * 1024
	hardExecutionMaxOutputBytes    = 4 * 1024 * 1024

	defaultExecutionMaxCommands = 64
	hardExecutionMaxCommands    = 128

	defaultExecutionMaxCommandLength = 64 * 1024
	hardExecutionMaxCommandLength    = 64 * 1024

	defaultRunScriptMaxArgs     = 256
	defaultRunScriptMaxArgBytes = 16 * 1024

	defaultSessionTTL  = 30 * time.Minute
	defaultMaxSessions = 256

	hardEnvVars       = 256
	hardEnvKeyBytes   = 256
	hardEnvValueBytes = 32 * 1024
	hardEnvTotalBytes = 256 * 1024
)

// BootstrappedDefaults contains best-effort host-derived defaults suitable for ExecTool setup.
type BootstrappedDefaults struct {
	DefaultShell ShellName
	BaseEnv      map[string]string
}

// ExecTool is an instance-owned execution tool runner.
// It centralizes:
//   - path sandboxing (workBaseDir, allowedRoots, blockSymlinks)
//   - execution policy (timeouts/output/limits)
//   - command blocklist
//   - default shell / bootstrapped base environment
//   - session store for shellcommand
//   - runscript policy (optional tool).
type ExecTool struct {
	mu         sync.RWMutex
	cfg        execToolConfig
	toolPolicy *execToolPolicy
	sessions   *executil.SessionStore
}

type ExecToolOption func(*ExecTool) error

func WithAllowedRoots(roots []string) ExecToolOption {
	return func(et *ExecTool) error {
		et.cfg.allowedRoots = roots
		return nil
	}
}

func WithWorkBaseDir(base string) ExecToolOption {
	return func(et *ExecTool) error {
		et.cfg.workBaseDir = base
		return nil
	}
}

// WithBlockSymlinks configures whether symlink traversal should be blocked (if supported downstream).
func WithBlockSymlinks(block bool) ExecToolOption {
	return func(et *ExecTool) error {
		et.cfg.blockSymlinks = block
		return nil
	}
}

func WithExecutionPolicy(p ExecutionPolicy) ExecToolOption {
	return func(et *ExecTool) error {
		et.cfg.executionPolicy = p
		return nil
	}
}

// WithDefaultShell configures the default shell used when ShellCommandArgs.Shell is omitted or "auto".
func WithDefaultShell(shell ShellName) ExecToolOption {
	return func(et *ExecTool) error {
		norm, err := executil.NormalizeShellName(shell)
		if err != nil {
			return err
		}
		if norm != ShellNameAuto {
			if _, err := selectShell(context.Background(), norm); err != nil {
				return err
			}
		}
		et.cfg.defaultShell = norm
		et.cfg.defaultShellSet = true
		return nil
	}
}

// WithBaseEnv configures a stable base environment merged into executions after the process env.
func WithBaseEnv(env map[string]string) ExecToolOption {
	return func(et *ExecTool) error {
		if err := validateExecutionEnvMap(env); err != nil {
			return err
		}
		et.cfg.baseEnv = maps.Clone(env)
		et.cfg.baseEnvSet = true
		return nil
	}
}

// WithBlockedCommands adds additional commands to the instance blocklist.
// Entries must be single command names (not full command lines).
func WithBlockedCommands(cmds []string) ExecToolOption {
	return func(et *ExecTool) error {
		if et.cfg.blockedCommands == nil {
			et.cfg.blockedCommands = maps.Clone(executil.HardBlockedCommands)
		}

		for _, c := range cmds {
			n, err := executil.NormalizeBlockedCommand(c)
			if err != nil {
				return err
			}
			if n == "" {
				continue
			}

			et.cfg.blockedCommands[n] = struct{}{}

			if runtime.GOOS == toolutil.GOOSWindows {
				ext := strings.ToLower(filepath.Ext(n))
				switch ext {
				case ".exe", ".com", ".bat", ".cmd":
					et.cfg.blockedCommands[strings.TrimSuffix(n, ext)] = struct{}{}
				}
			}
		}
		return nil
	}
}

func WithRunScriptPolicy(p RunScriptPolicy) ExecToolOption {
	return func(et *ExecTool) error {
		norm, err := NormalizeRunScriptPolicy(p)
		if err != nil {
			return err
		}
		et.cfg.runScriptPolicy = norm

		return nil
	}
}

func WithSessionTTL(ttl time.Duration) ExecToolOption {
	return func(et *ExecTool) error {
		if et.sessions == nil {
			et.sessions = executil.NewSessionStore(defaultSessionTTL, defaultMaxSessions)
		}
		et.sessions.SetTTL(ttl)
		return nil
	}
}

func WithMaxSessions(maxSessions int) ExecToolOption {
	return func(et *ExecTool) error {
		if et.sessions == nil {
			et.sessions = executil.NewSessionStore(defaultSessionTTL, defaultMaxSessions)
		}
		et.sessions.SetMaxSessions(maxSessions)
		return nil
	}
}

func NewExecTool(opts ...ExecToolOption) (*ExecTool, error) {
	et := &ExecTool{
		cfg: execToolConfig{
			allowedRoots:    nil,
			workBaseDir:     "",
			blockSymlinks:   false,
			blockedCommands: maps.Clone(executil.HardBlockedCommands),

			executionPolicy: DefaultExecutionPolicy(),
			runScriptPolicy: DefaultRunScriptPolicy(),
		},
		sessions: executil.NewSessionStore(defaultSessionTTL, defaultMaxSessions),
	}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(et); err != nil {
			return nil, err
		}
	}

	et.bootstrapUnsetDefaults()

	// Canonicalize/initialize path policy (fspolicy is the single source of truth).
	fsPol, err := fspolicy.New(et.cfg.workBaseDir, et.cfg.allowedRoots, et.cfg.blockSymlinks)
	if err != nil {
		return nil, err
	}

	// Final defensive normalization (covers defaults and any options that didn't normalize).
	rsPol, err := NormalizeRunScriptPolicy(et.cfg.runScriptPolicy)
	if err != nil {
		return nil, err
	}

	et.toolPolicy = &execToolPolicy{
		fsPolicy:        fsPol,
		blockedCommands: maps.Clone(et.cfg.blockedCommands),
		defaultShell:    et.cfg.defaultShell,
		baseEnv:         maps.Clone(et.cfg.baseEnv),
		executionPolicy: et.cfg.executionPolicy,
		runScriptPolicy: rsPol,
	}

	return et, nil
}

func (et *ExecTool) RunScriptTool() spec.Tool    { return toolutil.CloneTool(runScriptToolSpec) }
func (et *ExecTool) ShellCommandTool() spec.Tool { return toolutil.CloneTool(shellCommandToolSpec) }

func (et *ExecTool) RunScript(ctx context.Context, args RunScriptArgs) (*RunScriptOut, error) {
	return toolutil.WithRecoveryResp(func() (*RunScriptOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := et.snapshotPolicy()
		return runScript(ctx, args, *p)
	})
}

func (et *ExecTool) ShellCommand(ctx context.Context, args ShellCommandArgs) (*ShellCommandOut, error) {
	return toolutil.WithRecoveryResp(func() (*ShellCommandOut, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		p := et.snapshotPolicy()
		return shellCommand(ctx, args, *p, et.sessions)
	})
}

func (et *ExecTool) snapshotPolicy() *execToolPolicy {
	et.mu.RLock()
	p := et.toolPolicy
	et.mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.Clone()
}

func (et *ExecTool) bootstrapUnsetDefaults() {
	if et == nil {
		return
	}
	if et.cfg.defaultShellSet && et.cfg.baseEnvSet {
		return
	}

	defs, err := cachedBootstrapDefaultsForNewExecTool()
	if err != nil && defs == nil {
		return
	}
	if defs == nil {
		return
	}
	if !et.cfg.defaultShellSet && defs.DefaultShell != "" {
		et.cfg.defaultShell = defs.DefaultShell
	}
	if !et.cfg.baseEnvSet && len(defs.BaseEnv) != 0 {
		et.cfg.baseEnv = maps.Clone(defs.BaseEnv)
	}
}

func DefaultExecutionPolicy() ExecutionPolicy {
	return ExecutionPolicy{
		AllowDangerous:   false,
		Timeout:          defaultExecutionTimeout,
		MaxOutputBytes:   defaultExecutionMaxOutputBytes,
		MaxCommands:      defaultExecutionMaxCommands,
		MaxCommandLength: defaultExecutionMaxCommandLength,
	}
}
