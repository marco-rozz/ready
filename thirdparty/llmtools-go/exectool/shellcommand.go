package exectool

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/logutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const shellCommandFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/exectool/shellcommand.ShellCommand"

const (
	bootstrapEnvBeginMarker = "__EXECTOOL_ENV_BEGIN__"
	bootstrapEnvEndMarker   = "__EXECTOOL_ENV_END__"
)

var shellCommandToolSpec = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019bfeda-33f2-7315-9007-de55935d2302",
	Slug:          "shellcommand",
	Version:       spec.VersionOne,
	DisplayName:   "Shell Command",
	Description:   "Execute local shell commands (cross-platform). Supports session-like persistence for workdir/env.",
	Tags:          []string{"exec"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"commands": {
		"type": "array",
		"items": { "type": "string" },
		"description": "List of commands to execute sequentially. Prefer setting workdir rather than using 'cd'."
	},
	"workDir": {
		"type": "string",
		"description": "Working directory to execute in. If omitted and sessionID is used, uses the session workdir; otherwise uses tools workBaseDir."
	},
	"env": {
		"type": "object",
		"additionalProperties": { "type": "string" },
		"description": "Environment variable overrides (merged into the process env + tool base env + session env)."
	},
	"shell": {
		"type": "string",
		"enum": ["auto", "bash", "zsh", "sh", "dash", "ksh", "fish", "pwsh", "powershell", "cmd"],
		"default": "auto",
		"description": "Which shell to run. Prefer 'auto' which a safe default per OS unless explicit per shell behavior is needed."
	},
	"executeParallel": {
		"type": "boolean",
		"default": false,
		"description": "If true, treat commands as independent (do not stop on error)."
	},
	"sessionID": {
		"type": "string",
		"default": "",
		"description": "Optional session identifier. If omitted/empty, a new session is created and returned. Sessions persist workdir and env across calls (not a persistent shell process)."
	}
},
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: shellCommandFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ShellName = executil.ShellName

const (
	ShellNameAuto       ShellName = executil.ShellNameAuto
	ShellNameBash       ShellName = executil.ShellNameBash
	ShellNameZsh        ShellName = executil.ShellNameZsh
	ShellNameSh         ShellName = executil.ShellNameSh
	ShellNameDash       ShellName = executil.ShellNameDash
	ShellNameKsh        ShellName = executil.ShellNameKsh
	ShellNameFish       ShellName = executil.ShellNameFish
	ShellNamePwsh       ShellName = executil.ShellNamePwsh
	ShellNamePowershell ShellName = executil.ShellNamePowershell
	ShellNameCmd        ShellName = executil.ShellNameCmd
)

