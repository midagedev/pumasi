package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// On Windows the agent starts when its user logs on (HKCU Run), not as a
// service: a service runs in session 0, where runners have no desktop. What
// the Run key starts is `pumasi agent -join -supervise`: a hidden process
// that starts the agent and starts it again whenever it ends.
const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "pumasi"
	runHow   = "a process that starts when this user logs on (HKCU Run value \"pumasi\")"
	// The agent's exit code after it put another build in its place.
	exitSwitched = 75
	detachedEnv  = "PUMASI_DETACHED"
)

func runLine(exe string) string { return `"` + exe + `" agent -join -supervise` }

func installService(p hostPaths, exe string) (how, note string, err error) {
	stopService(p, true)
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return "", "", fmt.Errorf("registry %s: %w", runKey, err)
	}
	err = k.SetStringValue(runValue, runLine(exe))
	k.Close()
	if err != nil {
		return "", "", fmt.Errorf("registry %s: %w", runKey, err)
	}
	if err := detach(exe); err != nil {
		return "", "", err
	}
	if !interactive() {
		note = "note: this is not a logon session on the machine's desktop (ssh or a service): the agent started now may end with\n" +
			"this session and its jobs have no desktop. It starts properly the next time this user logs on."
	}
	return runHow, note, nil
}

// detach starts the supervisor without a console, in no one's process group.
func detach(exe string) error {
	cmd := exec.Command(exe, "agent", "-join", "-supervise")
	cmd.Env = append(os.Environ(), detachedEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// superviseAgent keeps the agent running until it ends with 0 (it left). At
// logon it is started with a console window of its own; then it starts itself
// again without one and gives the window back.
func superviseAgent(p hostPaths) error {
	exe := installedBin()
	if os.Getenv(detachedEnv) == "" {
		return detach(exe)
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	for {
		cmd := exec.Command(exe, "agent", "-join")
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(logf, "pumasi: cannot start the agent: %v\n", err)
		} else {
			// Both pids: stopping the supervisor alone would leave its agent.
			_ = os.WriteFile(p.pid, []byte(fmt.Sprintf("%d\n%d\n", os.Getpid(), cmd.Process.Pid)), 0o600)
			if cmd.Wait() == nil {
				return nil // it was told to leave
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// ours reports the pids of the pid file that still run a pumasi binary.
func ours(p hostPaths) []int {
	b, _ := os.ReadFile(p.pid)
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		pid, _ := strconv.Atoi(f)
		if pid > 0 && strings.EqualFold(filepath.Base(imageOf(pid)), filepath.Base(installedBin())) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// imageOf is the path of the program a pid runs ("" = no such process).
func imageOf(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func serviceRunning(p hostPaths) (bool, string) { return len(ours(p)) > 0, runHow }

// stopService removes the logon entry and, with kill, ends the supervisor and
// its agent (in that order, or the supervisor starts another agent). Runners
// are not ended with them: they live in Job Objects of their own.
func stopService(p hostPaths, kill bool) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(runValue)
		k.Close()
	}
	if kill {
		for _, pid := range ours(p) {
			if pid == os.Getpid() {
				continue
			}
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid)); err == nil {
				_ = windows.TerminateProcess(h, 1)
				_, _ = windows.WaitForSingleObject(h, 5000)
				windows.CloseHandle(h)
			}
		}
	}
	_ = os.Remove(p.pid)
}

// removeFiles: a running program and the files it holds open cannot be
// deleted on Windows, so a detached cmd does it once this process is gone.
func removeFiles(p hostPaths) string {
	_ = os.RemoveAll(p.dir) // whatever already can go
	bin := installedBin()
	script := fmt.Sprintf(`ping -n 4 127.0.0.1 >nul & rmdir /s /q "%s" & del /q "%s" "%s.old" "%s.new"`, p.dir, bin, bin, bin)
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: `cmd.exe /d /c ` + script,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("%s could not be removed (%v); delete it and %s by hand", p.dir, err, bin)
	}
	_ = cmd.Process.Release()
	return p.dir + " and the binary are removed in a few seconds"
}

func removeBinary() {} // the script moves the old one aside the next time; nothing can delete a running program

// replaceSelf puts the build at tmp in place of exe. A running program can be
// renamed but not replaced, and Windows has no exec: the agent ends and its
// supervisor starts the new build.
func replaceSelf(exe, tmp string) error {
	if os.Getenv(detachedEnv) == "" {
		return errors.New("this agent was not started by its supervisor (`pumasi agent -join -supervise`), nothing would start the new build")
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe)
		return err
	}
	os.Exit(exitSwitched)
	return nil
}

// interactive reports whether this process runs in a logon session with a
// desktop. Session 0 is where services and ssh sessions live.
func interactive() bool {
	var sid uint32
	return windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid) == nil && sid != 0
}

// diskFree is the free bytes of the volume holding dir (0 = unknown).
func diskFree(dir string) int64 {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var avail, all, totalFree uint64
	if windows.GetDiskFreeSpaceEx(p, &avail, &all, &totalFree) != nil {
		return 0
	}
	return int64(avail)
}

// preflightHost checks what runners need here. They are processes of this
// user in Job Objects, so there is no docker or cgroup to ask for.
func preflightHost(ctx context.Context, say func(bool, string, ...any) bool, note func(string, ...any)) {
	preflightNative(ctx, paths(), say)
	if interactive() {
		say(true, "logon session: on the machine's desktop — the agent starts with this user's logon, jobs can open windows")
	} else {
		note("logon session: this is session 0 (ssh or a service) — jobs have no desktop; run the invite line in a PowerShell window on the machine itself")
	}
}
