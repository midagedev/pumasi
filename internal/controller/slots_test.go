package controller

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

func slotsConfig() *config.Config {
	return &config.Config{URL: "https://github.com/o/r", Image: "ghcr.io/actions/actions-runner:2.338.2", Classes: []config.Class{
		{Name: "fixed", CPUs: 2, Memory: 2 << 30, Slots: 2, OS: "linux", Driver: "native", Labels: []string{"self-hosted", "ci"}},
		{Name: "mac", CPUs: 4, Memory: 8 << 30, Slots: 1, OS: "macos", Driver: "native"},
		{Name: "cache", CPUs: 1, Memory: 1 << 30, Slots: 1, OS: "linux", Driver: "native", Mounts: []string{"/srv/cache"}, Hosts: []string{"s1", "d1"}},
		{Name: "sets", CPUs: 1, Memory: 1 << 30, OS: "linux", Driver: "docker"},
	}}
}

func classOf(p api.SlotPlan, name string) *api.SlotClass {
	for i := range p.Classes {
		if p.Classes[i].Name == name {
			return &p.Classes[i]
		}
	}
	return nil
}

func TestSlotPlans(t *testing.T) {
	cfg := slotsConfig()
	listening := []api.Slot{{Class: "fixed", N: 1, State: "listening"}}
	hosts := []slotHost{
		{Name: "s1", Kind: "server", OS: "linux", BudgetCPUs: 4, BudgetMem: 8 << 30, Paths: []string{"/srv/cache"}, Slots: listening},
		{Name: "d1", Kind: "desktop", OS: "", BudgetCPUs: 6, BudgetMem: 8 << 30, Slots: listening},
		{Name: "m1", Kind: "server", OS: "macos", Slots: []api.Slot{{Class: "mac", N: 1, State: "busy"}}},
		{Name: "q1", Kind: "server", OS: "linux", Quarantined: true},
	}
	plans := slotPlans(cfg, hosts)
	s1, d1, m1, q1 := plans["s1"], plans["d1"], plans["m1"], plans["q1"]

	// A host gets the classes of its OS that may run on it; never a scale set class.
	if classOf(s1, "fixed") == nil || classOf(s1, "cache") == nil || classOf(s1, "mac") != nil || classOf(s1, "sets") != nil {
		t.Errorf("s1 classes %+v", s1.Classes)
	}
	if len(m1.Classes) != 1 || m1.Classes[0].Name != "mac" {
		t.Errorf("m1 classes %+v", m1.Classes)
	}
	if classOf(q1, "cache") != nil {
		t.Errorf("q1 is not among the class's hosts: %+v", q1.Classes)
	}
	if c := classOf(s1, "fixed"); c.Slots != 2 || c.CPUs != 2 || strings.Join(c.Labels, ",") != "self-hosted,ci" || c.Closed != "" {
		t.Errorf("fixed on s1 %+v", c)
	}
	if s1.BudgetCPUs != 4 || s1.URL != cfg.URL || s1.Image != cfg.Image || s1.Version == "" {
		t.Errorf("s1 plan %+v", s1)
	}
	// Servers first: a desktop's class is closed while a server still listens for it.
	if c := classOf(d1, "fixed"); !strings.Contains(c.Closed, "servers first: s1") {
		t.Errorf("fixed on d1 %+v", c)
	}
	if c := classOf(d1, "cache"); !strings.Contains(c.Closed, "lacks /srv/cache") {
		t.Errorf("cache on d1 %+v", c)
	}
	if q1.Closed != "quarantined" {
		t.Errorf("q1 %+v", q1)
	}

	// Every server slot of the class took a job: the desktop opens.
	hosts[0].Slots = []api.Slot{{Class: "fixed", N: 1, State: "busy"}, {Class: "fixed", N: 2, State: "busy"}}
	again := slotPlans(cfg, hosts)
	if c := classOf(again["d1"], "fixed"); c.Closed != "" {
		t.Errorf("fixed on d1 with the server full: %+v", c)
	}
	if again["d1"].Version == d1.Version {
		t.Error("a changed plan kept its version")
	}
	// A server slot that is off for want of room does not hold the desktop back.
	hosts[0].Slots = []api.Slot{{Class: "fixed", N: 1, State: "busy"}, {Class: "fixed", N: 2, State: "off", Why: "no room: 0.0cpu free"}}
	if c := classOf(slotPlans(cfg, hosts)["d1"], "fixed"); c.Closed != "" {
		t.Errorf("fixed on d1 with the server out of room: %+v", c)
	}
	// The same input gives the same version: the host is not told again.
	if slotPlans(cfg, hosts)["s1"].Version != slotPlans(cfg, hosts)["s1"].Version {
		t.Error("version is not stable")
	}
}

func TestControllerNeedsNoTokenWhenEveryClassHasSlots(t *testing.T) {
	open := func(cfg *config.Config) (*Controller, error) {
		dir := t.TempDir()
		cfg.Ledger, cfg.EventLog = filepath.Join(dir, "l.db"), filepath.Join(dir, "events.jsonl")
		l, err := ledger.Open(cfg.Ledger)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return New(cfg, "", l, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	cfg := slotsConfig()
	if _, err := open(cfg); err == nil {
		t.Error("a config with a scale set class started without a token")
	}
	cfg.Classes = cfg.Classes[:3]
	c, err := open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.gh != nil || !c.hasSlots() {
		t.Errorf("gh=%v hasSlots=%v", c.gh, c.hasSlots())
	}
}

func TestStateShowsSlotsAndSaysWaitingIsUnknown(t *testing.T) {
	cfg := slotsConfig()
	h1 := config.Host{Name: "s1", SSH: "s1"}
	h1.Budget.CPUs, h1.Budget.Memory = 4, 8<<30
	cfg.Hosts = []config.Host{h1}
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Now()
	took := now.Add(-30 * time.Second)
	live := Status{Hosts: map[string]*api.Snapshot{"s1": {OS: "linux", Slots: []api.Slot{
		{Class: "fixed", N: 1, Name: "ci-1", Origin: "adopted", State: "busy", Job: "build", Runner: &api.Runner{JobStartedAt: &took}},
		{Class: "fixed", N: 2, Name: "ci-2", Origin: "adopted", State: "listening"},
		{Class: "cache", N: 1, Name: "cache-s1-1", Origin: "registered", State: "off", Why: "lacks /srv/cache"},
	}}}}
	st, err := CollectState(cfg, l, live, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Unknown, ",") != "fixed,mac,cache" {
		t.Errorf("unknown %v", st.Unknown)
	}
	h := st.Hosts[0]
	if h.Runners != 1 || h.Listening != 1 || h.UsedCPUs != 2 || h.UsedMem != 2<<30 {
		t.Errorf("only a slot that runs a job holds budget: %+v", h)
	}
	if len(st.Slots) != 3 || st.Slots[0].Job != "build" || st.Slots[0].AgeSeconds != 30 || st.Slots[2].Why != "lacks /srv/cache" {
		t.Errorf("slots %+v", st.Slots)
	}
}
