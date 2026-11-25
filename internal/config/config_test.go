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
