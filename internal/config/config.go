// Package config loads the controller's YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	URL           string `yaml:"url"` // registration URL, e.g. https://github.com/owner/repo
	RunnerGroupID int    `yaml:"runnerGroupID"`
	Image         string `yaml:"image"`
	Ledger        string `yaml:"ledger"`
	EventLog      string `yaml:"eventLog"`

	EvaluateEvery    time.Duration `yaml:"evaluateEvery"`
	ProvisionTimeout time.Duration `yaml:"provisionTimeout"` // created -> listening
	IdleTimeout      time.Duration `yaml:"idleTimeout"`      // listening -> JobStarted
	HostLostAfter    time.Duration `yaml:"hostLostAfter"`
	DemandTTL        time.Duration `yaml:"demandTTL"`
	PSICPUThreshold  float64       `yaml:"psiCPUThreshold"`

	Classes []Class `yaml:"classes"`
	Hosts   []Host  `yaml:"hosts"`
}

type Class struct {
	Name       string   `yaml:"name"`
	ScaleSet   string   `yaml:"scaleSet"`
	Labels     []string `yaml:"labels"`
	CPUs       float64  `yaml:"cpus"`
	Memory     Bytes    `yaml:"memory"`
	MaxRunners int      `yaml:"maxRunners"`
	Hosts      []string `yaml:"hosts"` // empty = every host
}

type Host struct {
	Name string `yaml:"name"` // default: the ssh destination
	SSH  string `yaml:"ssh"`  // ssh destination (alias from ~/.ssh/config)
	// Shell is the remote command that reads a script on stdin. Default "sh -s";
	// a Windows host running WSL uses "wsl -e sh -s".
	Shell     string `yaml:"shell"`
	AgentPort int    `yaml:"agentPort"`
	Desktop   bool   `yaml:"desktop"` // someone works on it: filled after servers
	Kind      string `yaml:"kind"`    // server | desktop (same as desktop: true)
	Budget    struct {
		CPUs   float64 `yaml:"cpus"`
		Memory Bytes   `yaml:"memory"`
	} `yaml:"budget"`
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
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.EvaluateEvery, 10*time.Second)
	def(&c.ProvisionTimeout, 3*time.Minute)
	def(&c.IdleTimeout, 5*time.Minute)
	def(&c.HostLostAfter, 2*time.Minute)
	def(&c.DemandTTL, 24*time.Hour)
	if c.RunnerGroupID == 0 {
		c.RunnerGroupID = 1
	}
	if c.Image == "" {
		c.Image = "ghcr.io/actions/actions-runner:2.338.2"
	}
	if c.Ledger == "" {
		c.Ledger = "~/.local/state/pumasi/pumasi.db"
	}
	c.Ledger = expandHome(c.Ledger)
	if c.EventLog == "" {
		c.EventLog = filepath.Join(c.StateDir(), "events.jsonl")
	}
	c.EventLog = expandHome(c.EventLog)
	if c.PSICPUThreshold == 0 {
		c.PSICPUThreshold = 40
	}
	for i := range c.Classes {
		cl := &c.Classes[i]
		if cl.ScaleSet == "" {
			cl.ScaleSet = "pumasi-" + cl.Name
		}
		if len(cl.Labels) == 0 {
			cl.Labels = []string{cl.ScaleSet, "self-hosted"}
		}
		if cl.MaxRunners == 0 {
			cl.MaxRunners = 10
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
		if h.AgentPort == 0 {
			h.AgentPort = 7391
		}
		if h.Kind == "" {
			h.Kind = "server"
			if h.Desktop {
				h.Kind = "desktop"
			}
		}
	}
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

func (c *Config) validate() error {
	if c.URL == "" {
		return fmt.Errorf("url is required")
	}
	if len(c.Classes) == 0 || len(c.Hosts) == 0 {
		return fmt.Errorf("at least one class and one host are required")
	}
	hosts := map[string]bool{}
	for _, h := range c.Hosts {
		if h.SSH == "" {
			return fmt.Errorf("every host needs ssh (an ssh destination)")
		}
		if !nameRE.MatchString(h.Name) {
			return fmt.Errorf("host %q: set name to letters, digits, '.', '_' or '-'", h.Name)
		}
		if h.Budget.CPUs <= 0 || h.Budget.Memory <= 0 {
			return fmt.Errorf("host %s: budget needs cpus and memory, e.g. budget: {cpus: 4, memory: 6G}", h.Name)
		}
		if h.Kind != "server" && h.Kind != "desktop" {
			return fmt.Errorf("host %s: kind must be server or desktop", h.Name)
		}
		if hosts[h.Name] {
			return fmt.Errorf("duplicate host %s", h.Name)
		}
		hosts[h.Name] = true
	}
	for _, cl := range c.Classes {
		if !nameRE.MatchString(cl.Name) || cl.CPUs <= 0 || cl.Memory <= 0 {
			return fmt.Errorf("class needs name, cpus and memory: %+v", cl)
		}
		for _, h := range cl.Hosts {
			if !hosts[h] {
				return fmt.Errorf("class %s: unknown host %s", cl.Name, h)
			}
		}
	}
	return nil
}
