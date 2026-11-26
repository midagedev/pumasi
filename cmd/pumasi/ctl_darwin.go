package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// On a Mac the controller is a LaunchAgent like the agent: launchd starts it
// at login and again whenever it ends.
const ctlLabel = "dev.pumasi.controller"

func ctlPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", ctlLabel+".plist")
}

func (s ctlService) can() error {
	if !guiSession() {
		return errors.New("this user is not logged in at the Mac's screen: the service is a LaunchAgent, and launchd has a place for those only while the user is logged in. Log in at the screen (automatic login makes it come back after a reboot) and run this again")
	}
	return nil
}

func (s ctlService) start() (how string, notes []string, err error) {
	if err := os.MkdirAll(filepath.Dir(ctlPlistPath()), 0o755); err != nil {
		return "", nil, err
	}
	text := launchPlist(ctlLabel, []string{s.exe, "up", "-config", s.conf}, true, 5, "Interactive", filepath.Dir(s.conf), s.log, os.Getenv("PATH"))
	if err := os.WriteFile(ctlPlistPath(), []byte(text), 0o644); err != nil {
		return "", nil, err
	}
	if out, err := exec.Command("launchctl", "bootstrap", guiDomain(), ctlPlistPath()).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("launchctl bootstrap %s: %w: %s", guiDomain(), err, lastLineOf(strings.TrimSpace(string(out))))
	}
	notes = append(notes, "a LaunchAgent runs while this user is logged in: after a reboot the controller is back when the user logs in (automatic login makes that the boot).")
	if len(s.cfg.Hosts) > 0 {
		notes = append(notes, "hosts of the config (ssh:) are reached with the keys this user's ssh finds without a terminal to ask in.")
	}
	return "the LaunchAgent " + ctlLabel, notes, nil
}

func (s ctlService) stop() error {
	target := guiDomain() + "/" + ctlLabel
	if launchctl("print", target) == nil {
		if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil && launchctl("print", target) == nil {
			return fmt.Errorf("launchctl bootout %s: %w: %s", target, err, lastLineOf(strings.TrimSpace(string(out))))
		}
		// bootout returns before the controller has ended: it is told to stop
		// and tidies up first.
		for end := time.Now().Add(30 * time.Second); launchctl("print", target) == nil; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(end) {
				return fmt.Errorf("the controller has not ended 30s after launchctl bootout %s", target)
			}
		}
	}
	_ = os.Remove(ctlPlistPath())
	return nil
}

func (s ctlService) logHow() string { return s.log }

var launchField = regexp.MustCompile(`(?m)^\s*(state|pid|runs|last exit code) = (.*)$`)

func (s ctlService) status() error {
	if _, err := os.Stat(ctlPlistPath()); err != nil {
		fmt.Println("not installed (`pumasi service install`)")
		return nil
	}
	out, err := exec.Command("launchctl", "print", guiDomain()+"/"+ctlLabel).Output()
	if err != nil {
		fmt.Printf("%s: installed, but launchd does not know it (this user is not logged in at the screen?)\n", ctlLabel)
		return nil
	}
	p := map[string]string{}
	for _, m := range launchField.FindAllStringSubmatch(string(out), -1) {
		if _, dup := p[m[1]]; !dup {
			p[m[1]] = m[2]
		}
	}
	fmt.Printf("%s: %s", ctlLabel, p["state"])
	if p["pid"] != "" {
		fmt.Printf(", pid %s", p["pid"])
	}
	fmt.Printf(", started %s times (last exit: %s)\n", p["runs"], p["last exit code"])
	if b, err := os.ReadFile(ctlPlistPath()); err == nil && !strings.Contains(string(b), s.conf) {
		fmt.Printf("  it runs another config than %s (see %s)\n", s.conf, ctlPlistPath())
	}
	fmt.Println("  runs while this user is logged in; log: " + s.log)
	return nil
}
