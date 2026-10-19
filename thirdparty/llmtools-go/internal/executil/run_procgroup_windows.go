//go:build windows

package executil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsCreateNoWindow = 0x08000000 // CREATE_NO_WINDOW

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// CREATE_NO_WINDOW + HideWindow prevents black console windows from Wails/GUI apps.
	//
	// CREATE_NEW_PROCESS_GROUP is kept as a fallback isolation primitive for
	// taskkill and for environments where Job Object assignment is unavailable.
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windowsCreateNoWindow
	cmd.SysProcAttr.HideWindow = true
}

type processGroupHandle struct {
	job windows.Handle
}

func afterProcessStart(cmd *exec.Cmd) (*processGroupHandle, error) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil, nil
	}

	job, err := createKillOnCloseJob()
	if err != nil {
		return nil, err
	}

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		if isBenignWindowsJobSetupError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer windows.CloseHandle(process)

	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		if isBenignWindowsJobSetupError(err) {
			return nil, nil
		}
		return nil, err
	}

	return &processGroupHandle{job: job}, nil
}

func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}

	return job, nil
}

func isBenignWindowsJobSetupError(err error) bool {
	// ERROR_INVALID_PARAMETER commonly means the short-lived process already
	// exited before we could attach it.
	//
	// ERROR_ACCESS_DENIED commonly means the process is already in a job that
	// does not allow assignment. In that case we fall back to taskkill.
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func (h *processGroupHandle) terminate(cmd *exec.Cmd) error {
	if h != nil && h.job != 0 {
		if err := windows.TerminateJobObject(h.job, 1); err == nil {
			return nil
		}
	}

	killProcessGroup(cmd)
	return nil
}

func (h *processGroupHandle) close() {
	if h == nil || h.job == 0 {
		return
	}
	_ = windows.CloseHandle(h.job)
	h.job = 0
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Best-effort: kill process tree. Taskkill is available on Windows.
	pid := strconv.Itoa(cmd.Process.Pid)
	runTaskkill := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "taskkill", args...)
		configureProcessGroup(c)
		c.WaitDelay = managedCommandWaitDelay
		return c.Run()
	}

	// Force tree-kill. This is fallback for when the Job Object is unavailable
	// or assignment failed.
	if err := runTaskkill("/T", "/F", "/PID", pid); err != nil {
		_ = cmd.Process.Kill()
	}
}

func exitCodeFromProcessState(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	// Go sets ExitCode() appropriately on Windows.
	return ps.ExitCode()
}
