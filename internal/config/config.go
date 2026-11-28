// Package config loads the controller's YAML configuration.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	URL      string `yaml:"url"` // registration URL, e.g. https://github.com/owner/repo
	Image    string `yaml:"image"`
	Ledger   string `yaml:"ledger"`
	EventLog string `yaml:"eventLog"`

	// How fast pumasi reacts. The defaults are what was measured to work; the
	// keys exist for `make chaos`, which has to provoke each case in seconds.
	EvaluateEvery    time.Duration `yaml:"evaluateEvery"`
	ProvisionTimeout time.Duration `yaml:"provisionTimeout"` // created -> listening
	HostLostAfter    time.Duration `yaml:"hostLostAfter"`
	WaitAlert        time.Duration `yaml:"waitAlert"` // a job waiting this long is a problem
	// Not settable: nobody has needed another value.
	IdleTimeout     time.Duration `yaml:"-"` // listening -> JobStarted
	DemandTTL       time.Duration `yaml:"-"`
	PSICPUThreshold float64       `yaml:"-"`
	PSIMemThreshold float64       `yaml:"-"`
	PSIIOThreshold  float64       `yaml:"-"`
	MaxStarting     int           `yaml:"-"`
	// DiskMinFree: a host whose docker disk has less than this percent free gets no new runners.
	DiskMinFree float64 `yaml:"diskMinFree"`
	// Rerun: how often pumasi may rerun a workflow run's failed jobs after a
	// failure on its own side (host reboot, runner killed). 0 = only tell.
	Rerun int `yaml:"rerun"`

	Notify Notify `yaml:"notify"`

	// Watch: what is kept of finished jobs, and the limits set on them.
	Watch Watch `yaml:"watch"`

	// Listen: the address every host dials, the hosts of hosts: too. Default
	// DefaultListen: all of this machine's addresses.
	Listen string `yaml:"listen"`
	// Dashboard: where the dashboard, its JSON and /metrics are served.
	// Nothing there asks who is reading, so keep it on loopback and open it
	// elsewhere through a tunnel or an authenticating proxy. Default
	// DefaultDashboard: a free loopback port, written to the state directory.
	Dashboard string `yaml:"dashboard"`
	// DashboardToken lets the dashboard's writes (expedite a job, the mode,
	// runtime priority rules) come from other machines, with this token as
	// "Authorization: Bearer". Without it only this machine may write.
	DashboardToken string `yaml:"dashboardToken"`

	Classes []Class `yaml:"classes"`
	Hosts   []Host  `yaml:"hosts"`

	// Priority: which waiting jobs get a runner first while room is short.
	// The first rule that matches a job counts. A running `pumasi up` reads
	// this key again when the file changes; the rest of the file needs a restart.
	Priority []PriorityRule `yaml:"priority"`
	// PriorityHeadStart: a high job is placed as if it had come this much
	// earlier, a low one as if this much later (default 30m).
	PriorityHeadStart time.Duration `yaml:"priorityHeadStart"`

	// GitHubApp authenticates as a GitHub App's installation instead of with
	// a personal access token.
	GitHubApp GitHubApp `yaml:"githubApp"`
	// JobsMayReadCredentials ends the warning about hosts on the controller's
	// own machine whose jobs can read the controller's GitHub token and keys:
	// every workflow that reaches those classes is as trusted as the person
	// who runs the controller.
	JobsMayReadCredentials bool `yaml:"jobsMayReadCredentials"`
	// Path is the file the config was read from.
	Path string `yaml:"-"`
}

