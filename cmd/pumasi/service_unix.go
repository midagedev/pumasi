//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// startLoop runs the agent as a detached loop that starts it again whenever it
// ends: what is left where no service manager can keep it (it does not
// survive a reboot).
func startLoop(p hostPaths, exe string) error {
	logf, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	// The agent ends with 0 only when it was told to leave.
	cmd := exec.Command("sh", "-c", `until "$0" agent -join; do sleep 2; done`, exe)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(p.pid, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		return err
	}
	return cmd.Process.Release()
}

const loopHow = "a background process that restarts itself"

// loopPid is the restart loop's pid, if the pid file still names one.
func loopPid(p hostPaths) int {
	b, err := os.ReadFile(p.pid)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 {
		return 0
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil { // no /proc (macOS)
		cmdline, _ = exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	}
	if !strings.Contains(string(cmdline), "agent -join") {
		return 0
	}
	return pid
}

// stopLoop ends the restart loop and its agent, and forgets the pid file.
func stopLoop(p hostPaths, kill bool) {
	if pid := loopPid(p); pid != 0 && kill {
		// The loop leads its own session: the group is the loop and its agent.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		for i := 0; i < 50 && syscall.Kill(-pid, 0) == nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = os.Remove(p.pid)
}

// removeFiles deletes pumasi's directory and, when this is the installed
// binary, that too. It says what it did.
func removeFiles(p hostPaths) string {
	_ = os.RemoveAll(p.dir)
	if runningInstalled() {
		removeBinary()
		return p.dir + " and the binary removed"
	}
	return p.dir + " removed"
}

func removeBinary() { _ = os.Remove(installedBin()) }

// replaceSelf puts the build at tmp in place of exe and becomes it: the same
// pid, so whatever keeps the agent running does not notice.
func replaceSelf(exe, tmp string) error {
	if err := os.Rename(tmp, exe); err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

func superviseAgent(hostPaths) error {
	return errors.New("-supervise is how the agent is kept running on Windows; here its service does that")
}

// diskFree is the free bytes of the filesystem holding dir (0 = unknown).
func diskFree(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
