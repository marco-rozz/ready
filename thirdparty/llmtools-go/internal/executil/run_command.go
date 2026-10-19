package executil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/flexigpt/llmtools-go/internal/logutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const managedCommandWaitDelay = 5 * time.Second

// RunOneCmdBatchScript executes a Windows batch script (.cmd or .bat) via
// cmd.exe, passing the script path and every argument as a SEPARATE argument
// to exec.Command rather than embedding them in a /c command string.
//
// Background: the normal RunOneShellCommand path calls CommandFromArgv for the
// cmd dialect, which wraps the script path in cmd-style double-quotes
// ("C:\path\s.cmd").  When exec.Command (via syscall.EscapeArg) receives a
// string that already contains '"' it escapes them as \", producing "\"C:\path\s.cmd\"" in the raw Windows process
// command line. "cmd.exe" with /s strips the outer pair of double-quotes, leaving the literal
// \"C:\path\s.cmd\" which it cannot resolve to a file.
//
// By passing scriptPath and scriptArgs as separate exec.Command arguments,
// each is handled independently by EscapeArg:
//
//   - A path without spaces is left unchanged → cmd.exe sees it directly.
//   - A path with spaces is wrapped in a single "…" → without /s, Windows
//     condition 1 (exactly two quotes, whitespace between, points at a
//     real executable) preserves the quotes and cmd.exe resolves the path.
//
// Known limitation: a path-with-spaces combined with at least one
// arg-with-spaces produces four or more quote characters in the cmd.exe
// command line, causing condition 1 to fail and condition 2 (strip first/last
// quote) to garble the command.  This edge case requires SysProcAttr.CmdLine
// construction to fix correctly and is left for a future change.
func RunOneCmdBatchScript(
	parent context.Context,
	cmdShell SelectedShell,
	scriptPath string,
	scriptArgs []string,
	workdir string,
	env []string,
	timeout time.Duration,
	maxOut int64,
) (ShellCommandExecResult, error) {
	ctx := parent
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
		defer cancel()
	}

	// "cmd.exe" /d /v:off /c <scriptPath> [scriptArgs...]
	//     /d  – disable AutoRun registry keys,
	//     /v:off – disable delayed variable expansion (prevents !VAR! surprises),
	//     /c  – run command then exit
	// NO /s: without /s, Windows condition 1 applies for paths with spaces
	//         (quotes are preserved when there are exactly two quote chars,
	//          whitespace between them, and the string names a real file).
	baseArgs := make([]string, 0, 5+len(scriptArgs))
	baseArgs = append(baseArgs, cmdShell.Path, "/d", "/v:off", "/c", scriptPath)
	baseArgs = append(baseArgs, scriptArgs...)

	execArgs := baseArgs
	execEnv := env
	useHostSpawn := HostSpawnAvailable(ctx)
	if useHostSpawn {
		execArgs = buildHostSpawnArgs(baseArgs, filterEnvForHostSpawn(env), workdir)
		execEnv = nil
	}

	stdoutW := newCappedWriter(maxOut)
	stderrW := newCappedWriter(maxOut)
	cmd, state, prepErr := prepareManagedCommand(ctx, execArgs, workdir, execEnv, stdoutW, stderrW)
	if prepErr != nil {
		return ShellCommandExecResult{}, prepErr
	}

	start := time.Now()
	runErr := cmd.Start()
	if runErr != nil && useHostSpawn {
		logutil.WarnContext(ctx, "executil: host-spawn start failed; retrying direct", "err", runErr)
		stdoutW = newCappedWriter(maxOut)
		stderrW = newCappedWriter(maxOut)
		cmd, state, prepErr = prepareManagedCommand(ctx, baseArgs, workdir, env, stdoutW, stderrW)
		if prepErr != nil {
			return ShellCommandExecResult{}, prepErr
		}
		start = time.Now()
		runErr = cmd.Start()
	}
	if runErr != nil {
		return ShellCommandExecResult{}, runErr
	}

	state.afterStart(ctx, cmd)
	defer state.close()

	waitErr := cmd.Wait()
	dur := time.Since(start)
	timedOut := state.wasCancelled() && errors.Is(ctx.Err(), context.DeadlineExceeded)
	exitCode := exitCodeFromWait(waitErr, timedOut)

	displayCmd := scriptPath
	if len(scriptArgs) > 0 {
		displayCmd += " " + strings.Join(scriptArgs, " ")
	}

	return ShellCommandExecResult{
		Command:         displayCmd,
		WorkDir:         workdir,
		Shell:           cmdShell.Name,
		ShellPath:       cmdShell.Path,
		ExitCode:        exitCode,
		TimedOut:        timedOut,
		DurationMS:      dur.Milliseconds(),
		Stdout:          safeUTF8(stdoutW.Bytes()),
		Stderr:          safeUTF8(stderrW.Bytes()),
		StdoutTruncated: stdoutW.Truncated(),
		StderrTruncated: stderrW.Truncated(),
	}, nil
}

