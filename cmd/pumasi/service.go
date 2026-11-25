package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const unitName = "pumasi.service"

func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", unitName)
}

func systemctl(args ...string) error {
	return exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
}

// lingering reports whether this user's systemd units run without a login
// session. Without it a user unit dies when the ssh session that started it
// ends, which is worse than no unit.
func lingering() bool {
	if systemctl("show-environment") != nil {
		return false
	}
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := exec.Command("loginctl", "show-user", u.Username, "-p", "Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// installService (re)starts the agent so that it stays up: as a systemd user
// unit where those outlive the login, otherwise as a detached restart loop.
// It returns what it chose and, for the loop, what that does not cover.
func installService(p hostPaths, exe string) (how, note string, err error) {
	stopService(p, true)
	if lingering() {
		unit := fmt.Sprintf(`[Unit]
Description=pumasi agent (lends this host's spare capacity to a pumasi controller)

[Service]
ExecStart=%s agent -join
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, exe)
		if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(unitPath(), []byte(unit), 0o644); err != nil {
			return "", "", err
		}
		_ = systemctl("daemon-reload")
		if err := systemctl("enable", "--now", unitName); err != nil {
			return "", "", fmt.Errorf("systemctl --user enable --now %s: %w", unitName, err)
		}
		return "the systemd user unit " + unitName, "", nil
	}
	logf, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", "", err
	}
	defer logf.Close()
	// The agent ends with 0 only when it was told to leave.
	cmd := exec.Command("sh", "-c", `until "$0" agent -join; do sleep 2; done`, exe)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(p.pid, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		return "", "", err
	}
	_ = cmd.Process.Release()
	return "a background process that restarts itself",
		"note: it does not come back after a reboot. This user's systemd units do not outlive the login here;\n" +
			"after `loginctl enable-linger`, running the invite line again makes it a service.", nil
}

// loopPid is the restart loop's pid, if the pid file still names one.
func loopPid(p hostPaths) int {
	b, err := os.ReadFile(p.pid)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if pid <= 1 || !strings.Contains(string(cmdline), "agent -join") {
		return 0
	}
	return pid
}

func serviceRunning(p hostPaths) (bool, string) {
	if _, err := os.Stat(unitPath()); err == nil {
		return systemctl("is-active", "--quiet", unitName) == nil, "the systemd user unit " + unitName
	}
	return loopPid(p) != 0, "a background process that restarts itself"
}

// stopService stops the agent however it runs and removes the unit. With
// kill false the caller is the agent itself and ends on its own.
func stopService(p hostPaths, kill bool) {
	if _, err := os.Stat(unitPath()); err == nil {
		if kill {
			_ = systemctl("disable", "--now", unitName)
		} else {
			_ = systemctl("disable", unitName)
		}
		_ = os.Remove(unitPath())
		_ = systemctl("daemon-reload")
	}
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

// pumasiContainers lists the containers labelled pumasi: all of them, or only those that are up.
func pumasiContainers(ctx context.Context, all bool) []string {
	args := []string{"ps", "--filter", "label=pumasi", "--format", "{{.Names}}"}
	if all {
		args = append(args, "-a")
	}
	out, _ := exec.CommandContext(ctx, "docker", args...).Output()
	return strings.Fields(string(out))
}

func removeContainers(ctx context.Context) (int, error) {
	names := pumasiContainers(ctx, true)
	if len(names) == 0 {
		return 0, nil
	}
	if out, err := exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, names...)...).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("docker rm: %w: %s", err, lastLineOf(strings.TrimSpace(string(out))))
	}
	return len(names), nil
}

// removeFiles deletes pumasi's directory and, when it is the installed one, the binary.
func removeFiles(p hostPaths) {
	_ = os.RemoveAll(p.dir)
	if exe, err := os.Executable(); err == nil && strings.HasSuffix(exe, "/.local/bin/pumasi") {
		_ = os.Remove(exe)
	}
}
