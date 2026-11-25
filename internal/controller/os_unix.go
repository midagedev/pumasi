//go:build !windows

package controller

import (
	"os"
	"os/exec"
	"syscall"
)

// detach gives a child its own process group: a terminal Ctrl-C must not
// reach it before the controller is done with it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// lockFile takes an exclusive lock that goes away with the process, or fails at once.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
