package exectool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/executil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	testCommandEchoHi           = "echo hi"
	testCommandEchoA            = "echo a"
	testCommandEchoB            = "echo b"
	testCommandEchoOk           = "echo ok"
	testCommandEchoShouldNotRun = "echo should_not_run"
	testCommandEchoShouldFail   = "echo should_fail"
	testCommandExit7            = "exit 7"
	testCommandPrintfHelloErr   = `printf '%s' hello; printf '%s' err_msg 1>&2`
	testCommandPrintfFOO        = `printf '%s' "$FOO"`
	testCommandSleep2           = `sleep 2`
	testCommandEcho12345        = "echo_12345"
	testCommandRMFoo            = `rm foo`
	testCommandCurlExample      = `curl https://example.com`
	testCommandRMRoot           = `rm -rf /`
	testCommandNUL              = "echo hi\x00there"
	testCommandRMNUL            = "rm\x00"
	testCommandEmpty            = ""
	testCommandSpaces           = "  "
	testCommandNewline          = "\n"
	testCommandWhitespaceOnly   = " \n\t "
	testCommandEchoBSpaced      = " echo b "
	testCommandPwd              = "pwd"
	testCommandPwdAndPrintfFOO  = `pwd; printf '%s' "$FOO"`
	testStdoutHello             = "hello"
	testStdoutBar               = "bar"
	testStdoutBaz               = "baz"
	testStderrErrMsg            = "err_msg"
	testEnvKeyFOO               = "FOO"
	testBlockedEchoCommand      = "echo"
)

func TestShellCommand_AutoSession_DoesNotLeakOnError(t *testing.T) {
	t.Helper()

	td := t.TempDir()
	nonexistent := filepath.Join(td, "does-not-exist")
	outside := t.TempDir()

	cases := []struct {
		name          string
		opts          []ExecToolOption
		args          ShellCommandArgs
		needsShell    bool
		wantErrSubstr string
	}{
		{
			name:          "early_error_missing_commands",
			args:          ShellCommandArgs{Commands: nil},
			wantErrSubstr: "commands is required",
		},
		{
			name:          "workdir_does_not_exist",
			args:          ShellCommandArgs{Commands: []string{testCommandEchoHi}, WorkDir: nonexistent},
			wantErrSubstr: "stat dir error",
		},
		{
			name:          "invalid_env_map",
			args:          ShellCommandArgs{Commands: []string{testCommandEchoHi}, Env: map[string]string{"": "1"}},
			wantErrSubstr: "env",
		},
		{
			name:          "invalid_shell_name",
			args:          ShellCommandArgs{Commands: []string{testCommandEchoHi}, Shell: ShellName("nope")},
			wantErrSubstr: "invalid shell",
		},
		{
			name:          "command_contains_nul",
			args:          ShellCommandArgs{Commands: []string{testCommandNUL}},
			needsShell:    true, // NUL check happens after selectShell()
			wantErrSubstr: "nul",
		},
		{
			name:          "workdir_outside_allowed_roots",
			opts:          []ExecToolOption{WithAllowedRoots([]string{td})},
			args:          ShellCommandArgs{Commands: []string{testCommandEchoHi}, WorkDir: outside},
			wantErrSubstr: "outside allowed roots",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestShellTool(t, tc.opts...)

			if tc.needsShell {
				requireAnyShell(t)
			}

			_, err := st.ShellCommand(t.Context(), tc.args)
			if err == nil {
				t.Fatalf("expected error")
			}
			if tc.wantErrSubstr != "" &&
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}

			if got := st.sessions.Size(); got != 0 {
				t.Fatalf("expected no sessions left, found %d", got)
			}
		})
	}
}

