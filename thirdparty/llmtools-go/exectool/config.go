package exectool

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

// ExecutionPolicy provides policy / hardening knobs (host-configured).
// All limits are clamped to executil hard maximums (downstream enforcement).
type ExecutionPolicy struct {
	// If true, skip heuristic checks (fork-bomb/backgrounding).
	// NOTE: hard-blocked commands are ALWAYS blocked.
	AllowDangerous bool

	Timeout          time.Duration
	MaxOutputBytes   int64
	MaxCommands      int
	MaxCommandLength int
}

type execToolConfig struct {
	allowedRoots    []string
	workBaseDir     string
	blockSymlinks   bool
	blockedCommands map[string]struct{}

	defaultShell    ShellName
	defaultShellSet bool
	baseEnv         map[string]string
	baseEnvSet      bool

	executionPolicy ExecutionPolicy
	runScriptPolicy RunScriptPolicy
}

type execToolPolicy struct {
	fsPolicy        fspolicy.FSPolicy
	blockedCommands map[string]struct{}

	defaultShell ShellName
	baseEnv      map[string]string

	executionPolicy ExecutionPolicy
	runScriptPolicy RunScriptPolicy
}

// Clone returns an independent copy of the policy snapshot.
func (p *execToolPolicy) Clone() *execToolPolicy {
	if p == nil {
		return nil
	}

	cp := new(execToolPolicy)
	*cp = *p // copy all value fields (and slice/map headers)

	if p.blockedCommands != nil {
		cp.blockedCommands = make(map[string]struct{}, len(p.blockedCommands))
		maps.Copy(cp.blockedCommands, p.blockedCommands)
	} else {
		cp.blockedCommands = nil
	}

	cp.baseEnv = maps.Clone(p.baseEnv)

	if c := p.executionPolicy.Clone(); c != nil {
		cp.executionPolicy = *c
	}

	if c := p.runScriptPolicy.Clone(); c != nil {
		cp.runScriptPolicy = *c
	}

	return cp
}

// Clone returns an independent copy of the ExecutionPolicy.
// (All fields are value types, so a plain copy is sufficient.)
func (p *ExecutionPolicy) Clone() *ExecutionPolicy {
	if p == nil {
		return nil
	}
	cp := new(ExecutionPolicy)
	*cp = *p
	return cp
}

func validateExecutionEnvMap(env map[string]string) error {
	if len(env) > hardEnvVars {
		return fmt.Errorf("too many env vars: %d (max %d)", len(env), hardEnvVars)
	}

	totalBytes := 0
	for key, value := range env {
		trimmedKey := strings.TrimSpace(key)
		if len(trimmedKey) > hardEnvKeyBytes {
			return fmt.Errorf("env name too long (%d bytes; max %d)", len(trimmedKey), hardEnvKeyBytes)
		}
		if len(value) > hardEnvValueBytes {
			return fmt.Errorf("env value too long (%d bytes; max %d)", len(value), hardEnvValueBytes)
		}

		totalBytes += len(trimmedKey) + len(value)
		if totalBytes > hardEnvTotalBytes {
			return fmt.Errorf("env overrides too large (max %d bytes)", hardEnvTotalBytes)
		}
	}

	return executil.ValidateEnvMap(env)
}
