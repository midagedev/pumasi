package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const ctlUnit = "pumasi-controller.service"

func ctlUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", ctlUnit)
}

// ctlUnitText: Restart=always, because `up` ends with 0 on a signal and a
// controller that was stopped by anything but `service remove` should be back.
func ctlUnitText(exe, conf string) string {
	return fmt.Sprintf(`[Unit]
Description=pumasi controller (places GitHub Actions runners on this user's hosts)

[Service]
ExecStart=%s up -config %s
WorkingDirectory=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, systemdQuote(exe), systemdQuote(conf), strings.ReplaceAll(filepath.Dir(conf), "%", "%%")) // WorkingDirectory= is the rest of its line
}

// systemdQuote makes a path one word of a unit's command line.
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\%$") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

func (s ctlService) can() error {
	if systemctl("show-environment") != nil {
		return errors.New("this user's systemd does not answer (`systemctl --user`): the service is a systemd user unit. Without systemd, run `pumasi up` under whatever keeps programs running here")
	}
	return nil
}

func (s ctlService) start() (how string, notes []string, err error) {
	if err := os.MkdirAll(filepath.Dir(ctlUnitPath()), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(ctlUnitPath(), []byte(ctlUnitText(s.exe, s.conf)), 0o644); err != nil {
		return "", nil, err
	}
	_ = systemctl("daemon-reload")
	if err := systemctl("enable", "--now", ctlUnit); err != nil {
		return "", nil, fmt.Errorf("systemctl --user enable --now %s: %w", ctlUnit, err)
	}
	if !lingering() {
		name := "$USER"
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
		notes = append(notes, fmt.Sprintf("this user's services stop when the user logs out and do not start at boot. `loginctl enable-linger %s` (once) makes the controller outlive the login and come back after a reboot.", name))
	}
	if len(s.cfg.Hosts) > 0 {
		notes = append(notes, "the service has no ssh agent: hosts of the config (ssh:) need a key it can use without one, or join them with `pumasi invite`.")
	}
	return "the systemd user unit " + ctlUnit, notes, nil
}

func (s ctlService) stop() error {
	if _, err := os.Stat(ctlUnitPath()); err != nil {
		return nil
	}
	if err := systemctl("disable", "--now", ctlUnit); err != nil {
		return fmt.Errorf("systemctl --user disable --now %s: %w", ctlUnit, err)
	}
	_ = os.Remove(ctlUnitPath())
	_ = systemctl("daemon-reload")
	_ = systemctl("reset-failed", ctlUnit)
	return nil
}

func (s ctlService) logHow() string { return "journalctl --user -u " + ctlUnit }

func (s ctlService) status() error {
	if _, err := os.Stat(ctlUnitPath()); err != nil {
		fmt.Println("not installed (`pumasi service install`)")
		return nil
	}
	out, _ := userSystemctl("show", ctlUnit, "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "NRestarts", "-p", "ActiveEnterTimestamp", "-p", "ExecStart").Output()
	p := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			p[k] = v
		}
	}
	fmt.Printf("%s: %s (%s)", ctlUnit, p["ActiveState"], p["SubState"])
	if p["ActiveState"] == "active" {
		since := p["ActiveEnterTimestamp"]
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", since); err == nil {
			since = t.Local().Format("01-02 15:04:05")
		}
		fmt.Printf(", pid %s, since %s", p["MainPID"], since)
	}
	fmt.Printf(", started again %s times\n", cmpOr(p["NRestarts"], "0"))
	if !strings.Contains(p["ExecStart"], s.conf) {
		fmt.Printf("  it runs another config than %s:\n  %s\n", s.conf, p["ExecStart"])
	}
	if lingering() {
		fmt.Println("  outlives the login and starts at boot (linger is on)")
	} else {
		fmt.Println("  stops at logout and does not start at boot: `loginctl enable-linger` changes that")
	}
	tail, _ := exec.Command("journalctl", "--user", "-u", ctlUnit, "-n", "5", "--no-pager", "-o", "cat").Output()
	if t := strings.TrimSpace(string(tail)); t != "" {
		fmt.Println("  last lines (" + s.logHow() + "):\n    " + strings.ReplaceAll(t, "\n", "\n    "))
	}
	return nil
}