func TestNormalizeBlockedCommand(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		want       string
		wantErrSub string
	}{
		{name: "empty_is_ok", in: testCommandEmpty, want: testCommandEmpty},
		{name: "whitespace_only_is_ok", in: testCommandWhitespaceOnly, want: testCommandEmpty},
		{name: "lowercases_and_trims", in: " RM ", want: "rm"},
		{name: "basenames_slash", in: "/bin/rm", want: "rm"},
		{name: "basenames_backslash", in: `C:\Windows\System32\CURL.EXE`, want: "curl.exe"},
		{name: "trims_trailing_separators", in: "/usr/bin/rm////", want: "rm"},
		{name: "rejects_nul", in: testCommandRMNUL, wantErrSub: "NUL"},
		{name: "rejects_whitespace_in_name", in: "rm -rf", wantErrSub: "whitespace"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := executil.NormalizeBlockedCommand(tc.in)
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSub)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizedCommandList(t *testing.T) {
	args := ShellCommandArgs{
		Commands: []string{
			testCommandEmpty,
			testCommandSpaces,
			testCommandNewline,
			testCommandEchoA,
			testCommandEchoBSpaced,
		},
	}
	got := normalizedCommandList(args)
	if len(got) != 2 {
		t.Fatalf("expected 2 commands, got %d: %#v", len(got), got)
	}
	if got[0] != testCommandEchoA || got[1] != testCommandEchoBSpaced {
		t.Fatalf("unexpected command list: %#v", got)
	}
}

func TestPolicy_EffectiveTimeout_UsesDefaultAndClampsToHardMax(t *testing.T) {
	p := ExecutionPolicy{}
	if got := effectiveTimeout(p); got != defaultExecutionTimeout {
		t.Fatalf("expected DefaultTimeout=%v got %v", defaultExecutionTimeout, got)
	}

	p.Timeout = 999 * time.Hour
	if got := effectiveTimeout(p); got != hardExecutionTimeout {
		t.Fatalf("expected clamp to HardMaxTimeout=%v got %v", hardExecutionTimeout, got)
	}
}

func TestPolicy_EffectiveMaxOutputBytes_UsesDefaultAndClamps(t *testing.T) {
	p := ExecutionPolicy{}
	if got := effectiveMaxOutputBytes(p); got != defaultExecutionMaxOutputBytes {
		t.Fatalf("expected DefaultMaxOutputBytes=%d got %d", defaultExecutionMaxOutputBytes, got)
	}

	p.MaxOutputBytes = 1
	if got := effectiveMaxOutputBytes(p); got != minExecutionOutputBytes {
		t.Fatalf("expected clamp to MinOutputBytes=%d got %d", minExecutionOutputBytes, got)
	}

	p.MaxOutputBytes = 1 << 62
	if got := effectiveMaxOutputBytes(p); got != min(hardExecutionMaxOutputBytes, int64(^uint(0)>>1)) {
		// The implementation clamps to HardMaxOutputBytes and also to MaxInt.
		t.Fatalf("expected clamp to hard max, got %d", got)
	}
}

func TestSelectShell_ResolveAndAuto(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific expectations")
	}

	shPath := mustLookPath(t, "sh")

	// Explicit resolution.
	sel, err := selectShell(t.Context(), ShellNameSh)
	if err != nil {
		t.Fatalf("selectShell(sh) error: %v", err)
	}
	if sel.Name != ShellNameSh {
		t.Fatalf("expected name=sh, got %q", sel.Name)
	}
	if sel.Path == "" {
		t.Fatalf("expected path for sh")
	}

	// Auto via $SHELL should pick sh when basename is sh.
	t.Setenv("SHELL", shPath)
	sel, err = selectShell(t.Context(), ShellNameAuto)
	if err != nil {
		t.Fatalf("selectShell(auto) error: %v", err)
	}
	if sel.Path == "" {
		t.Fatalf("expected auto-selected path")
	}

	// Invalid.
	_, err = selectShell(t.Context(), ShellName("nope"))
	if err == nil || !strings.Contains(err.Error(), "invalid shell") {
		t.Fatalf("expected invalid shell error, got: %v", err)
	}
}