type ShellCommandArgs struct {
	Commands        []string          `json:"commands,omitempty"`
	WorkDir         string            `json:"workDir,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Shell           ShellName         `json:"shell,omitempty"`
	ExecuteParallel bool              `json:"executeParallel,omitempty"`
	SessionID       string            `json:"sessionID,omitempty"`
}

type ShellCommandExecResult = executil.ShellCommandExecResult

type ShellCommandOut struct {
	SessionID string                   `json:"sessionID,omitempty"`
	WorkDir   string                   `json:"workDir,omitempty"`
	Results   []ShellCommandExecResult `json:"results,omitempty"`
}

func shellCommand(
	ctx context.Context,
	args ShellCommandArgs,
	tp execToolPolicy,
	sessions *executil.SessionStore,
) (out *ShellCommandOut, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessions == nil {
		return nil, errors.New("invalid session store")
	}

	fsPol := tp.fsPolicy
	policy := tp.executionPolicy
	blocked := tp.blockedCommands

	args.SessionID = strings.TrimSpace(args.SessionID)

	// Determine commands early (so we don't create sessions for invalid requests).
	cmds := normalizedCommandList(args)
	if len(cmds) == 0 {
		return nil, errors.New("commands is required")
	}
	maxCmds := effectiveMaxCommands(policy)
	if maxCmds > 0 && len(cmds) > maxCmds {
		return nil, fmt.Errorf("too many commands: %d (max %d)", len(cmds), maxCmds)
	}

	createdSessionID := ""
	defer func() {
		// If we created a session but the call failed, do not leak it.
		if err != nil && createdSessionID != "" {
			sessions.Delete(createdSessionID)
		}
	}()

	// Handle session lifecycle first.
	var sess *executil.ShellSession
	if args.SessionID != "" {
		var ok bool
		sess, ok = sessions.Get(args.SessionID)
		if !ok {
			return nil, fmt.Errorf("unknown sessionID: %s", args.SessionID)
		}
	} else {
		sess = sessions.NewSession()
		args.SessionID = sess.GetID()
		createdSessionID = sess.GetID()
	}

	// Determine effective settings (policy-only).
	timeout := effectiveTimeout(policy)
	maxOut := effectiveMaxOutputBytes(policy)
	maxCmdLen := effectiveMaxCommandLength(policy)

	// "executeParallel=true" => treat commands as independent => do not stop on error.
	stopOnError := !args.ExecuteParallel

	// Determine effective workdir string (args > session > policy base dir), then
	// resolve + verify once using fspolicy.
	workdirCandidate, err := sess.GetEffectiveWorkdir(args.WorkDir, fsPol.WorkBaseDir())
	if err != nil {
		return nil, err
	}
	workdirAbs, err := fsPol.ResolvePath(workdirCandidate, fsPol.WorkBaseDir())
	if err != nil {
		return nil, err
	}
	if err := fsPol.VerifyDirResolved(workdirAbs); err != nil {
		return nil, err
	}

	// Validate env early so we don't:
	//  1) store invalid env into sessions
	//  2) fail later at exec.Start with confusing errors
	if err := validateExecutionEnvMap(args.Env); err != nil {
		return nil, err
	}

	// Determine effective env (process env + tool base env + session env + args env).
	env, err := sess.GetEffectiveEnvWithBase(tp.baseEnv, args.Env)
	if err != nil {
		return nil, err
	}

	// Choose shell + path using process + tool base env + platform overlay.
	// Do not include per-call/session overrides here: command PATH overrides
	// should affect commands inside the shell, not whether the wrapper shell can
	// be found.
	shellLookupEnv, shellEnvErr := executil.EffectiveEnvWithBase(tp.baseEnv, nil)
	if shellEnvErr != nil {
		shellLookupEnv = env
	}
	sel, err := selectShellWithDefaultEnv(ctx, args.Shell, tp.defaultShell, shellLookupEnv)
	if err != nil {
		return nil, err
	}

	// Validate command strings and policy before mutating an existing session.
	// Otherwise a rejected command can still persist WorkDir/Env changes.
	checkedCmds := make([]string, 0, len(cmds))
	for _, one := range cmds {
		command := strings.TrimSpace(one)
		if command == "" {
			continue
		}
		if maxCmdLen > 0 && len(command) > maxCmdLen {
			return nil, fmt.Errorf("command too long (%d bytes; max %d)", len(command), maxCmdLen)
		}
		if strings.ContainsRune(command, '\x00') {
			return nil, errors.New("command contains NUL byte")
		}

		// Always enforce command blocklist. Heuristic checks are optional.
		if err := executil.RejectDangerousCommand(
			command,
			sel.Path,
			sel.Name,
			blocked,
			!policy.AllowDangerous,
		); err != nil {
			return nil, err
		}

		checkedCmds = append(checkedCmds, command)
	}
	if len(checkedCmds) == 0 {
		return nil, errors.New("commands is required")
	}

	// Persist session defaults if caller provided values.
	if strings.TrimSpace(args.WorkDir) != "" {
		sess.SetWorkDir(workdirAbs)
	}
	if err := sess.AddToEnv(args.Env); err != nil {
		return nil, err
	}

	results := make([]ShellCommandExecResult, 0, len(cmds))
	for _, command := range checkedCmds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		res, runErr := executil.RunOneShellCommand(ctx, sel, command, workdirAbs, env, timeout, maxOut)
		if runErr != nil {
			// Return structured output when possible.
			res = ShellCommandExecResult{
				Command:   command,
				WorkDir:   workdirAbs,
				Shell:     sel.Name,
				ShellPath: sel.Path,

				ExitCode:   127,
				TimedOut:   false,
				DurationMS: 0,
				Stdout:     "",
				Stderr:     runErr.Error(),
			}
		}
		results = append(results, res)
		if stopOnError && (res.TimedOut || res.ExitCode != 0) {
			break
		}
	}

	resp := ShellCommandOut{
		SessionID: args.SessionID,
		WorkDir:   workdirAbs,
		Results:   results,
	}
	return &resp, nil
}

func normalizedCommandList(args ShellCommandArgs) []string {
	if len(args.Commands) == 0 {
		return nil
	}
	out := make([]string, 0, len(args.Commands))
	for _, c := range args.Commands {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

func selectShell(ctx context.Context, requested ShellName) (executil.SelectedShell, error) {
	return selectShellWithDefault(ctx, requested, "")
}

func selectShellWithDefault(ctx context.Context, requested, defaultShell ShellName) (executil.SelectedShell, error) {
	env, err := executil.EffectiveEnvWithBase(nil, nil)
	if err != nil {
		env = nil
	}
	return selectShellWithDefaultEnv(ctx, requested, defaultShell, env)
}

func selectShellWithDefaultEnv(
	ctx context.Context,
	requested, defaultShell ShellName,
	env []string,
) (executil.SelectedShell, error) {
	r := strings.ToLower(strings.TrimSpace(string(requested)))
	if r == "" || r == string(ShellNameAuto) {
		d := strings.ToLower(strings.TrimSpace(string(defaultShell)))
		if d != "" && d != string(ShellNameAuto) {
			r = d
		} else {
			r = string(ShellNameAuto)
		}
	}

	if r != string(ShellNameAuto) {
		return resolveShell(ctx, r, env)
	}
	return resolveAutoShell(ctx, env)
}

func resolveAutoShell(ctx context.Context, env []string) (executil.SelectedShell, error) {
	if runtime.GOOS == toolutil.GOOSWindows {
		return resolveWindowsAutoShell(env)
	}
	return resolveUnixAutoShell(ctx, env)
}

func resolveWindowsAutoShell(env []string) (executil.SelectedShell, error) {
	// Prefer pwsh, then Windows PowerShell, then cmd.
	if p, _ := executil.LookPathInEnv("pwsh", env); p != "" {
		return executil.SelectedShell{Name: ShellNamePwsh, Path: p}, nil
	}
	if p, ok := resolveWindowsPowerShellPath(env); ok {
		return executil.SelectedShell{Name: ShellNamePowershell, Path: p}, nil
	}
	if p, ok := resolveWindowsCmdPath(env); ok {
		return executil.SelectedShell{Name: ShellNameCmd, Path: p}, nil
	}
	return executil.SelectedShell{}, errors.New("no suitable shell found on windows (pwsh/powershell/cmd)")
}

func resolveUnixAutoShell(ctx context.Context, env []string) (executil.SelectedShell, error) {
	// In Flatpak: prefer querying the host for the user's real shell.
	if executil.HostSpawnAvailable(ctx) {
		if sel, ok := executil.ResolveHostAutoShell(ctx); ok {
			return sel, nil
		}
		logutil.WarnContext(ctx, "exectool: host shell detection failed; falling back to sandbox shell detection")
	}

	// Prefer $SHELL if present.
	if shellEnv, ok := executil.EnvListValue(env, "SHELL"); ok {
		if sel, ok := selectedShellFromCandidateEnv(shellEnv, env); ok {
			return sel, nil
		}
	} else if sel, ok := selectedShellFromCandidateEnv(os.Getenv("SHELL"), env); ok {
		return sel, nil
	}
	// Then try the user's configured login shell.
	if sel, ok := lookupAccountLoginShell(ctx, env); ok {
		return sel, nil
	}
	// Finally fall back by platform/tool availability.
	for _, name := range []ShellName{
		ShellNameBash,
		ShellNameZsh,
		ShellNameSh,
		ShellNameDash,
		ShellNameKsh,
		ShellNameFish,
	} {
		if p, _ := executil.LookPathInEnv(string(name), env); p != "" {
			return executil.SelectedShell{Name: name, Path: p}, nil
		}
	}
	return executil.SelectedShell{}, errors.New("no suitable shell found (bash/zsh/sh)")
}

func lookupAccountLoginShell(ctx context.Context, env []string) (executil.SelectedShell, bool) {
	u, err := user.Current()
	if err != nil || u == nil {
		return executil.SelectedShell{}, false
	}
	username := strings.TrimSpace(u.Username)
	if username == "" {
		return executil.SelectedShell{}, false
	}

	if runtime.GOOS == toolutil.GOOSDarwin {
		if sel, ok := selectedShellFromCandidateEnv(lookupDarwinUserShell(ctx, username), env); ok {
			return sel, true
		}
	}
	if sel, ok := selectedShellFromCandidateEnv(lookupGetentUserShell(ctx, username), env); ok {
		return sel, true
	}
	if sel, ok := selectedShellFromCandidateEnv(lookupPasswdUserShell(username), env); ok {
		return sel, true
	}
	return executil.SelectedShell{}, false
}

func selectedShellFromCandidateEnv(candidate string, env []string) (executil.SelectedShell, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return executil.SelectedShell{}, false
	}

	p, err := executil.LookPathInEnv(candidate, env)
	if err != nil || p == "" {
		return executil.SelectedShell{}, false
	}

	base := strings.ToLower(filepath.Base(p))
	if runtime.GOOS == toolutil.GOOSWindows {
		switch ext := strings.ToLower(filepath.Ext(base)); ext {
		case ".exe", ".com", ".bat", ".cmd":
			base = strings.TrimSuffix(base, ext)
		}
	}

	norm, err := executil.NormalizeShellName(ShellName(base))
	if err != nil || norm == ShellNameAuto {
		return executil.SelectedShell{}, false
	}
	return executil.SelectedShell{Name: norm, Path: p}, true
}

func lookupDarwinUserShell(ctx context.Context, username string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	//nolint:gosec // Shell discovery hand crafted.
	out, err := exec.CommandContext(ctx, "dscl", ".", "-read", "/Users/"+username, "UserShell").Output()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "UserShell:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func lookupGetentUserShell(ctx context.Context, username string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getent", "passwd", username).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	parts := strings.Split(line, ":")
	if len(parts) < 7 {
		return ""
	}
	return strings.TrimSpace(parts[6])
}

func lookupPasswdUserShell(username string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 || parts[0] != username {
			continue
		}
		return strings.TrimSpace(parts[6])
	}
	return ""
}

func resolveShell(ctx context.Context, name string, env []string) (executil.SelectedShell, error) {
	shellName := ShellName(name)
	switch shellName {
	case ShellNameBash, ShellNameZsh, ShellNameSh, ShellNameDash, ShellNameKsh, ShellNameFish:
		// In Flatpak: resolve the shell path on the host, not the sandbox.
		if executil.HostSpawnAvailable(ctx) {
			if p, ok := lookupHostExecutable(ctx, name); ok {
				return executil.SelectedShell{Name: shellName, Path: p}, nil
			}
			logutil.WarnContext(ctx, "exectool: host lookup for shell failed; trying sandbox PATH", "shellname", name)
		}

		p, err := executil.LookPathInEnv(name, env)
		if err != nil {
			return executil.SelectedShell{}, fmt.Errorf("shell not found: %s", name)
		}
		return executil.SelectedShell{Name: shellName, Path: p}, nil
	case ShellNamePwsh:
		if executil.HostSpawnAvailable(ctx) {
			if p, ok := lookupHostExecutable(ctx, "pwsh"); ok {
				return executil.SelectedShell{Name: ShellNamePwsh, Path: p}, nil
			}
			logutil.WarnContext(ctx, "exectool: host lookup for pwsh failed; trying sandbox PATH")
		}
		p, err := executil.LookPathInEnv("pwsh", env)
		if err != nil {
			return executil.SelectedShell{}, errors.New("pwsh requested but not found")
		}
		return executil.SelectedShell{Name: ShellNamePwsh, Path: p}, nil
	case ShellNamePowershell:
		if executil.HostSpawnAvailable(ctx) {
			if p, ok := lookupHostExecutable(ctx, "powershell"); ok {
				return executil.SelectedShell{Name: ShellNamePowershell, Path: p}, nil
			}
			logutil.WarnContext(ctx, "exectool: host lookup for powershell failed; trying sandbox PATH")
		}
		if p, ok := resolveWindowsPowerShellPath(env); ok {
			return executil.SelectedShell{Name: ShellNamePowershell, Path: p}, nil
		}
		return executil.SelectedShell{}, errors.New("powershell requested but not found")
	case ShellNameCmd:
		if p, ok := resolveWindowsCmdPath(env); ok {
			return executil.SelectedShell{Name: ShellNameCmd, Path: p}, nil
		}
		return executil.SelectedShell{}, errors.New("cmd requested but not found")
	default:
		return executil.SelectedShell{}, fmt.Errorf("invalid shell: %q", name)
	}
}

func resolveWindowsPowerShellPath(env []string) (string, bool) {
	if p, _ := executil.LookPathInEnv("powershell", env); p != "" {
		return p, true
	}
	if runtime.GOOS != toolutil.GOOSWindows {
		return "", false
	}
	root, ok := executil.EnvListValue(env, "SystemRoot")
	if !ok || strings.TrimSpace(root) == "" {
		return "", false
	}
	p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if existingRegularFile(p) {
		return p, true
	}
	return "", false
}

func resolveWindowsCmdPath(env []string) (string, bool) {
	if p, _ := executil.LookPathInEnv("cmd", env); p != "" {
		return p, true
	}
	if runtime.GOOS != toolutil.GOOSWindows {
		return "", false
	}

	if comspec, ok := executil.EnvListValue(env, "COMSPEC"); ok {
		comspec = strings.TrimSpace(comspec)
		if comspec != "" {
			if p, err := executil.LookPathInEnv(comspec, env); err == nil && p != "" {
				return p, true
			}
			if existingRegularFile(comspec) {
				return comspec, true
			}
		}
	}

	if root, ok := executil.EnvListValue(env, "SystemRoot"); ok && strings.TrimSpace(root) != "" {
		p := filepath.Join(root, "System32", "cmd.exe")
		if existingRegularFile(p) {
			return p, true
		}
	}
	return "", false
}

func lookupHostExecutable(ctx context.Context, name string) (string, bool) {
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()

	out, err := executil.HostExec(rctx, "sh", "-c", "command -v "+name)
	if err != nil {
		return "", false
	}
	p := strings.TrimSpace(string(out))
	if i := strings.IndexAny(p, "\r\n"); i >= 0 {
		p = strings.TrimSpace(p[:i])
	}
	if p == "" {
		return "", false
	}
	return p, true
}

func effectiveTimeout(policy ExecutionPolicy) time.Duration {
	d := policy.Timeout
	if d <= 0 {
		d = defaultExecutionTimeout
	}
	if d > hardExecutionTimeout {
		d = hardExecutionTimeout
	}
	return d
}

func effectiveMaxOutputBytes(policy ExecutionPolicy) int64 {
	v := policy.MaxOutputBytes
	if v <= 0 {
		v = defaultExecutionMaxOutputBytes
	}
	v = max(v, int64(minExecutionOutputBytes))
	v = min(v, int64(hardExecutionMaxOutputBytes))
	v = min(v, int64(math.MaxInt))
	return v
}

func effectiveMaxCommands(policy ExecutionPolicy) int {
	v := policy.MaxCommands
	if v <= 0 {
		v = defaultExecutionMaxCommands
	}
	v = max(v, 1)
	v = min(v, hardExecutionMaxCommands)
	return v
}

func effectiveMaxCommandLength(policy ExecutionPolicy) int {
	v := policy.MaxCommandLength
	if v <= 0 {
		v = defaultExecutionMaxCommandLength
	}
	v = max(v, 1)
	v = min(v, hardExecutionMaxCommandLength)
	return v
}

func existingRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
