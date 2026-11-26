package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// On a Mac the agent is a LaunchAgent of the logged-in user: it starts with the
// login, and the runners it starts can use that user's keychain and open
// windows — what signing and browser jobs need.
const launchLabel = "dev.pumasi.agent"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error { return exec.Command("launchctl", args...).Run() }

// guiSession reports whether this user is logged in at the screen: only then
// does launchd have a place for the user's agents.
func guiSession() bool { return launchctl("print", guiDomain()) == nil }

// launchAgent is the agent's job: started again unless it ended with 0 (it
// was told to leave).
func launchAgent(exe, log, path string) string {
	return launchPlist(launchLabel, []string{exe, "agent", "-join"}, false, 2, "Interactive", "", log, path)
}

// launchPlist is a LaunchAgent that launchd keeps running: always, or unless
// it ended with 0.
func launchPlist(label string, argv []string, always bool, throttle int, procType, dir, log, path string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var args strings.Builder
	for _, a := range argv {
		args.WriteString("<string>" + esc(a) + "</string>")
	}
	keep := "<dict><key>SuccessfulExit</key><false/></dict>"
	if always {
		keep = "<true/>"
	}
	wd := ""
	if dir != "" {
		wd = "\n\t<key>WorkingDirectory</key><string>" + esc(dir) + "</string>"
	}
	// launchd starts jobs with a bare PATH; jobs need the tools this user installed.
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>%s</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key>%s
	<key>ThrottleInterval</key><integer>%d</integer>
	<key>ProcessType</key><string>%s</string>%s
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
	<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
</dict>
</plist>
`, label, args.String(), keep, throttle, procType, wd, esc(log), esc(log), esc(path))
}

// installService (re)starts the agent so that it stays up: as a LaunchAgent
// when the user is logged in at the screen, otherwise as a detached restart loop.
func installService(p hostPaths, exe string) (how, note string, err error) {
	stopService(p, true)
	if !guiSession() {
		if err := startLoop(p, exe); err != nil {
			return "", "", err
		}
		return loopHow,
			"note: this user is not logged in at the Mac's screen, so the agent is not a LaunchAgent: it does not come back\n" +
				"after a reboot, and jobs cannot use the login keychain or open windows. Log in at the screen and run the\n" +
				"invite line again to change that.", nil
	}
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(plistPath(), []byte(launchAgent(exe, p.log, os.Getenv("PATH"))), 0o644); err != nil {
		return "", "", err
	}
	if out, err := exec.Command("launchctl", "bootstrap", guiDomain(), plistPath()).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("launchctl bootstrap %s: %w: %s", guiDomain(), err, lastLineOf(strings.TrimSpace(string(out))))
	}
	return "the LaunchAgent " + launchLabel, "", nil
}

func serviceRunning(p hostPaths) (bool, string) {
	if _, err := os.Stat(plistPath()); err == nil {
		out, _ := exec.Command("launchctl", "print", guiDomain()+"/"+launchLabel).Output()
		return strings.Contains(string(out), "state = running"), "the LaunchAgent " + launchLabel
	}
	return loopPid(p) != 0, loopHow
}

// stopService stops the agent however it runs and removes the LaunchAgent.
// With kill false the caller is the agent itself and ends on its own: launchd
// does not start it again after exit 0, and forgets the job a moment later.
func stopService(p hostPaths, kill bool) {
	target := guiDomain() + "/" + launchLabel
	_, statErr := os.Stat(plistPath())
	if statErr == nil || launchctl("print", target) == nil {
		_ = os.Remove(plistPath())
		if kill {
			_ = launchctl("bootout", target)
		} else {
			// Its own session, or launchd takes it down together with the agent.
			cmd := exec.Command("sh", "-c", `sleep 2; launchctl bootout "$0"`, target)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if cmd.Start() == nil {
				_ = cmd.Process.Release()
			}
		}
	}
	stopLoop(p, kill)
}

// preflightHost checks what runners need here. They are processes of this
// user, so there is no docker or cgroup to ask for.
func preflightHost(ctx context.Context, say func(bool, string, ...any) bool, note func(string, ...any)) {
	preflightNative(ctx, paths(), say)
	if guiSession() {
		say(true, "login session: this user is logged in at the screen — the agent becomes a LaunchAgent, jobs can sign and open windows")
	} else {
		note("login session: this user is not logged in at the Mac's screen — jobs cannot use the login keychain or open windows, and the agent will not come back after a reboot")
	}
}