func TestShellCommand_Run_CapturesStdoutStderr_ExitCode(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix command expectations")
	}
	st := newTestShellTool(t)

	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		Commands: []string{testCommandPrintfHelloErr},
	})
	if err != nil {
		t.Fatalf("ShellCommand error: %v", err)
	}
	resp := out
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if r.ExitCode != 0 {
		t.Fatalf("expected exitCode=0, got %d (stderr=%q)", r.ExitCode, r.Stderr)
	}
	if r.Stdout != testStdoutHello {
		t.Fatalf("expected stdout=%s, got %q", testStdoutHello, r.Stdout)
	}
	if r.Stderr != testStderrErrMsg {
		t.Fatalf("expected stderr=%s, got %q", testStderrErrMsg, r.Stderr)
	}
	if strings.TrimSpace(r.ShellPath) == "" {
		t.Fatalf("expected shellPath set")
	}
}

func TestShellCommand_ExitCode_NonZeroAndSignaled(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific exit/signal expectations")
	}
	st := newTestShellTool(t)

	// Exit with explicit code.
	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		Commands: []string{testCommandExit7},
	})
	if err != nil {
		t.Fatalf("ShellCommand error: %v", err)
	}
	resp := out
	if resp.Results[0].ExitCode != 7 {
		t.Fatalf("expected exitCode=7, got %d", resp.Results[0].ExitCode)
	}

	// Signal self with SIGKILL; expect 128+9=137 per unix convention in exitCodeFromProcessState.
	out, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		Commands: []string{`kill -9 $$`},
	})
	if err != nil {
		t.Fatalf("ShellCommand error: %v", err)
	}
	resp = out
	if resp.Results[0].ExitCode != 137 {
		t.Fatalf("expected exitCode=137, got %d (stderr=%q)", resp.Results[0].ExitCode, resp.Results[0].Stderr)
	}
}

func TestShellCommand_Timeout_SetsTimedOutAnd124(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific sleep/timeout expectations")
	}
	p := DefaultExecutionPolicy()
	p.Timeout = 150 * time.Millisecond
	st := newTestShellTool(t, WithExecutionPolicy(p))

	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:     ShellNameSh,
		Commands:  []string{testCommandSleep2},
		SessionID: testCommandEmpty,
		WorkDir:   testCommandEmpty,
		Env:       nil,
	})
	if err != nil {
		t.Fatalf("ShellCommand error: %v", err)
	}
	resp := out
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	r := resp.Results[0]
	if !r.TimedOut {
		t.Fatalf("expected TimedOut=true, got false (exitCode=%d, stderr=%q)", r.ExitCode, r.Stderr)
	}
	if r.ExitCode != 124 {
		t.Fatalf("expected exitCode=124 on timeout, got %d", r.ExitCode)
	}
}

func TestShellCommand_MaxOutput_Truncates(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific sh loop expectations")
	}
	p := DefaultExecutionPolicy()
	p.MaxOutputBytes = 1024
	st := newTestShellTool(t, WithExecutionPolicy(p))

	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell: ShellNameSh,
		Commands: []string{
			// Print 3000 'a' characters using POSIX sh arithmetic.
			`i=0; while [ $i -lt 3000 ]; do printf a; i=$((i+1)); done`,
		},
	})
	if err != nil {
		t.Fatalf("ShellCommand error: %v", err)
	}
	resp := out
	r := resp.Results[0]

	if !r.StdoutTruncated {
		t.Fatalf("expected stdout truncated")
	}
	if got := int64(len(r.Stdout)); got != 1024 {
		t.Fatalf("expected captured stdout len=1024, got %d", got)
	}
}

func TestShellCommand_ExecuteParallelly_False_StopsOnFirstError(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific")
	}
	st := newTestShellTool(t)

	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell: ShellNameSh,
		Commands: []string{
			testCommandExit7,
			testCommandEchoShouldNotRun,
		},
		ExecuteParallel: false,
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	resp := out
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result due to stop-on-error, got %d", len(resp.Results))
	}
	if resp.Results[0].ExitCode != 7 {
		t.Fatalf("expected exitCode=7 got %d", resp.Results[0].ExitCode)
	}
}

