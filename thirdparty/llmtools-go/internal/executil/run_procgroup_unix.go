//go:build !windows

package executil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type processGroupHandle struct{}

func afterProcessStart(cmd *exec.Cmd) (*processGroupHandle, error) {
	return &processGroupHandle{}, nil
}

func (h *processGroupHandle) terminate(cmd *exec.Cmd) error {
	killProcessGroup(cmd)
	return nil
}

func (h *processGroupHandle) close() {
}

func configureProcessGroup(cmd *exec.Cmd) {
	// Use Setsid (new session) rather than Setpgid (new process group only).
	//
	// Both approaches make PGID == child PID so kill(-child_pid, sig) reaches
	// the entire process tree.  Setsid additionally detaches from any
	// controlling terminal.
	//
	// Why this matters: with Setpgid the child remains in the parent's session.
	// If the parent has a controlling terminal the child is a background process
	// group; interactive shells (zsh/bash invoked with -i during bootstrap to
	// source .zshrc/.bashrc) call tcsetpgrp() to manage job control.  The kernel
	// delivers SIGTTOU to a background process that attempts tcsetpgrp(), which
	// stops the shell by default.  The bootstrap timeout then fires, SIGKILL is
	// sent, and RunManagedCombinedOutput returns "signal: killed".
	//
	// With Setsid the new session has no controlling terminal; tcgetpgrp()
	// returns ENOTTY so the shell disables job control and runs normally.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative PID targets the process group.

	pgid := -cmd.Process.Pid

	// Best-effort graceful shutdown first.
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	// Short grace period, then SIGKILL if still alive.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		// Signal 0: check existence.
		err := syscall.Kill(pgid, 0)
		if err == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// ESRCH => no such process group => already gone.
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		// Any other error: break and try SIGKILL.
		break
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

func exitCodeFromProcessState(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	// On Unix, ps.Sys() is syscall.WaitStatus.
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			// Conventional: 128 + signal.
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	// Fallback.
	return ps.ExitCode()
}
