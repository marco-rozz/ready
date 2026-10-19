package executil

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	runShellPathUnix = "/bin/sh"
	runEchoHi        = "echo hi"
	runAbcdBytes     = "abcd"
)

func TestUnixSpecific_ProcessGroupAndExitCodeHelpers(t *testing.T) {
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("unix-specific")
	}

	// "configureProcessGroup" should populate SysProcAttr (Setsid=true on Unix).
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	configureProcessGroup(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatalf("expected SysProcAttr set")
	}

	// "exitCodeFromProcessState" should reflect normal exit and signaled exit.
	c := exec.CommandContext(t.Context(), "sh", "-c", "exit 9")
	if err := c.Run(); err == nil {
		t.Fatalf("expected non-zero exit")
	}
	if got := exitCodeFromProcessState(c.ProcessState); got != 9 {
		t.Fatalf("expected exit code 9, got %d", got)
	}

	c = exec.CommandContext(t.Context(), "sh", "-c", "kill -9 $$")
	_ = c.Run() // expect error
	if got := exitCodeFromProcessState(c.ProcessState); got != 137 {
		t.Fatalf("expected signaled exit code 137, got %d", got)
	}

	// "killProcessGroup" should be safe on nils.
	killProcessGroup(nil)
	killProcessGroup(&exec.Cmd{})
}