func TestShellCommand_ExecuteParallelly_True_RunsAllCommandsEvenIfError(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific")
	}
	st := newTestShellTool(t)

	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell: ShellNameSh,
		Commands: []string{
			testCommandExit7,
			testCommandEchoOk,
		},
		ExecuteParallel: true,
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	resp := out
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].ExitCode != 7 {
		t.Fatalf("expected first exitCode=7 got %d", resp.Results[0].ExitCode)
	}
	if strings.TrimSpace(resp.Results[1].Stdout) != "ok" {
		t.Fatalf("expected second stdout=ok, got %q", resp.Results[1].Stdout)
	}
}

func TestShellCommand_RejectsNULInCommand(t *testing.T) {
	st := newTestShellTool(t)
	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandNUL},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "nul") {
		t.Fatalf("expected NUL error, got %v", err)
	}
}

func TestShellCommand_DangerousRejected_BeforeExec(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific dangerous patterns")
	}
	st := newTestShellTool(t)

	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		Commands: []string{testCommandRMRoot},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("expected blocked error, got: %v", err)
	}
}

func TestShellCommand_MaxCommands_PolicyLimit(t *testing.T) {
	st := newTestShellTool(t, WithExecutionPolicy(ExecutionPolicy{
		MaxCommands: 1,
	}))
	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandEchoA, testCommandEchoB},
	})
	if err == nil || !strings.Contains(err.Error(), "too many commands") {
		t.Fatalf("expected too many commands error, got %v", err)
	}
}

func TestShellCommand_MaxCommandLength_PolicyLimit(t *testing.T) {
	st := newTestShellTool(t, WithExecutionPolicy(ExecutionPolicy{
		MaxCommandLength: 5,
	}))
	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandEcho12345},
	})
	if err == nil || !strings.Contains(err.Error(), "command too long") {
		t.Fatalf("expected command too long error, got %v", err)
	}
}

func TestSessions_LRU_MaxSessions_EvictsOldest(t *testing.T) {
	st := newTestShellTool(t, WithMaxSessions(1))

	out1, err := st.ShellCommand(t.Context(), ShellCommandArgs{Commands: []string{testCommandEchoA}})
	if err != nil {
		t.Fatalf("Run1 error: %v", err)
	}
	sid1 := out1.SessionID

	_, err = st.ShellCommand(t.Context(), ShellCommandArgs{Commands: []string{testCommandEchoB}})
	if err != nil {
		t.Fatalf("Run2 error: %v", err)
	}

	_, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		SessionID: sid1,
		Commands:  []string{testCommandEchoShouldFail},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown sessionID") {
		t.Fatalf("expected unknown sessionID after LRU eviction, got %v", err)
	}
}