func RunOneShellCommand(
	parent context.Context,
	sel SelectedShell,
	command string,
	workdir string,
	env []string,
	timeout time.Duration,
	maxOut int64,
) (ShellCommandExecResult, error) {
	ctx := parent
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
		defer cancel()
	}

	shellArgs := deriveExecArgs(sel, command)

	// Determine execution strategy: host-spawn (Flatpak) or direct.
	execArgs := shellArgs
	execEnv := env
	useHostSpawn := HostSpawnAvailable(ctx)
	if useHostSpawn {
		execArgs = buildHostSpawnArgs(shellArgs, filterEnvForHostSpawn(env), workdir)
		execEnv = nil // env is forwarded via --env= flags to the host command
	}

	stdoutW := newCappedWriter(maxOut)
	stderrW := newCappedWriter(maxOut)
	cmd, state, prepErr := prepareManagedCommand(ctx, execArgs, workdir, execEnv, stdoutW, stderrW)
	if prepErr != nil {
		return ShellCommandExecResult{}, prepErr
	}

	start := time.Now()
	runErr := cmd.Start()

	// Fallback: if host-spawn itself failed to start, retry direct execution.
	if runErr != nil && useHostSpawn {
		logutil.WarnContext(ctx, "executil: host-spawn start failed; retrying with direct execution", "err", runErr)

		stdoutW = newCappedWriter(maxOut)
		stderrW = newCappedWriter(maxOut)

		cmd, state, prepErr = prepareManagedCommand(ctx, shellArgs, workdir, env, stdoutW, stderrW)
		if prepErr != nil {
			return ShellCommandExecResult{}, prepErr
		}

		start = time.Now()
		runErr = cmd.Start()
	}

	if runErr != nil {
		return ShellCommandExecResult{}, runErr
	}

	state.afterStart(ctx, cmd)
	defer state.close()

	waitErr := cmd.Wait()
	dur := time.Since(start)

	timedOut := state.wasCancelled() && errors.Is(ctx.Err(), context.DeadlineExceeded)

	exitCode := exitCodeFromWait(waitErr, timedOut)

	return ShellCommandExecResult{
		Command:   command,
		WorkDir:   workdir,
		Shell:     sel.Name,
		ShellPath: sel.Path,

		ExitCode:   exitCode,
		TimedOut:   timedOut,
		DurationMS: dur.Milliseconds(),

		Stdout: safeUTF8(stdoutW.Bytes()),
		Stderr: safeUTF8(stderrW.Bytes()),

		StdoutTruncated: stdoutW.Truncated(),
		StderrTruncated: stderrW.Truncated(),
	}, nil
}

