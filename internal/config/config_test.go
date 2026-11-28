package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestClassWork(t *testing.T) {
	load := func(class string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		if err := os.WriteFile(p, []byte("url: u\nclasses: ["+class+"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(p)
	}
	for _, good := range []string{
		`{name: a, driver: native, work: "/srv/ci/w{slot}"}`,
		`{name: a, os: windows, work: 'C:\ci\w{slot}'}`,
	} {
		c, err := load(good)
		if err != nil {
			t.Fatalf("%s: %v", good, err)
		}
		if got := c.Classes[0].WorkDir("r1"); got != c.Classes[0].Work {
			t.Errorf("%s: WorkDir %q, want the class's work with {slot} still in it", good, got)
		}
	}
	for _, bad := range []string{
		`{name: a, work: "/srv/ci/w{slot}"}`,                      // a container keeps its work in its mount
		`{name: a, driver: native, work: "/srv/ci/w"}`,            // every runner in one place
		`{name: a, driver: native, work: "ci/w{slot}"}`,           // relative
		`{name: a, driver: native, work: "/srv/{runner}/{slot}"}`, // gone with the runner
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestPriorityRules(t *testing.T) {
	load := func(rules string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: [{name: a}]\npriority:\n"+rules), 0o600)
		return Load(p)
	}
	c, err := load(`  - {match: {workflow: "deploy-*.yml"}, level: high}
  - {match: {event: schedule}, level: low}
  - {match: {class: e2e, branch: "release/*"}, level: high}
  - {match: {job: "lint*"}, level: low}
`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		job   WaitingJob
		level int
		why   string
	}{
		{WaitingJob{Class: "a", Repo: "o/r", WorkflowRef: "o/r/.github/workflows/deploy-prod.yml@refs/heads/main", Name: "ship", Event: "push"}, 1, "high by rule 1 (workflow: deploy-*.yml)"},
		{WaitingJob{Class: "a", WorkflowRef: "o/r/.github/workflows/ci.yml@refs/heads/main", Name: "build", Event: "schedule"}, -1, "low by rule 2 (event: schedule)"},
		// The first rule that matches counts: a scheduled deploy is a deploy.
		{WaitingJob{Class: "a", WorkflowRef: "o/r/.github/workflows/deploy-x.yml@refs/heads/main", Event: "schedule"}, 1, "high by rule 1 (workflow: deploy-*.yml)"},
		{WaitingJob{Class: "e2e", WorkflowRef: "o/r/.github/workflows/e2e.yml@refs/heads/release/2026-04", Event: "push"}, 1, "high by rule 3 (class: e2e, branch: release/*)"},
		{WaitingJob{Class: "e2e", WorkflowRef: "o/r/.github/workflows/e2e.yml@refs/pull/7/merge", Event: "pull_request"}, 0, ""},
		{WaitingJob{Class: "a", WorkflowRef: "o/r/.github/workflows/ci.yml@refs/heads/x", Name: "lint (go)"}, -1, "low by rule 4 (job: lint*)"},
		{WaitingJob{Class: "a"}, 0, ""}, // a job known only from the count has no name to match
	}
	for _, tt := range tests {
		if level, why := c.LevelOf(tt.job); level != tt.level || why != tt.why {
			t.Errorf("%+v: %d %q, want %d %q", tt.job, level, why, tt.level, tt.why)
		}
	}
	if c.PriorityHeadStart != 30*time.Minute {
		t.Errorf("head start %s", c.PriorityHeadStart)
	}
	for rules, want := range map[string]string{
		"  - {match: {event: push}, level: urgent}\n": "high, normal or low",
		"  - {level: high}\n":                         "at least one",
		"  - {match: {job: \"[\"}, level: low}\n":     "bad pattern",
		"  - {match: {label: x}, level: low}\n":       "label",
	} {
		if _, err := load(rules); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", rules, err, want)
		}
	}
}

func TestClassOverflow(t *testing.T) {
	load := func(classes string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: "+classes+"\n"), 0o600)
		return Load(p)
	}
	c, err := load("[{name: linux, labels: [self-hosted, build], overflow: true}, {name: mac, os: macos, labels: [self-hosted, build], overflow: true, after: linux}, {name: all}]")
	if err != nil || !c.Classes[0].Overflow || c.Classes[1].After != "linux" || c.Classes[2].Overflow {
		t.Fatalf("%+v err %v", c, err)
	}
	for classes, want := range map[string]string{
		"[{name: a, overflow: true}, {name: b, after: a}]":                                                      "overflow: true",
		"[{name: a}, {name: b, overflow: true, after: a}]":                                                      "must have overflow: true too",
		"[{name: b, overflow: true, after: nobody}]":                                                            "names no other class",
		"[{name: b, overflow: true, after: b}]":                                                                 "names no other class",
		"[{name: a, overflow: true}, {name: b, overflow: true, after: a}, {name: c, overflow: true, after: b}]": "one step only",
	} {
		if _, err := load(classes); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", classes, err, want)
		}
	}
}

// A runtime rule goes before the file's; the line `pumasi priority` prints
// for it loads back as the same rule.
func TestRuntimeRules(t *testing.T) {
	load := func(body string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: [{name: a}, {name: e2e}]\n"+body), 0o600)
		return Load(p)
	}
	c, err := load("priority:\n  - {match: {event: schedule}, level: low}\n")
	if err != nil {
		t.Fatal(err)
	}
	rt := Rule{ID: "r-1", Runtime: true, PriorityRule: PriorityRule{Match: RuleMatch{Workflow: "deploy-*.yml", Branch: "release/*"}, Level: "high"}}
	rules := c.Rules([]Rule{rt})
	if len(rules) != 2 || rules[0].ID != "r-1" || rules[1].ID != "file-1" {
		t.Fatalf("rules %+v", rules)
	}
	j := WaitingJob{Class: "a", WorkflowRef: "o/r/.github/workflows/deploy-x.yml@refs/heads/release/1", Event: "schedule"}
	if level, why := LevelIn(rules, j); level != 1 || why != "high by runtime rule r-1 (workflow: deploy-*.yml, branch: release/*)" {
		t.Errorf("%d %q", level, why)
	}
	if level, why := c.LevelOf(j); level != -1 || why != "low by rule 1 (event: schedule)" {
		t.Errorf("file rules alone: %d %q", level, why)
	}
	if !MayBeHigh(rules, "e2e") || MayBeHigh(c.Rules(nil), "a") {
		t.Error("MayBeHigh")
	}
	back, err := load("priority:\n  " + rt.YAML() + "\n")
	if err != nil || len(back.Priority) != 1 || back.Priority[0] != rt.PriorityRule {
		t.Errorf("%s loads as %+v (%v)", rt.YAML(), back, err)
	}

	for token, ok := range map[string]bool{"0123456789abcdef": true, "short": false, "0123456789 abcdef": false} {
		if _, err := load("dashboardToken: \"" + token + "\"\n"); (err == nil) != ok {
			t.Errorf("dashboardToken %q: %v", token, err)
		}
	}
}

// cpuOvercommit is 1.5 unless set; 1 turns it off; below 1 or above 4 is refused.
func TestCPUOvercommit(t *testing.T) {
	load := func(extra string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: [{name: a}]\nhosts: [{ssh: h}]\n"+extra), 0o600)
		return Load(p)
	}
	for extra, want := range map[string]float64{"": DefaultCPUOvercommit, "cpuOvercommit: 1\n": 1, "cpuOvercommit: 2\n": 2} {
		c, err := load(extra)
		if err != nil || c.CPUOvercommit != want {
			t.Errorf("%q: %v %v, want %g", extra, c, err, want)
		}
	}
	for _, bad := range []string{"cpuOvercommit: 0.5\n", "cpuOvercommit: -1\n", "cpuOvercommit: 5\n"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "cpuOvercommit") {
			t.Errorf("%q: err %v", bad, err)
		}
	}
}

func TestWarm(t *testing.T) {
	load := func(extra string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: [{name: a}]\nhosts: [{ssh: h}]\n"+extra), 0o600)
		return Load(p)
	}
	for extra, want := range map[string]int{"": DefaultWarm, "warm: 0\n": 0, "warm: 3\n": 3} {
		c, err := load(extra)
		if err != nil || *c.Warm != want {
			t.Errorf("%q: %v %v, want %d", extra, c, err, want)
		}
	}
	for _, bad := range []string{"warm: -1\n", "warm: 33\n"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "warm") {
			t.Errorf("%q: err %v", bad, err)
		}
	}
}

func TestWarmMin(t *testing.T) {
	load := func(class string) error {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: ["+class+"]\nhosts: [{ssh: h}]\n"), 0o600)
		_, err := Load(p)
		return err
	}
	if err := load("{name: a, warmMin: 1}"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{name: a, warmMin: -1}", "{name: a, warmMin: 33}", "{name: a, warmMin: 1, overflow: true}"} {
		if err := load(bad); err == nil || !strings.Contains(err.Error(), "warmMin") {
			t.Errorf("%s: err %v", bad, err)
		}
	}
}

func TestAhead(t *testing.T) {
	for extra, want := range map[string]bool{"": true, "ahead: false\n": false} {
		p := filepath.Join(t.TempDir(), "pumasi.yaml")
		os.WriteFile(p, []byte("url: u\nclasses: [{name: a}]\nhosts: [{ssh: h}]\n"+extra), 0o600)
		c, err := Load(p)
		if err != nil || c.AheadOn() != want {
			t.Errorf("%q: %v, ahead %v, want %v", extra, err, c != nil && c.AheadOn(), want)
		}
	}
	if (&Config{}).AheadOn() {
		t.Error("a Config not made by Load starts runners ahead")
	}
}