func TestShellCommand_Session_PersistsWorkdirAndEnv_UpdateRestartClose(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific")
	}
	st := newTestShellTool(t)

	td := t.TempDir()

	// 1) Create session, set workdir and env, and run "pwd".
	out, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		WorkDir:  td,
		Env:      map[string]string{testEnvKeyFOO: testStdoutBar},
		Commands: []string{testCommandPwd},
	})
	if err != nil {
		t.Fatalf("ShellCommand(auto session) error: %v", err)
	}
	resp := out

	if resp.SessionID == "" {
		t.Fatalf("expected sessionID returned")
	}
	mustSameDir(t, td, resp.WorkDir)
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	r0 := resp.Results[0]
	mustSameDir(t, td, strings.TrimSpace(r0.Stdout))

	sid := resp.SessionID

	// 2) Verify env persists without passing Env.
	out, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		SessionID: sid,
		Shell:     ShellNameSh,
		Commands:  []string{testCommandPrintfFOO},
	})
	if err != nil {
		t.Fatalf("ShellCommand(session reuse) error: %v", err)
	}
	resp = out
	if resp.Results[0].Stdout != testStdoutBar {
		t.Fatalf("expected FOO=bar, got %q (stderr=%q)", resp.Results[0].Stdout, resp.Results[0].Stderr)
	}
	mustSameDir(t, td, resp.WorkDir)

	// 3) Update session env by providing Env; should persist for subsequent calls.
	out, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		SessionID: sid,
		Shell:     ShellNameSh,
		Env:       map[string]string{testEnvKeyFOO: testStdoutBaz},
		Commands:  []string{testCommandPrintfFOO},
	})
	if err != nil {
		t.Fatalf("ShellCommand(session update env) error: %v", err)
	}
	resp = out
	if resp.Results[0].Stdout != testStdoutBaz {
		t.Fatalf("expected FOO=baz, got %q", resp.Results[0].Stdout)
	}

	out, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		SessionID: sid,
		Shell:     ShellNameSh,
		Commands:  []string{testCommandPrintfFOO},
	})
	if err != nil {
		t.Fatalf("ShellCommand(session verify env persisted) error: %v", err)
	}
	resp = out
	if resp.Results[0].Stdout != testStdoutBaz {
		t.Fatalf("expected FOO=baz persisted, got %q", resp.Results[0].Stdout)
	}

	// 4) Start a NEW session by omitting sessionID; should not inherit prior session's workdir/env.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	out, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		Shell:    ShellNameSh,
		Commands: []string{testCommandPwdAndPrintfFOO},
	})
	if err != nil {
		t.Fatalf("ShellCommand(new session) error: %v", err)
	}
	resp = out
	mustSameDir(t, cwd, resp.WorkDir)

	// After restart, FOO should be empty (unless inherited from process env; to avoid flake,
	// assert only that it is not "baz").
	if strings.Contains(resp.Results[0].Stdout, testStdoutBaz) {
		t.Fatalf("expected new session not to have baz, got stdout=%q", resp.Results[0].Stdout)
	}
}

func TestShellCommand_ContextCanceledEarly(t *testing.T) {
	st := newTestShellTool(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := st.ShellCommand(ctx, ShellCommandArgs{
		Commands: []string{testCommandEchoHi},
	})
	if err == nil {
		t.Fatalf("expected context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestShellCommand_Blocklist_DefaultBlocksRMAndCurl(t *testing.T) {
	st := newTestShellTool(t)

	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandRMFoo},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("expected rm to be blocked, got %v", err)
	}

	_, err = st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandCurlExample},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("expected curl to be blocked, got %v", err)
	}
}

func TestShellCommand_Blocklist_NotOverridableByAllowDangerous(t *testing.T) {
	p := DefaultExecutionPolicy()
	p.AllowDangerous = true
	st := newTestShellTool(t, WithExecutionPolicy(p))

	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandRMFoo},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("expected rm to be blocked even with AllowDangerous=true, got %v", err)
	}
}

func TestShellCommand_Blocklist_AdditionalBlocks(t *testing.T) {
	st := newTestShellTool(t, WithBlockedCommands([]string{testBlockedEchoCommand}))

	_, err := st.ShellCommand(t.Context(), ShellCommandArgs{
		Commands: []string{testCommandEchoHi},
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("expected echo to be blocked via additional blocklist, got %v", err)
	}
}

func newTestShellTool(t *testing.T, opts ...ExecToolOption) *ExecTool {
	t.Helper()

	st, err := NewExecTool(opts...)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	return st
}

func requireAnyShell(t *testing.T) {
	t.Helper()
	if _, err := selectShell(t.Context(), ShellNameAuto); err != nil {
		t.Skipf("no suitable shell found on PATH: %v", err)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil || strings.TrimSpace(p) == "" {
		t.Skipf("missing dependency on PATH: %s (%v)", name, err)
	}
	return p
}

func mustSameDir(t *testing.T, a, b string) {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat(%q): %v", a, err)
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat(%q): %v", b, err)
	}
	if !os.SameFile(sa, sb) {
		t.Fatalf("expected same dir:\n  a=%q\n  b=%q", a, b)
	}
}
