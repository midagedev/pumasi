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
	if cl.ScaleSet() != "pumasi-linux-s" || cl.OS != "linux" || cl.Driver != "docker" {
		t.Errorf("class defaults %+v", cl)
	}
	if s.Name != "server1" || s.Kind() != "server" || s.Shell != "sh -s" || c.Listen != DefaultListen {
		t.Errorf("server defaults %+v", s)
	}
	if d.Kind() != "desktop" || d.Shell != "wsl -e sh -s" || d.Budget.Memory != 9<<30 {
		t.Errorf("desktop %+v", d)
	}
	if !strings.HasSuffix(c.Ledger, "/.local/state/pumasi/pumasi.db") || strings.HasPrefix(c.Ledger, "~") ||
		c.EventLog != filepath.Join(c.StateDir(), "events.jsonl") {
		t.Errorf("state paths %s %s", c.Ledger, c.EventLog)
	}
}

// A config without a number in it is a whole config: the class starts from
// the default first estimate and the host lends what it measures free.
func TestNoNumbersNeeded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pumasi.yaml")
	os.WriteFile(p, []byte("url: u\nclasses: [{name: a}, {name: b, limit: {memory: 8G}}]\nhosts: [{ssh: h}]\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	a, b := c.Classes[0], c.Classes[1]
	if a.CPUs != DefaultCPUs || a.Memory != DefaultMemory || a.Limit != (Size{}) || b.Limit.Memory != 8<<30 {
		t.Errorf("classes %+v %+v", a, b)
	}
	if c.Hosts[0].Budget != (Size{}) {
		t.Errorf("budget %+v", c.Hosts[0].Budget)
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
	  {name: b, cpus: 1, memory: 1G, driver: native, hosts: [h], mounts: [/srv/cache, "/srv/keys:ro", /srv/cache]},
	  {name: d, cpus: 1, memory: 1G, docker: inside}]`)
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
		"[{name: a, cpus: 1, memory: 1G, driver: vm}]":                     "docker or native",
		"[{name: a, cpus: 1, memory: 1G, os: macos, driver: docker}]":      "processes only",
		"[{name: a, cpus: 1, memory: 1G, mounts: [cache]}]":                "absolute path",
		`[{name: a, cpus: 1, memory: 1G, mounts: ["/srv/a:/srv/b"]}]`:      "absolute path",
		`[{name: a, cpus: 1, memory: 1G, mounts: ["/srv/a,readonly"]}]`:    "absolute path",
		"[{name: a, cpus: 1, memory: 1G, driver: native, hosts: [no pe]}]": "bad host name",
		"[{name: a, cpus: 1, memory: 1G, docker: host}]":                   "must be inside",
		"[{name: a, cpus: 1, memory: 1G, driver: native, docker: inside}]": "container runners",
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

// A misspelt key is an error, not a setting that silently does not apply.
func TestUnknownKeysAndDuplicateClasses(t *testing.T) {
	load := func(body string) error {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte(body), 0o600)
		_, err := Load(p)
		return err
	}
	const hosts = "\nhosts: [{ssh: h, budget: {cpus: 1, memory: 1G}}]\n"
	for body, want := range map[string]string{
		"url: u\nclasses: [{name: a, cpus: 1, memory: 1G, maxduration: 1h}]" + hosts:                                           "maxduration",
		"url: u\nclasses: [{name: a, cpus: 1, memory: 1G}]\nhosts: [{ssh: h, budget: {cpu: 1}}]\n":                             "cpu",
		"url: u\nclasses: [{name: a, cpus: 1, memory: 1G}]\nhosts: [{ssh: h, kind: desktop, budget: {cpus: 1, memory: 1G}}]\n": "kind",
		"url: u\nidleTimeout: 1m\nclasses: [{name: a, cpus: 1, memory: 1G}]" + hosts:                                           "idleTimeout",
		"url: u\nclasses: [{name: a, cpus: 1, memory: 1G}, {name: a, cpus: 2, memory: 2G}]" + hosts:                            "duplicate class a",
	} {
		if err := load(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want one naming %q", body, err, want)
		}
	}
	if err := load("url: u\nclasses: [{name: a, cpus: 1, memory: 1G, maxDuration: 1h}]" + hosts); err != nil {
		t.Errorf("a correct file: %v", err)
	}
}

func TestExampleLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "pumasi.example.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestClassLabels(t *testing.T) {
	if got := strings.Join((Class{Name: "small"}).RunsOn(), ","); got != "self-hosted,pumasi-small" {
		t.Fatalf("default labels %s", got)
	}
	cl := Class{Name: "shared", Labels: []string{"self-hosted", "Linux", "X64", "ci-pool"}}
	if got := strings.Join(cl.RunsOn(), ","); got != "self-hosted,Linux,X64,ci-pool" || cl.ScaleSet() != "pumasi-shared" {
		t.Fatalf("labels %s, scale set %s", got, cl.ScaleSet())
	}
}

func TestClassEnvAndPath(t *testing.T) {
	load := func(class string) error {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		if err := os.WriteFile(p, []byte("url: u\nclasses: ["+class+"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		return err
	}
	if err := load(`{name: a, cpus: 1, memory: 1G, driver: native, env: {JAVA_HOME: /opt/java, HOME: "{runner}"}, path: [/opt/java/bin]}`); err != nil {
		t.Fatal(err)
	}
	if err := load(`{name: a, cpus: 1, memory: 1G, env: {LANG: C}}`); err != nil {
		t.Fatalf("env on a container class: %v", err)
	}
	for _, bad := range []string{
		`{name: a, cpus: 1, memory: 1G, path: [/x]}`,                       // a container has its image's PATH
		`{name: a, cpus: 1, memory: 1G, env: {"A B": x}}`,                  // not a variable name
		`{name: a, cpus: 1, memory: 1G, driver: native, env: {A: "x\ny"}}`, // one line
	} {
		if err := load(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
