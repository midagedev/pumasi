package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/config"
)

// fakeSSH puts an `ssh` first in PATH that records its arguments and the
// script it was given, prints out and exits with code.
func fakeSSH(t *testing.T, out string, code int) (args, script func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" > '" + dir + "/args'\ncat > '" + dir + "/script'\ncat '" + dir + "/out'\nexit " + fmt.Sprint(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "out"), []byte(out), 0o600)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	read := func(name string) func() string {
		return func() string { b, _ := os.ReadFile(filepath.Join(dir, name)); return string(b) }
	}
	return read("args"), read("script")
}

func configHost() config.Host {
	h := config.Host{Name: "server1", SSH: "srv", Shell: "sh -s", Desktop: true}
	h.Budget.CPUs, h.Budget.Memory = 4, 6<<30
	return h
}

// A host of the config joins by the controller running the invite line there:
// the config's name, budget and kind, and the token on stdin only.
func TestBootstrapRunsTheInviteLineOverSSH(t *testing.T) {
	c, _, _ := joinController(t)
	c.listenAddr = "10.0.0.5:7411"
	args, script := fakeSSH(t, "joined as server1", 0)
	if err := c.bootstrap(context.Background(), configHost()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(args()); !strings.HasSuffix(got, " srv sh -s") || strings.Contains(got, "PUMASI_TOKEN") {
		t.Fatalf("ssh arguments: %s", got)
	}
	s := script()
	for _, want := range []string{"export PUMASI_TOKEN='", "for a in 10.0.0.5:7411; do", `"https://$a/join.sh"`,
		"sh -s -- --name server1 --cpus 4 --mem 6442450944 --desktop\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in the script:\n%s", want, s)
		}
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(s)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script is not valid sh: %v\n%s", err, out)
	}
}

// What went wrong on the host is said in the host's words.
func TestBootstrapNamesWhyAHostDidNotJoin(t *testing.T) {
	c, _, _ := joinController(t)
	c.listenAddr = "10.0.0.5:7411"
	for _, row := range []struct {
		out  string
		code int
		want string
	}{
		{"curl: (28) Connection timed out after 5002 milliseconds", exitUnreachable,
			"server1 cannot reach this controller at 10.0.0.5:7411 (curl: (28) Connection timed out after 5002 milliseconds): hosts connect to the controller"},
		{"ok    resolve 10.0.0.5\nFAIL  docker: not installed (no docker in PATH) — runners are Docker containers\npumasi: not joined: fix the FAIL lines", 1,
			"server1 did not join: docker: not installed"},
		{"sh: curl: not found", 127, "server1 did not join: ssh srv: exit status 127: sh: curl: not found"},
	} {
		fakeSSH(t, row.out, row.code)
		if err := c.bootstrap(context.Background(), configHost()); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("exit %d: %v\nwant it to contain %q", row.code, err, row.want)
		}
	}
}

// A host of the config is on the list before it has joined, gets its
// connection when it does, and stays when it leaves. A host that joined by
// itself comes and goes with its membership.
func TestHostsOfTheConfigAndHostsThatJoined(t *testing.T) {
	c, addr, _ := joinController(t)
	c.hosts = []*host{newHost(c.cfg.Hosts[0])}
	c.syncJoined(true)
	if len(c.hosts) != 1 || c.hosts[0].connected() || !c.needsBootstrap(c.hosts[0]) {
		t.Fatalf("before anyone joined: %+v", c.hosts)
	}
	if _, err := c.hosts[0].agent.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), errNotJoined.Error()) {
		t.Fatalf("snapshot of a host that has not joined: %v", err)
	}
	joinedHost(t, c, addr, "desk1")
	c.syncJoined(false)
	if len(c.hosts) != 2 || c.hosts[1].cfg.Name != "desk1" || c.hosts[1].wait(context.Background()) != nil {
		t.Fatalf("after desk1 joined: %d hosts", len(c.hosts))
	}
	c.join.Forget(c.hosts[1].id)
	c.syncJoined(true)
	if len(c.hosts) != 1 || c.hosts[0].cfg.Name != "server1" {
		t.Fatalf("after desk1 left: %d hosts", len(c.hosts))
	}
}

// down reaches a connected host through its connection, and a host of the
// config that is not connected over ssh.
func TestDownLeavesOverSSHWhenAHostIsNotConnected(t *testing.T) {
	c, _, _ := joinController(t)
	c.gh = &fakeGitHub{}
	c.hosts = []*host{newHost(configHost())}
	_, script := fakeSSH(t, "left: agent stopped, 2 runners removed", 0)
	if err := c.down(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if s := script(); !strings.Contains(s, `exec "$b" leave --force`) {
		t.Fatalf("script:\n%s", s)
	}
	if err := c.down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if s := script(); strings.Contains(s, "--force") || !strings.Contains(s, `exec "$b" leave`) {
		t.Fatalf("without force the host decides:\n%s", s)
	}
}