// GitHubApp names a GitHub App installed on the repository or organization.
// The private key is a file's path, never its content.
type GitHubApp struct {
	ID             string `yaml:"id"` // the App's client id (Iv23…) or app id
	InstallationID int64  `yaml:"installationId"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
}

// Set reports whether any of it was given.
func (a GitHubApp) Set() bool { return a.ID != "" || a.InstallationID != 0 || a.PrivateKeyFile != "" }

// Notify says where problems are announced. Both may be set.
type Notify struct {
	Webhook string `yaml:"webhook"` // JSON POST: {text, key, kind, subject, state, detail}
	Command string `yaml:"command"` // run with sh -c; gets the same JSON on stdin
	// problem (default): problems and their end. recovered: also what pumasi
	// fixed by itself. all: also plain information.
	Level string `yaml:"level"`
	// Away: two hosts are given the webhook and tell it when they have not
	// reached the controller for this long — a controller that is gone cannot
	// say so itself. Default 5m; 0 gives the webhook to no host.
	Away *time.Duration `yaml:"away"`
}

// AwayAfter is how long hosts wait before they say that the controller is
// gone (0 = they are not asked to).
func (n Notify) AwayAfter() time.Duration {
	switch {
	case n.Webhook == "":
		return 0
	case n.Away == nil:
		return 5 * time.Minute
	}
	return *n.Away
}

type Class struct {
	Name string `yaml:"name"`
	// OS the class's jobs need: linux (default), macos or windows. A class is
	// placed only on hosts of its OS.
	OS string `yaml:"os"`
	// CPUs and Memory are the first estimate: what a job is taken to need
	// while no run of it has finished (default 2 cpus, 4G). After that the
	// job is placed by what its last runs used. They are not a limit.
	CPUs   float64 `yaml:"cpus"`
	Memory Bytes   `yaml:"memory"`
	// Limit holds every runner of the class to these numbers (a job that goes
	// over memory loses a process to the kernel). Left out, a runner is held
	// only to what protects its host.
	Limit Size `yaml:"limit"`
	// Hosts binds the class to these hosts (empty = every host). Jobs that hand
	// each other files in a host directory need the same machine.
	Hosts []string `yaml:"hosts"`
	// Reserve keeps room for this many runners of the class: other classes may
	// not take it (deploy jobs that must not queue behind builds).
	Reserve int `yaml:"reserve"`
	// Driver: docker (a container per runner) or native (a process of the
	// host's user, which sees the host's paths, credentials and docker).
	// Default: docker on linux, native on macos and windows.
	Driver string `yaml:"driver"`
	// Mounts are host paths a container runner sees at the same path
	// ("/srv/cache", "/srv/keys:ro"). The class goes only to hosts that have
	// them all; a native class sees the host anyway and only uses them for that.
	Mounts []string `yaml:"mounts"`
	// Docker: inside gives each container runner a docker daemon of its own,
	// so what the job runs with docker counts against the runner's limits.
	// The runner container is privileged for it.
	Docker string `yaml:"docker"`
	// Overflow: the class takes only as many jobs as it has room for right
	// now. GitHub gives the rest to other runners of the same labels (fixed
	// runners, another class), where there are any; pumasi never sees those
	// jobs. Default: the class takes every job of its labels and lets them wait.
	Overflow bool `yaml:"overflow"`
	// After names another overflow class of the same labels that is filled
	// first: this class offers room only while that one has none.
	After string `yaml:"after"`
	// Labels replace the class's own label: the scale set takes jobs whose
	// runs-on is among them ([self-hosted, Linux, X64, ci-pool] takes over
	// from fixed runners with those labels, workflows unchanged).
	Labels []string `yaml:"labels"`
	// Env is added to the environment of the class's runners, and so of their
	// jobs: what a fixed runner got from its service unit or its .env file.
	// {runner} in a value is the runner's own directory, removed with it.
	// {slot} is the runner's number among the class's live runners on its
	// host (1, 2, …; the lowest free one): a place that stays, used by one
	// runner at a time.
	Env map[string]string `yaml:"env"`
	// Path goes in front of PATH of a native runner (a fixed runner's .path).
	Path []string `yaml:"path"`
	// Work is where a native runner keeps its work directory (_work: the
	// checkout, node_modules, build output), named with {slot}: the next
	// runner of that slot finds the last one's work there, as a fixed
	// runner's next job does. Empty = inside the runner, gone with it.
	Work string `yaml:"work"`
	// MaxDuration stops a runner whose job runs longer (0 = never: jobs are not touched).
	MaxDuration time.Duration `yaml:"maxDuration"`
}

// Host is a machine the controller makes join by itself: it goes there over
// ssh and runs the invite line with this name, budget and kind. From then on
// the host is connected like one that joined on its own.
// ScaleSet is the class's scale set on GitHub, and the label its jobs ask for:
// runs-on: [self-hosted, pumasi-<class>].
func (cl Class) ScaleSet() string { return "pumasi-" + cl.Name }

// Platform is what the class's runners run as: its OS and driver,
// "linux/docker". Measured runs are kept apart by it.
func (cl Class) Platform() string {
	os := cmp.Or(cl.OS, "linux")
	driver := cl.Driver
	if driver == "" {
		driver = "native"
		if os == "linux" {
			driver = "docker"
		}
	}
	return os + "/" + driver
}

// RunsOn is the labels of the class's scale set: a job comes to it when all
// of its runs-on labels are among them.
func (cl Class) RunsOn() []string {
	if len(cl.Labels) > 0 {
		return cl.Labels
	}
	return []string{"self-hosted", cl.ScaleSet()}
}

// Size is an amount of cpus and memory; zero = not said.
type Size struct {
	CPUs   float64 `yaml:"cpus"`
	Memory Bytes   `yaml:"memory"`
}

// The first estimate of a class that gives none.
const (
	DefaultCPUs   = 2
	DefaultMemory = 4 << 30
)

type Host struct {
	Name string `yaml:"name"` // default: the ssh destination
	SSH  string `yaml:"ssh"`  // ssh destination (alias from ~/.ssh/config)
	// Shell is the remote command that reads a script on stdin. Default "sh -s";
	// a Windows host running WSL uses "wsl -e sh -s".
	Shell   string `yaml:"shell"`
	Desktop bool   `yaml:"desktop"` // someone works on it: filled after servers, and theirs first
	// Budget caps what pumasi lends on the host. Left out, pumasi lends what
	// the machine has free as it measures it.
	Budget Size `yaml:"budget"`
}

// Kind is server or desktop.
func (h Host) Kind() string {
	if h.Desktop {
		return "desktop"
	}
	return "server"
}

// Bytes is a size written as 512M, 3G or a plain number of bytes.
type Bytes int64

func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseBytes(n.Value)
	*b = Bytes(v)
	return err
}

// ParseBytes parses "", "1024", "512M", "3G", "1.5G" (binary units).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40}} {
		if t, ok := strings.CutSuffix(strings.TrimSuffix(s, "B"), u.suffix); ok {
			s, mult = t, u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(f * float64(mult)), nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w (copy pumasi.example.yaml to %s, or pass -config)", err, path)
	}
	c := &Config{}
	// A key that is not known is a typo: maxduration instead of maxDuration
	// would otherwise run without the limit and say nothing.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	c.defaults()
	return c, c.validate()
}

const (
	DefaultListen    = "0.0.0.0:7411"
	DefaultDashboard = "127.0.0.1:0"
)

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Dashboard == "" {
		c.Dashboard = DefaultDashboard
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.EvaluateEvery, 10*time.Second)
	def(&c.ProvisionTimeout, 3*time.Minute)
	def(&c.HostLostAfter, 2*time.Minute)
	def(&c.WaitAlert, 10*time.Minute)
	def(&c.PriorityHeadStart, DefaultHeadStart)
	c.IdleTimeout, c.DemandTTL, c.PSICPUThreshold, c.PSIMemThreshold = 5*time.Minute, 24*time.Hour, 40, 10
	c.PSIIOThreshold, c.MaxStarting = 50, 4
	if c.DiskMinFree == 0 {
		c.DiskMinFree = 10
	}
	c.Watch.defaults()
	if c.Notify.Level == "" {
		c.Notify.Level = "problem"
	}
	if c.Image == "" {
		c.Image = "ghcr.io/actions/actions-runner:2.338.2"
	}
	if c.Ledger == "" {
		c.Ledger = "~/.local/state/pumasi/pumasi.db"
	}
	c.Ledger = expandHome(c.Ledger)
	c.GitHubApp.PrivateKeyFile = expandHome(c.GitHubApp.PrivateKeyFile)
	if c.EventLog == "" {
		c.EventLog = filepath.Join(c.StateDir(), "events.jsonl")
	}
	c.EventLog = expandHome(c.EventLog)
	for i := range c.Classes {
		cl := &c.Classes[i]
		if cl.OS == "" {
			cl.OS = "linux"
		}
		if cl.CPUs == 0 {
			cl.CPUs = DefaultCPUs
		}
		if cl.Memory == 0 {
			cl.Memory = DefaultMemory
		}
		if cl.Driver == "" {
			cl.Driver = "native"
			if cl.OS == "linux" {
				cl.Driver = "docker"
			}
		}
	}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if h.Name == "" {
			h.Name = h.SSH
		}
		if h.Shell == "" {
			h.Shell = "sh -s"
		}
	}
}

// winAbsRE is an absolute Windows path (C:\ci or C:/ci), checked where the
// controller runs on another OS.
var winAbsRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// MountPath is the path of a mounts entry, without its ":ro".
func MountPath(m string) string { return strings.TrimSuffix(m, ":ro") }

// WorkDir is where a runner of the class keeps its work directory. Native:
// the class's work, still with {slot} (the host's agent knows the number).
// Container: inside the class's first writable mount, so that the job's checkout and the
// caches in that mount are one filesystem behind one mount and tools can
// hard-link between them (pnpm's store, ccache). "" = inside the container.
func (cl Class) WorkDir(runner string) string {
	if cl.Driver == "native" {
		return cl.Work
	}
	if cl.Driver != "docker" {
		return ""
	}
	for _, m := range cl.Mounts {
		if !strings.HasSuffix(m, ":ro") {
			return m + "/.pumasi-work/" + runner
		}
	}
	return ""
}

// MountPaths is every host path some class asks for.
func (c *Config) MountPaths() []string {
	var out []string
	for _, cl := range c.Classes {
		for _, m := range cl.Mounts {
			if p := MountPath(m); !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// StateDir holds the ledger, the event log and the running controller's address.
func (c *Config) StateDir() string { return filepath.Dir(c.Ledger) }

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// Names end up in runner names, container names and remote scripts.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) validate() error {
	if c.URL == "" {
		return fmt.Errorf("url is required")
	}
	if _, _, err := net.SplitHostPort(c.Dashboard); err != nil {
		return fmt.Errorf("dashboard: %w (an address like 127.0.0.1:7420)", err)
	}
	if t := c.DashboardToken; t != "" && (len(t) < 16 || strings.ContainsAny(t, " \t\r\n")) {
		return fmt.Errorf("dashboardToken: at least 16 characters without spaces (openssl rand -hex 16)")
	}
	if l := c.Notify.Level; l != "problem" && l != "recovered" && l != "all" {
		return fmt.Errorf("notify.level must be problem, recovered or all")
	}
	if g := c.GitHubApp; g.Set() && (g.ID == "" || g.InstallationID <= 0 || g.PrivateKeyFile == "") {
		return fmt.Errorf("githubApp needs id, installationId and privateKeyFile")
	}
	if a := c.Notify.Away; a != nil && (*a < 0 || *a > 0 && *a < time.Minute) {
		return fmt.Errorf("notify.away must be 0 (no host is asked) or at least 1m")
	}
	if len(c.Classes) == 0 {
		return fmt.Errorf("at least one class is required")
	}
	for i, r := range c.Priority {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("priority rule %d: %w", i+1, err)
		}
	}
	hosts := map[string]bool{}
	for _, h := range c.Hosts {
		if h.SSH == "" {
			return fmt.Errorf("every host needs ssh (an ssh destination)")
		}
		if !nameRE.MatchString(h.Name) {
			return fmt.Errorf("host %q: set name to letters, digits, '.', '_' or '-'", h.Name)
		}
		if h.Budget.CPUs < 0 || h.Budget.Memory < 0 {
			return fmt.Errorf("host %s: budget must not be negative", h.Name)
		}
		if hosts[h.Name] {
			return fmt.Errorf("duplicate host %s", h.Name)
		}
		hosts[h.Name] = true
	}
	classes := map[string]bool{}
	for _, cl := range c.Classes {
		if !nameRE.MatchString(cl.Name) {
			return fmt.Errorf("class needs a name of letters, digits, '.', '_' or '-': %+v", cl)
		}
		if cl.CPUs < 0 || cl.Memory < 0 || cl.Limit.CPUs < 0 || cl.Limit.Memory < 0 {
			return fmt.Errorf("class %s: cpus, memory and limit must not be negative", cl.Name)
		}
		// Two classes of one name would be two listeners on one scale set.
		if classes[cl.Name] {
			return fmt.Errorf("duplicate class %s", cl.Name)
		}
		classes[cl.Name] = true
		for k, v := range cl.Env {
			if !envKeyRE.MatchString(k) || strings.ContainsAny(v, "\n\x00") {
				return fmt.Errorf("class %s: env %s: names are letters, digits and '_', values one line", cl.Name, k)
			}
		}
		for _, p := range cl.Path {
			if p == "" || strings.ContainsAny(p, "\n\x00") {
				return fmt.Errorf("class %s: path: empty or multi-line entry", cl.Name)
			}
		}
		if cl.OS != "linux" && cl.OS != "macos" && cl.OS != "windows" {
			return fmt.Errorf("class %s: os must be linux, macos or windows", cl.Name)
		}
		switch {
		case cl.Reserve < 0:
			return fmt.Errorf("class %s: reserve must not be negative", cl.Name)
		case cl.Driver != "docker" && cl.Driver != "native":
			return fmt.Errorf("class %s: driver must be docker or native", cl.Name)
		case cl.Docker != "" && cl.Docker != "inside":
			return fmt.Errorf("class %s: docker must be inside (or left out)", cl.Name)
		case cl.Work != "" && cl.Driver != "native":
			return fmt.Errorf("class %s: work is for native runners; a container runner keeps its work in its first writable mount", cl.Name)
		case cl.Work != "" && (!filepath.IsAbs(cl.Work) && !strings.HasPrefix(cl.Work, "/") && !winAbsRE.MatchString(cl.Work) || !strings.Contains(cl.Work, "{slot}") || strings.Contains(cl.Work, "{runner}") || strings.ContainsAny(cl.Work, "\n\x00")):
			return fmt.Errorf("class %s: work %q must be an absolute path with {slot} in it, so that two runners never share one", cl.Name, cl.Work)
		case len(cl.Path) > 0 && cl.Driver != "native":
			return fmt.Errorf("class %s: path is for native runners; a container has its image's PATH (set env: {PATH: ...} to replace it)", cl.Name)
		case cl.Docker == "inside" && cl.Driver != "docker":
			return fmt.Errorf("class %s: docker: inside is for container runners; a native runner uses the host's docker", cl.Name)
		case cl.Driver == "docker" && cl.OS != "linux":
			return fmt.Errorf("class %s: %s hosts run runners as processes only (driver: native)", cl.Name, cl.OS)
		}
		for _, m := range cl.Mounts {
			if p := MountPath(m); !strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":, \t\n") {
				return fmt.Errorf("class %s: mount %q must be an absolute path, optionally ending in :ro", cl.Name, m)
			}
		}
		if cl.After != "" {
			i := slices.IndexFunc(c.Classes, func(o Class) bool { return o.Name == cl.After })
			switch {
			case !cl.Overflow:
				return fmt.Errorf("class %s: after is for classes with overflow: true", cl.Name)
			case cl.After == cl.Name || i < 0:
				return fmt.Errorf("class %s: after names no other class (%q)", cl.Name, cl.After)
			case !c.Classes[i].Overflow:
				return fmt.Errorf("class %s: after: %s must have overflow: true too — a class that takes every job leaves none to come after it", cl.Name, cl.After)
			case c.Classes[i].After != "":
				return fmt.Errorf("class %s: after: %s itself comes after %s; one step only", cl.Name, cl.After, c.Classes[i].After)
			}
		}
		for _, h := range cl.Hosts {
			// A class may name a host that joins later.
			if !nameRE.MatchString(h) {
				return fmt.Errorf("class %s: bad host name %q", cl.Name, h)
			}
		}
	}
	return nil
}
