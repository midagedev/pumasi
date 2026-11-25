package controller

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach gives a child its own process group: a terminal Ctrl-C must not
// reach it before the controller is done with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// lockFile takes an exclusive lock that goes away with the process, or fails at once.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
}
