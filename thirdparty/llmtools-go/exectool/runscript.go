package exectool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	defaultPython3Command = "python3"
	defaultPythonCommand  = "python"
)

const runScriptFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/exectool/runscript.RunScript"

var runScriptToolSpec = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c3df0-e332-717f-85d1-3d752f9f6046",
	Slug:          "runscript",
	Version:       spec.VersionOne,
	DisplayName:   "Run Script",
	Description:   "Run an existing script from disk.",
	Tags:          []string{"exec"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path to the script. Can be absolute or relative. If relative and workdir is provided, resolves against workdir; otherwise resolves against the tool workBaseDir."
	},
	"args": {
		"type": "array",
		"items": { "type": "string" },
		"description": "Arguments passed to the script."
	},
	"env": {
		"type": "object",
		"additionalProperties": { "type": "string" },
		"description": "Environment variable overrides (merged into the process env + tool base env)."
	},
	"workDir": {
		"type": "string",
		"description": "Working directory. Can be absolute or relative to workBaseDir."
	}
},
"required": ["path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: runScriptFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type RunScriptArgs struct {
	Path    string            `json:"path"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workDir,omitempty"`
}

type RunScriptOut struct {
	Path       string `json:"path"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	DurationMS int64  `json:"durationMS,omitempty"`

	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool `json:"stderrTruncated,omitempty"`
}

type RunScriptMode string

const (
	// RunScriptModeDirect executes the script path directly (as the "command").
	// This is appropriate for PowerShell scripts executed via "& 'script.ps1' ...".
	RunScriptModeDirect RunScriptMode = "direct"

	// RunScriptModeShell executes the script by invoking the selected wrapper shell as an interpreter:
	//   <shellPath> <script> <args...>
	// This avoids requiring execute bits on Unix shell scripts.
	RunScriptModeShell RunScriptMode = "shell"

	// RunScriptModeInterpreter executes the script via an explicit interpreter command:
	//   <command> <commandArgs...> <script> <args...>
	RunScriptModeInterpreter RunScriptMode = "interpreter"
)

type RunScriptInterpreter struct {
	// Shell selects the wrapper shell used to run the *constructed command string*.
	// This affects quoting dialect and which binary is used for "shell -c" / "-Command".
	Shell ShellName

	Mode RunScriptMode

	// Command is required for ModeInterpreter.
	// Examples: "python3", "python", "node", "ruby".
	Command string
	Args    []string
}

type RunScriptPolicy struct {
	// AllowedExtensions is an optional lowercase allowlist (e.g. [.sh, .ps1, .py]).
	// If empty/nil, extension is allowed iff InterpreterByExtension has a match (or a "" fallback is configured).
	AllowedExtensions []string

	// InterpreterByExtension controls how scripts are executed based on extension.
	// Keys should be lowercase and include the leading dot (".sh", ".ps1", ".py").
	//
	// If no mapping exists for the script extension:
	//   - if a mapping for "" exists, it is used as a fallback
	//   - otherwise runscript fails with "no interpreter mapping for extension"
	InterpreterByExtension map[string]RunScriptInterpreter

	// ExecutionPolicy overrides the ExecTool-wide defaults for runscript.
	// If left zero-valued, ExecTool.execPolicy is used.
	ExecutionPolicy ExecutionPolicy

	// Arg limits (defense-in-depth).
	MaxArgs     int
	MaxArgBytes int
}

// Clone returns an independent copy of the RunScriptPolicy.
// It deep-copies reference fields (slice/map). ExecutionPolicy is value-copied.
//
// Note: Map values (RunScriptInterpreter) are copied as-is. If RunScriptInterpreter
// contains reference types that must be independent, add a Clone() for it and
// use that in the map copy loop.
func (p *RunScriptPolicy) Clone() *RunScriptPolicy {
	if p == nil {
		return nil
	}

	cp := new(RunScriptPolicy)
	*cp = *p // copies scalar fields + slice/map headers (we fix those below)

	if p.AllowedExtensions != nil {
		cp.AllowedExtensions = make([]string, len(p.AllowedExtensions))
		copy(cp.AllowedExtensions, p.AllowedExtensions)
	} else {
		cp.AllowedExtensions = nil
	}

	if p.InterpreterByExtension != nil {
		cp.InterpreterByExtension = make(map[string]RunScriptInterpreter, len(p.InterpreterByExtension))
		for k, v := range p.InterpreterByExtension {
			v.Args = slices.Clone(v.Args)
			cp.InterpreterByExtension[k] = v
		}
	} else {
		cp.InterpreterByExtension = nil
	}

	cp.ExecutionPolicy = *p.ExecutionPolicy.Clone()

	return cp
}

func DefaultRunScriptPolicy() RunScriptPolicy {
	pyCmd := defaultPython3Command
	pyShell := ShellNameSh
	psShell := ShellNamePwsh
	if runtime.GOOS == toolutil.GOOSWindows {
		pyCmd = defaultPythonCommand
		pyShell = ShellNamePowershell
		psShell = ShellNamePowershell
	}
	allowed := []string{
		string(ioutil.ExtShell),
		string(ioutil.ExtBash),
		string(ioutil.ExtZsh),
		string(ioutil.ExtKsh),
		string(ioutil.ExtDash),
		string(ioutil.ExtPS1),
		string(ioutil.ExtPY),
	}

	interpreters := map[string]RunScriptInterpreter{
		// Shell scripts: run via the wrapper shell path as interpreter.
		string(ioutil.ExtShell): {Shell: ShellNameSh, Mode: RunScriptModeShell},
		string(ioutil.ExtBash):  {Shell: ShellNameBash, Mode: RunScriptModeShell},
		string(ioutil.ExtZsh):   {Shell: ShellNameZsh, Mode: RunScriptModeShell},
		string(ioutil.ExtKsh):   {Shell: ShellNameKsh, Mode: RunScriptModeShell},
		string(ioutil.ExtDash):  {Shell: ShellNameDash, Mode: RunScriptModeShell},

		// PowerShell: execute the script directly via PowerShell dialect ("& 'script.ps1' ...").
		string(ioutil.ExtPS1): {Shell: psShell, Mode: RunScriptModeDirect},

		// Python: interpreter-based.
		string(ioutil.ExtPY): {Shell: pyShell, Mode: RunScriptModeInterpreter, Command: pyCmd},
	}

	if runtime.GOOS == toolutil.GOOSWindows {
		allowed = append(allowed, string(ioutil.ExtBAT), string(ioutil.ExtCMD))
		interpreters[string(ioutil.ExtBAT)] = RunScriptInterpreter{Shell: ShellNameCmd, Mode: RunScriptModeDirect}
		interpreters[string(ioutil.ExtCMD)] = RunScriptInterpreter{Shell: ShellNameCmd, Mode: RunScriptModeDirect}
	}

	return RunScriptPolicy{
		AllowedExtensions:      allowed,
		InterpreterByExtension: interpreters,
		ExecutionPolicy:        ExecutionPolicy{}, // inherit from ExecTool by default
		MaxArgs:                defaultRunScriptMaxArgs,
		MaxArgBytes:            defaultRunScriptMaxArgBytes,
	}
}

// NormalizeRunScriptPolicy deep-clones and normalizes a RunScriptPolicy.
// This is defensive: it prevents shared map/slice backing storage and makes
// policy behavior deterministic (lowercased extensions, leading dots, etc).
func NormalizeRunScriptPolicy(in RunScriptPolicy) (RunScriptPolicy, error) {
	out := in

	// Normalize numeric limits.
	if out.MaxArgs < 0 {
		return RunScriptPolicy{}, errors.New("runscript policy: MaxArgs must be >= 0")
	}
	if out.MaxArgBytes < 0 {
		return RunScriptPolicy{}, errors.New("runscript policy: MaxArgBytes must be >= 0")
	}
	if out.MaxArgs == 0 {
		out.MaxArgs = defaultRunScriptMaxArgs
	}
	if out.MaxArgBytes == 0 {
		out.MaxArgBytes = defaultRunScriptMaxArgBytes
	}

	// AllowedExtensions: clone + normalize + stable-dedup (preserve order).
	if out.AllowedExtensions != nil {
		seen := map[string]struct{}{}
		norm := make([]string, 0, len(out.AllowedExtensions))
		for _, e := range out.AllowedExtensions {
			x := strings.ToLower(strings.TrimSpace(e))
			if strings.ContainsRune(x, '\x00') {
				return RunScriptPolicy{}, errors.New("runscript policy: AllowedExtensions contains NUL byte")
			}
			if x != "" && !strings.HasPrefix(x, ".") {
				x = "." + x
			}
			if _, ok := seen[x]; ok {
				continue
			}
			seen[x] = struct{}{}
			norm = append(norm, x)
		}
		out.AllowedExtensions = norm
	}

	// InterpreterByExtension: deep clone + normalize keys.
	if out.InterpreterByExtension != nil {
		m := make(map[string]RunScriptInterpreter, len(out.InterpreterByExtension))
		for k, v := range out.InterpreterByExtension {
			key := strings.ToLower(strings.TrimSpace(k))
			if strings.ContainsRune(key, '\x00') {
				return RunScriptPolicy{}, errors.New("runscript policy: InterpreterByExtension key contains NUL byte")
			}
			if key != "" && !strings.HasPrefix(key, ".") {
				key = "." + key
			}

			// Defensive clone of args slice (RunScriptInterpreter contains []string).
			v.Args = slices.Clone(v.Args)
			if strings.TrimSpace(string(v.Shell)) == "" {
				v.Shell = ShellNameAuto
			} else {
				normShell, err := executil.NormalizeShellName(v.Shell)
				if err != nil {
					return RunScriptPolicy{}, fmt.Errorf("runscript policy: invalid shell for %q: %w", key, err)
				}
				v.Shell = normShell
			}

			v.Command = strings.TrimSpace(v.Command)
			if strings.ContainsRune(v.Command, '\x00') {
				return RunScriptPolicy{}, fmt.Errorf("runscript policy: command for %q contains NUL byte", key)
			}
			for i, a := range v.Args {
				if strings.ContainsRune(a, '\x00') {
					return RunScriptPolicy{}, fmt.Errorf("runscript policy: args[%d] for %q contains NUL byte", i, key)
				}
			}
			// Validate mapping is internally consistent.
			switch v.Mode {
			case RunScriptModeDirect, RunScriptModeShell, RunScriptModeInterpreter:
			default:
				return RunScriptPolicy{}, fmt.Errorf("runscript policy: invalid mode for %q: %q", key, v.Mode)
			}
			if v.Mode == RunScriptModeInterpreter && strings.TrimSpace(v.Command) == "" {
				return RunScriptPolicy{}, fmt.Errorf(
					"runscript policy: interpreter mapping for %q has empty Command",
					key,
				)
			}
			m[key] = v
		}
		out.InterpreterByExtension = m
	}

	return out, nil
}

func runScript(
	ctx context.Context,
	args RunScriptArgs,
	tp execToolPolicy,
) (*RunScriptOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fsPol := tp.fsPolicy
	defaultExecPol := tp.executionPolicy
	blocked := tp.blockedCommands
	pol := tp.runScriptPolicy

	reqPath := strings.TrimSpace(args.Path)
	if reqPath == "" {
		return nil, errors.New("path is required")
	}
	// WorkDir: absolute or relative; default to policy work base dir.
	workdirAbs, err := fsPol.ResolvePath(args.WorkDir, fsPol.WorkBaseDir())
	if err != nil {
		return nil, err
	}
	if err := fsPol.VerifyDirResolved(workdirAbs); err != nil {
		return nil, err
	}

	// Resolve script path:
	// - relative => workBaseDir
	// - absolute => must still be within allowedRoots (if configured).
	// If relative and workdir provided, resolve relative to workdir.
	scriptInput := reqPath
	if strings.TrimSpace(args.WorkDir) != "" && !filepath.IsAbs(reqPath) {
		scriptInput = filepath.Join(workdirAbs, reqPath)
	}
	scriptAbs, err := fsPol.ResolvePath(scriptInput, "")
	if err != nil {
		return nil, err
	}

	// Require existing, regular, non-symlink file and refuse symlink traversal in parents.
	if _, err := fsPol.RequireExistingRegularFileResolved(scriptAbs); err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(scriptAbs))
	if len(pol.AllowedExtensions) != 0 && !extAllowed(ext, pol.AllowedExtensions) {
		return nil, fmt.Errorf("script extension %q is not allowed", ext)
	}

	// Validate env + args.
	if err := validateExecutionEnvMap(args.Env); err != nil {
		return nil, err
	}
	if pol.MaxArgs > 0 && len(args.Args) > pol.MaxArgs {
		return nil, fmt.Errorf("too many args: %d (max %d)", len(args.Args), pol.MaxArgs)
	}
	maxArgBytes := pol.MaxArgBytes
	if maxArgBytes <= 0 {
		maxArgBytes = defaultRunScriptMaxArgBytes
	}
	for i, a := range args.Args {
		if strings.ContainsRune(a, '\x00') {
			return nil, fmt.Errorf("args[%d] contains NUL byte", i)
		}
		if len(a) > maxArgBytes {
			return nil, fmt.Errorf("args[%d] too long", i)
		}
	}

	interp, ok := lookupInterpreter(pol, ext)
	if !ok {
		return nil, fmt.Errorf("no interpreter mapping for extension %q", ext)
	}

	// Select wrapper shell (concrete shell needed for quoting + execution).
	// Merge env like shellcommand does (process env + tool base env + overrides), but no session.
	env, err := executil.EffectiveEnvWithBase(tp.baseEnv, args.Env)
	if err != nil {
		return nil, err
	}

	shellLookupEnv, shellEnvErr := executil.EffectiveEnvWithBase(tp.baseEnv, nil)
	if shellEnvErr != nil {
		shellLookupEnv = env
	}

	// Select wrapper shell (concrete shell needed for quoting + execution).
	sel, err := selectShellWithDefaultEnv(ctx, interp.Shell, tp.defaultShell, shellLookupEnv)
	if err != nil {
		return nil, err
	}

	scriptArg := runScriptPathArgForShell(sel.Name, scriptAbs)

	// On Windows, RunScriptModeDirect with ShellNameCmd (.cmd/.bat files) must
	// bypass CommandFromArgv.  The normal path double-quotes the script path
	// inside the /c command string; exec.Command's EscapeArg then escapes those
	// double-quotes as \", causing cmd.exe to see \"C:\path\s.cmd\" and fail.
	//
	// RunOneCmdBatchScript passes the path and args as separate exec arguments,
	// so EscapeArg applies exactly one layer of quoting – which is what cmd.exe
	// expects from its own command-line parser.
	if runtime.GOOS == toolutil.GOOSWindows &&
		sel.Name == ShellNameCmd &&
		interp.Mode == RunScriptModeDirect {

		execPol := mergeExecutionPolicy(defaultExecPol, pol.ExecutionPolicy)
		timeout := effectiveTimeout(execPol)
		maxOut := effectiveMaxOutputBytes(execPol)
		maxCmdLen := effectiveMaxCommandLength(execPol)

		// Build a plain check string for the safety/blocklist check.
		checkStr := scriptAbs
		if len(args.Args) > 0 {
			checkStr += " " + strings.Join(args.Args, " ")
		}
		if maxCmdLen > 0 && len(checkStr) > maxCmdLen {
			return nil, fmt.Errorf(
				"constructed command too long (%d bytes; max %d)",
				len(checkStr), maxCmdLen,
			)
		}
		if err := executil.RejectDangerousCommand(
			checkStr, sel.Path, sel.Name, blocked, !execPol.AllowDangerous,
		); err != nil {
			return nil, err
		}

		res, runErr := executil.RunOneCmdBatchScript(
			ctx, sel, scriptAbs, args.Args, workdirAbs, env, timeout, maxOut,
		)
		if runErr != nil {
			return &RunScriptOut{ //nolint:nilerr // Deliberate exitcode conversion.
				Path:     scriptAbs,
				ExitCode: 127,
				Stderr:   runErr.Error(),
			}, nil
		}
		return &RunScriptOut{
			Path:            scriptAbs,
			ExitCode:        res.ExitCode,
			Stdout:          res.Stdout,
			Stderr:          res.Stderr,
			TimedOut:        res.TimedOut,
			DurationMS:      res.DurationMS,
			StdoutTruncated: res.StdoutTruncated,
			StderrTruncated: res.StderrTruncated,
		}, nil
	}

	// Build argv based on mode.
	var argv []string
	switch interp.Mode {
	case RunScriptModeDirect:
		// Execute script path directly.
		argv = append([]string{scriptArg}, args.Args...)
	case RunScriptModeShell:
		// Use the wrapper shell binary as the interpreter.
		argv = append([]string{runScriptInterpreterCommand(sel), scriptArg}, args.Args...)
	case RunScriptModeInterpreter:
		cmd := strings.TrimSpace(interp.Command)
		if cmd == "" {
			return nil, errors.New("invalid interpreter mapping: empty command")
		}
		argv = append(argv, cmd)
		argv = append(argv, interp.Args...)
		argv = append(argv, scriptArg)
		argv = append(argv, args.Args...)
	default:
		return nil, fmt.Errorf("invalid interpreter mode: %q", interp.Mode)
	}

	// Convert argv into a safely-quoted command string for the selected wrapper shell.
	cmdStr, err := executil.CommandFromArgv(sel.Name, argv)
	if err != nil {
		return nil, err
	}
	cmdStrExec, cmdStrCheck := cmdStr, cmdStr

	// PowerShell/Pwsh: when running external commands or scripts via optionCommand,
	// use the call operator (&). Without it, script execution often fails, and
	// executable paths with spaces can fail.
	//
	// But for safety checks (blocklist/heuristics), we want the command string
	// without the leading '&' so parsers don't treat '&' as the "command".
	if sel.Name == ShellNamePwsh || sel.Name == ShellNamePowershell {
		trimmed := strings.TrimSpace(cmdStr)
		if !strings.HasPrefix(trimmed, "&") {
			cmdStrExec = "& " + trimmed
			cmdStrCheck = trimmed
		} else {
			cmdStrExec = trimmed
			cmdStrCheck = strings.TrimSpace(strings.TrimPrefix(trimmed, "&"))
			if cmdStrCheck == "" {
				cmdStrCheck = trimmed
			}
		}
	}

	// Effective execution policy: runscript fields override ExecTool defaults
	// field-by-field. Zero numeric fields inherit. AllowDangerous=true is
	// additive because ExecutionPolicy has no tri-state bool.
	execPol := mergeExecutionPolicy(defaultExecPol, pol.ExecutionPolicy)

	timeout := effectiveTimeout(execPol)
	maxOut := effectiveMaxOutputBytes(execPol)
	maxCmdLen := effectiveMaxCommandLength(execPol)

	// Defense-in-depth: bound constructed command length (similar to shellcommand).
	if maxCmdLen > 0 && (len(cmdStrExec) > maxCmdLen || len(cmdStrCheck) > maxCmdLen) {
		return nil, fmt.Errorf(
			"constructed command too long (%d bytes; max %d)",
			max(len(cmdStrExec), len(cmdStrCheck)),
			maxCmdLen,
		)
	}

	// Apply the same outer-command checks (blocklist always, heuristics optional).
	if err := executil.RejectDangerousCommand(
		cmdStrCheck,
		sel.Path,
		sel.Name,
		blocked,
		!execPol.AllowDangerous,
	); err != nil {
		return nil, err
	}

	res, runErr := executil.RunOneShellCommand(ctx, sel, cmdStrExec, workdirAbs, env, timeout, maxOut)
	if runErr != nil {
		return &RunScriptOut{ //nolint:nilerr // For shell exec, we return a exit code on err.
			Path:     scriptAbs,
			ExitCode: 127,
			Stderr:   runErr.Error(),
		}, nil
	}

	return &RunScriptOut{
		Path:       scriptAbs,
		ExitCode:   res.ExitCode,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		TimedOut:   res.TimedOut,
		DurationMS: res.DurationMS,

		StdoutTruncated: res.StdoutTruncated,
		StderrTruncated: res.StderrTruncated,
	}, nil
}

func extAllowed(ext string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	// Allow extension-less scripts if explicitly configured.
	if strings.TrimSpace(ext) == "" {
		for _, a := range allowed {
			if strings.TrimSpace(strings.ToLower(a)) == "" {
				return true
			}
		}
		return false
	}

	x := strings.ToLower(ext)
	for _, a := range allowed {
		if strings.ToLower(strings.TrimSpace(a)) == x {
			return true
		}
	}
	return false
}

func lookupInterpreter(pol RunScriptPolicy, ext string) (RunScriptInterpreter, bool) {
	if pol.InterpreterByExtension == nil {
		return RunScriptInterpreter{}, false
	}
	// Exact match.
	if v, ok := pol.InterpreterByExtension[strings.ToLower(ext)]; ok {
		return v, true
	}
	// Optional fallback for extension-less scripts / shebang-style files.
	if v, ok := pol.InterpreterByExtension[""]; ok {
		return v, true
	}
	return RunScriptInterpreter{}, false
}

func runScriptPathArgForShell(shell ShellName, p string) string {
	if runtime.GOOS == toolutil.GOOSWindows && isRunScriptShLikeShell(shell) {
		// Git Bash/MSYS-like shells are much more reliable with "C:/" than "C:\" when the path is interpreted by the
		// shell.
		return filepath.ToSlash(p)
	}
	return p
}

func runScriptInterpreterCommand(sel executil.SelectedShell) string {
	if runtime.GOOS == toolutil.GOOSWindows && isRunScriptShLikeShell(sel.Name) {
		// The outer shell is already the selected shell. Re-invoking by command
		// name avoids trying to execute a Windows C:\...\sh.exe path from inside
		// a POSIX-like shell.
		return string(sel.Name)
	}
	return sel.Path
}

func isRunScriptShLikeShell(shell ShellName) bool {
	switch shell {
	case ShellNameBash, ShellNameZsh, ShellNameSh, ShellNameDash, ShellNameKsh, ShellNameFish:
		return true
	default:
		return false
	}
}

func mergeExecutionPolicy(base, override ExecutionPolicy) ExecutionPolicy {
	out := base
	if override.Timeout > 0 {
		out.Timeout = override.Timeout
	}
	if override.MaxOutputBytes > 0 {
		out.MaxOutputBytes = override.MaxOutputBytes
	}
	if override.MaxCommands > 0 {
		out.MaxCommands = override.MaxCommands
	}
	if override.MaxCommandLength > 0 {
		out.MaxCommandLength = override.MaxCommandLength
	}
	if override.AllowDangerous {
		out.AllowDangerous = true
	}
	return out
}
