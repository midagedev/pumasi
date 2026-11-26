package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/midagedev/pumasi/internal/agent"
)

const unitName = "pumasi.service"

func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", unitName)
}

func systemctl(args ...string) error {
	return userSystemctl(args...).Run()
}

// userSystemctl is `systemctl --user`. A shell that is not a login session
// (ssh into WSL, a command run through another program) has no
// XDG_RUNTIME_DIR, and without it systemctl does not find the user's systemd
// although it runs: a join there would start a second agent beside the unit,
// a leave would not stop the unit.
func userSystemctl(args ...string) *exec.Cmd {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		dir := "/run/user/" + strconv.Itoa(os.Getuid())
		if _, err := os.Stat(filepath.Join(dir, "systemd", "private")); err == nil {
			cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir)
		}
	}
	return cmd
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
	if err := startLoop(p, exe); err != nil {
		return "", "", err
	}
	return loopHow,
		"note: it does not come back after a reboot. This user's systemd units do not outlive the login here;\n" +
			"after `loginctl enable-linger`, running the invite line again makes it a service.", nil
}

func serviceRunning(p hostPaths) (bool, string) {
	if _, err := os.Stat(unitPath()); err == nil {
		return systemctl("is-active", "--quiet", unitName) == nil, "the systemd user unit " + unitName
	}
	return loopPid(p) != 0, loopHow
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
		_ = systemctl("reset-failed", unitName) // stopping the agent by signal counts as a failure there
	}
	stopLoop(p, kill)
}

// preflightHost checks what runners need here: docker and cgroup v2. Whether
// a class with driver: native can use this host is said too, as a note: the
// host does not know yet which classes it will be given.
func preflightHost(ctx context.Context, say func(bool, string, ...any) bool, note func(string, ...any)) {
	defer func() {
		if p := agent.NativeProblem(); p != "" {
			note("driver: native classes will not be placed here: %s", p)
		} else {
			say(true, "driver: native classes can run here (systemd user session, linger, the runner's libraries)")
		}
	}()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	text := strings.TrimSpace(string(out))
	switch {
	case errors.Is(err, exec.ErrNotFound):
		say(false, "docker: not installed (no docker in PATH) — runners are Docker containers")
	case err != nil && strings.Contains(text, "permission denied"):
		say(false, "docker: this user may not use it — add the user to the docker group and log in again")
	case err != nil:
		say(false, "docker: the daemon does not answer (%s)", lastLineOf(text))
	default:
		say(true, "docker %s reachable as this user", text)
	}

	ctrl, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	have := strings.Fields(string(ctrl))
	switch {
	case err != nil:
		say(false, "cgroup v2: /sys/fs/cgroup is not a cgroup v2 mount — pumasi measures and limits runners through cgroup v2")
	case !slices.Contains(have, "memory") || !slices.Contains(have, "cpu"):
		say(false, "cgroup v2: the memory and cpu controllers are not both enabled (have: %s)", strings.Join(have, " "))
	default:
		say(true, "cgroup v2 with memory and cpu controllers")
	}
}