func TestDeriveExecArgs(t *testing.T) {
	wantPowerShellArgs := []string{"pwsh", "-NoLogo", "-NonInteractive", "-NoProfile"}
	if runtime.GOOS == toolutil.GOOSWindows {
		wantPowerShellArgs = append(wantPowerShellArgs,
			"-ExecutionPolicy",
			"Bypass",
		)
	}
	wantPowerShellArgs = append(wantPowerShellArgs,
		"-Command",
		"Write-Output hi",
	)

	cases := []struct {
		name string
		sel  SelectedShell
		cmd  string
		want []string
	}{
		{
			name: "sh_like_uses_dash_c",
			sel:  SelectedShell{Name: ShellNameSh, Path: runShellPathUnix},
			cmd:  runEchoHi,
			want: []string{runShellPathUnix, "-c", runEchoHi},
		},
		{
			name: "powershell_uses_no_profile_non_interactive",
			sel:  SelectedShell{Name: ShellNamePwsh, Path: "pwsh"},
			cmd:  "Write-Output hi",
			want: wantPowerShellArgs,
		},
		{
			name: "cmd_uses_d_s_c",
			sel:  SelectedShell{Name: ShellNameCmd, Path: string(ShellNameCmd)},
			cmd:  runEchoHi,
			want: []string{string(ShellNameCmd), "/d", "/s", "/v:off", "/c", runEchoHi},
		},
		{
			name: "unknown_defaults_to_dash_c",
			sel:  SelectedShell{Name: ShellName("weird"), Path: "weirdsh"},
			cmd:  runEchoHi,
			want: []string{"weirdsh", "-c", runEchoHi},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveExecArgs(tc.sel, tc.cmd)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestExitCodeFromWait(t *testing.T) {
	cases := []struct {
		name     string
		waitErr  error
		timedOut bool
		want     int
	}{
		{name: "nil_err_exit0", waitErr: nil, timedOut: false, want: 0},
		{name: "timeout_non_nil_err_124", waitErr: errors.New("killed"), timedOut: true, want: 124},
		{name: "other_error_127", waitErr: errors.New("spawn failed"), timedOut: false, want: 127},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFromWait(tc.waitErr, tc.timedOut); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestCappedWriter_RingWrapAndOverwrite(t *testing.T) {
	// Use a small custom writer (not via newCappedWriter) to test ring behavior precisely.
	cases := []struct {
		name           string
		cap            int
		writes         []string
		wantBytes      string
		wantTruncated  bool
		wantTotalBytes int64
	}{
		{
			name:           "no_overflow_no_trunc",
			cap:            5,
			writes:         []string{"ab", "cd"},
			wantBytes:      runAbcdBytes,
			wantTruncated:  false,
			wantTotalBytes: 4,
		},
		{
			name:           "overflow_advances_start",
			cap:            5,
			writes:         []string{"abcd", "ef"},
			wantBytes:      "bcdef",
			wantTruncated:  true,
			wantTotalBytes: 6,
		},
		{
			name:           "single_write_larger_than_cap_keeps_tail",
			cap:            5,
			writes:         []string{"0123456789"},
			wantBytes:      "56789",
			wantTruncated:  true,
			wantTotalBytes: 10,
		},
		{
			name:           "wrap_copy_correct",
			cap:            5,
			writes:         []string{"abcd", "e", "f"},
			wantBytes:      "bcdef",
			wantTruncated:  true,
			wantTotalBytes: 6,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &cappedWriter{capBytes: tc.cap, buf: make([]byte, tc.cap)}
			for _, s := range tc.writes {
				_, _ = w.Write([]byte(s))
			}
			got := string(w.Bytes())
			if got != tc.wantBytes {
				t.Fatalf("bytes got %q want %q", got, tc.wantBytes)
			}
			if w.Truncated() != tc.wantTruncated {
				t.Fatalf("truncated got %v want %v", w.Truncated(), tc.wantTruncated)
			}
			if w.TotalBytes() != tc.wantTotalBytes {
				t.Fatalf("total got %d want %d", w.TotalBytes(), tc.wantTotalBytes)
			}
		})
	}
}

func TestCappedWriter_ConcurrentWrites_NoPanicsAndCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency stress in -short")
	}

	cases := []struct {
		name    string
		g       int
		writes  int
		payload string
	}{
		{name: "many_writers", g: 16, writes: 200, payload: strings.Repeat("x", 37)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &cappedWriter{capBytes: 1024, buf: make([]byte, 1024)}
			var wg sync.WaitGroup
			wg.Add(tc.g)
			for i := 0; i < tc.g; i++ {
				go func() {
					defer wg.Done()
					for j := 0; j < tc.writes; j++ {
						_, _ = w.Write([]byte(tc.payload))
					}
				}()
			}
			wg.Wait()

			wantTotal := int64(tc.g * tc.writes * len(tc.payload))
			if w.TotalBytes() != wantTotal {
				t.Fatalf("TotalBytes got %d want %d", w.TotalBytes(), wantTotal)
			}
			// Should never exceed cap.
			if len(w.Bytes()) > 1024 {
				t.Fatalf("Bytes len got %d want <= 1024", len(w.Bytes()))
			}
		})
	}
}

func TestRunOneShellCommand_ContextCancellationBeforeTimeout_NotMarkedTimedOut(t *testing.T) {
	// This is intentionally light-touch and should work cross-platform if a shell exists.
	// If no shell exists, skip.
	cases := []struct {
		name string
		sel  SelectedShell
		cmd  string
	}{
		{
			name: "auto_sh_like",
			sel: func() SelectedShell {
				if runtime.GOOS == toolutil.GOOSWindows {
					return SelectedShell{Name: ShellNameCmd, Path: "cmd"}
				}
				return SelectedShell{Name: ShellNameSh, Path: "sh"}
			}(),
			cmd: func() string {
				if runtime.GOOS == toolutil.GOOSWindows {
					return "ping -n 3 127.0.0.1 >NUL"
				}
				return "sleep 2"
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			// Even if process doesn't start, RunOneShellCommand may error; accept either:
			// - error returned
			// - result returned with TimedOut=false (if it somehow ran/ended quickly).
			res, err := RunOneShellCommand(ctx, tc.sel, tc.cmd, ".", nil, 5*time.Second, 1024)
			if err == nil {
				if res.TimedOut {
					t.Fatalf("expected TimedOut=false on immediate cancel")
				}
			}
		})
	}
}
