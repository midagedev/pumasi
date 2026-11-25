package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinimalConfigDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pumasi.yaml")
	os.WriteFile(p, []byte(`
url: https://github.com/o/r
classes:
  - {name: linux-s, cpus: 1, memory: 1G}
hosts:
  - {ssh: server1, budget: {cpus: 4, memory: 6G}}
  - {ssh: desk1, desktop: true, shell: wsl -e sh -s, budget: {cpus: 6, memory: 9G}}
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cl, s, d := c.Classes[0], c.Hosts[0], c.Hosts[1]
	if cl.ScaleSet != "pumasi-linux-s" || strings.Join(cl.Labels, ",") != "pumasi-linux-s,self-hosted" || cl.MaxRunners != 10 {
		t.Errorf("class defaults %+v", cl)
	}
	if s.Name != "server1" || s.Kind != "server" || s.Shell != "sh -s" || s.AgentPort != 7391 {
		t.Errorf("server defaults %+v", s)
	}
	if d.Kind != "desktop" || d.Shell != "wsl -e sh -s" || d.Budget.Memory != 9<<30 {
		t.Errorf("desktop %+v", d)
	}
	if !strings.HasSuffix(c.Ledger, "/.local/state/pumasi/pumasi.db") || strings.HasPrefix(c.Ledger, "~") ||
		c.EventLog != filepath.Join(c.StateDir(), "events.jsonl") {
		t.Errorf("state paths %s %s", c.Ledger, c.EventLog)
	}
}

func TestBudgetRequired(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pumasi.yaml")
	os.WriteFile(p, []byte("url: u\nclasses: [{name: a, cpus: 1, memory: 1G}]\nhosts: [{ssh: h}]\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Errorf("err %v", err)
	}
}

func TestClassOS(t *testing.T) {
	load := func(classes string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: "+classes+"\nhosts: [{ssh: h, budget: {cpus: 1, memory: 1G}}]\n"), 0o600)
		return Load(p)
	}
	c, err := load("[{name: a, cpus: 1, memory: 1G}, {name: m, cpus: 1, memory: 1G, os: macos}]")
	if err != nil || c.Classes[0].OS != "linux" || c.Classes[1].OS != "macos" {
		t.Errorf("os %+v err %v", c, err)
	}
	if _, err := load("[{name: a, cpus: 1, memory: 1G, os: darwin}]"); err == nil || !strings.Contains(err.Error(), "macos") {
		t.Errorf("err %v", err)
	}
}

func TestClassDriverAndMounts(t *testing.T) {
	load := func(classes string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: "+classes+"\nhosts: [{ssh: h, budget: {cpus: 1, memory: 1G}}]\n"), 0o600)
		return Load(p)
	}
	c, err := load(`[{name: a, cpus: 1, memory: 1G}, {name: m, cpus: 1, memory: 1G, os: macos}, {name: w, cpus: 1, memory: 1G, os: windows},
	  {name: b, cpus: 1, memory: 1G, driver: native, hosts: [h], mounts: [/srv/cache, "/srv/keys:ro", /srv/cache]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{c.Classes[0].Driver, c.Classes[1].Driver, c.Classes[2].Driver, c.Classes[3].Driver}; strings.Join(got, ",") != "docker,native,native,native" {
		t.Errorf("drivers %v", got)
	}
	if got := strings.Join(c.MountPaths(), ","); got != "/srv/cache,/srv/keys" {
		t.Errorf("mount paths %s", got)
	}
	for classes, want := range map[string]string{
		"[{name: a, cpus: 1, memory: 1G, driver: vm}]":                    "docker or native",
		"[{name: a, cpus: 1, memory: 1G, os: macos, driver: docker}]":     "processes only",
		"[{name: a, cpus: 1, memory: 1G, mounts: [cache]}]":               "absolute path",
		`[{name: a, cpus: 1, memory: 1G, mounts: ["/srv/a:/srv/b"]}]`:     "absolute path",
		`[{name: a, cpus: 1, memory: 1G, mounts: ["/srv/a,readonly"]}]`:   "absolute path",
		"[{name: a, cpus: 1, memory: 1G, driver: native, hosts: [nope]}]": "unknown host",
	} {
		if _, err := load(classes); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", classes, err, want)
		}
	}
}

func TestWorkDirSitsInTheFirstWritableMount(t *testing.T) {
	for _, c := range []struct {
		cl   Class
		want string
	}{
		{Class{Driver: "docker", Mounts: []string{"/srv/keys:ro", "/srv/cache", "/srv/other"}}, "/srv/cache/.pumasi-work/r1"},
		{Class{Driver: "docker", Mounts: []string{"/srv/keys:ro"}}, ""},
		{Class{Driver: "docker"}, ""},
		{Class{Driver: "native", Mounts: []string{"/srv/cache"}}, ""},
	} {
		if got := c.cl.WorkDir("r1"); got != c.want {
			t.Errorf("%+v: %q, want %q", c.cl, got, c.want)
		}
	}
}