// RunManagedCombinedOutput is like exec.Cmd.CombinedOutput, but uses the same
// process-tree isolation, hidden-window behavior, cancellation, and WaitDelay
// semantics as RunOneShellCommand.
func RunManagedCombinedOutput(ctx context.Context, args []string, workdir string, env []string) ([]byte, error) {
	var out lockedBuffer
	cmd, state, err := prepareManagedCommand(ctx, args, workdir, env, &out, &out)
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	state.afterStart(ctx, cmd)
	defer state.close()

	err = cmd.Wait()
	return out.Bytes(), err
}

type managedCommandState struct {
	mu        sync.Mutex
	pg        *processGroupHandle
	cancelled bool
}

func prepareManagedCommand(
	ctx context.Context,
	args []string,
	workdir string,
	env []string,
	stdout io.Writer,
	stderr io.Writer,
) (*exec.Cmd, *managedCommandState, error) {
	if len(args) == 0 {
		return nil, nil, errors.New("empty command args")
	}

	//nolint:gosec // The caller intentionally constructs the shell/process invocation.
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	configureProcessGroup(cmd)

	state := &managedCommandState{}

	// Important: override CommandContext's default Cancel.
	// The default kills only the immediate shell process. On Windows this can
	// orphan grandchildren such as go.exe/node.exe, and those grandchildren can
	// keep inherited stdout/stderr pipe handles open, making Wait hang.
	cmd.Cancel = func() error {
		return state.cancel(cmd)
	}

	// Bound Wait if descendants keep inherited pipe handles open.
	cmd.WaitDelay = managedCommandWaitDelay

	return cmd, state, nil
}

func (s *managedCommandState) afterStart(ctx context.Context, cmd *exec.Cmd) {
	pg, err := afterProcessStart(cmd)
	if err != nil {
		logutil.WarnContext(
			ctx,
			"executil: process-tree isolation setup failed; using fallback cancellation",
			"err",
			err,
		)
	}
	if pg == nil {
		return
	}

	s.mu.Lock()
	if s.pg != nil {
		s.mu.Unlock()
		pg.close()
		return
	}
	s.pg = pg
	alreadyCancelled := s.cancelled
	s.mu.Unlock()

	if alreadyCancelled {
		_ = s.cancel(cmd)
	}
}

func (s *managedCommandState) cancel(cmd *exec.Cmd) error {
	s.mu.Lock()
	s.cancelled = true
	pg := s.pg
	s.mu.Unlock()
	if pg != nil {
		return pg.terminate(cmd)
	}
	killProcessGroup(cmd)
	return nil
}

func (s *managedCommandState) close() {
	s.mu.Lock()
	pg := s.pg
	s.pg = nil
	s.mu.Unlock()

	if pg != nil {
		pg.close()
	}
}

func (s *managedCommandState) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

func exitCodeFromWait(waitErr error, timedOut bool) int {
	if timedOut {
		return 124 // conventional timeout exit code
	}
	if waitErr == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return exitCodeFromProcessState(ee.ProcessState)
	}

	return 127 // Spawn/other failure
}

func safeUTF8(b []byte) string {
	// Replace invalid UTF-8 sequences; avoids breaking JSON / UIs.
	return string(bytes.ToValidUTF8(b, []byte("\uFFFD")))
}

func deriveExecArgs(sel SelectedShell, command string) []string {
	switch sel.Name {
	case ShellNameBash, ShellNameZsh, ShellNameSh, ShellNameDash, ShellNameKsh, ShellNameFish:
		return []string{sel.Path, "-c", command}

	case ShellNamePowershell, ShellNamePwsh:
		// Always deterministic by default: no profile; non-interactive to avoid prompts.
		args := []string{sel.Path, "-NoLogo", "-NonInteractive", "-NoProfile"}
		if runtime.GOOS == toolutil.GOOSWindows {
			args = append(args, "-ExecutionPolicy", "Bypass")
		}
		args = append(args, "-Command", command)
		return args

	case ShellNameCmd:
		// Options: /d disables AutoRun from registry (safer); /s handles quotes; /c runs then exits.
		return []string{sel.Path, "/d", "/s", "/v:off", "/c", command}

	default:
		return []string{sel.Path, "-c", command}
	}
}
