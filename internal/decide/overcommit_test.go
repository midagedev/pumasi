package decide

import (
	"strings"
	"testing"
)

// shared is a server that measures cpu pressure and lends its cores without
// a budget: 16 cores, 2 used by the rest of the machine, 64G of memory free.
func shared() Host {
	return Host{Name: "big", Kind: "server", OS: "linux", Up: true, CPUs: 16, MemBytes: 72 * gb, NonRunnerCPU: 2, NonRunnerMem: 4 * gb,
		KeepMem: 4 * gb, CPUPressureKnown: true}
}

// cpuJobs are n waiting jobs measured at 4 cpus and 2G each: cpu runs out long before memory.
func cpuJobs(n int) []Demand {
	ds := demand("linux-s", n)
	for i := range ds {
		ds[i].CPUs, ds[i].MemBytes = 4, 2*gb
	}
	return ds
}

func overcommitted(h Host, n int, ratio float64) Snapshot {
	s := snap([]Host{h}, cpuJobs(n), nil)
	s.CPUOvercommit = ratio
	return s
}

// The cpu runners hold may add up to cpuOvercommit times the cores, less
// what the rest of the machine uses: 16*1.5-2 = 22 cpus, five 4-cpu jobs
// where the cores alone take three. The start says the cores are shared.
func TestCPUOvercommitPlacesPastTheCores(t *testing.T) {
	p := Decide(overcommitted(shared(), 8, 1.5))
	if len(p.Start) != 5 {
		t.Fatalf("started %d, want 5: %v", len(p.Start), starts(p))
	}
	if r := p.Start[4].Reason; !strings.Contains(r, "cores shared: runners hold 20.0cpu of 14.0 there") {
		t.Fatalf("reason of the fifth: %q", r)
	}
	if r := p.Start[2].Reason; strings.Contains(r, "cores shared") {
		t.Fatalf("the third fits the cores, yet: %q", r)
	}
	if !strings.Contains(p.Defer[0].Reason, "big 2.0cpu/") {
		t.Fatalf("the sixth's reason does not count the shared cores: %q", p.Defer[0].Reason)
	}
	// Off, or 1: the cores alone.
	for _, ratio := range []float64{0, 1} {
		if p := Decide(overcommitted(shared(), 8, ratio)); len(p.Start) != 3 {
			t.Fatalf("ratio %g started %d, want 3", ratio, len(p.Start))
		}
	}
}

// Memory is never overcommitted: jobs of 20G stop at the memory, whatever the cpu.
func TestCPUOvercommitLeavesMemoryAlone(t *testing.T) {
	s := overcommitted(shared(), 8, 4)
	for i := range s.Demand {
		s.Demand[i].CPUs, s.Demand[i].MemBytes = 1, 20*gb
	}
	if p := Decide(s); len(p.Start) != 3 {
		t.Fatalf("started %d, want 3 (64G free)", len(p.Start))
	}
}

// Running runners count with what they hold: a host whose runners already
// hold its overcommitted cpu takes none.
func TestCPUOvercommitCountsRunningRunners(t *testing.T) {
	s := overcommitted(shared(), 3, 1.5)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		s.Runners = append(s.Runners, working(Runner{Name: name, Class: "linux-s", Host: "big", State: "busy", JobKnown: true, CreatedAt: t0, Container: "running", CPUs: 4.4, MemBytes: 2 * gb}))
	}
	if p := Decide(s); len(p.Start) != 0 {
		t.Fatalf("started %v on a host whose runners hold 22cpu", starts(p))
	}
}

// Where it does not apply, the cores are what they were: a desktop (its
// person keeps every core), a host with a cpu budget (the agent refuses
// past it), a host that does not measure cpu pressure, no threshold to stop at.
func TestCPUOvercommitOnlyWhereCPUPressureGuards(t *testing.T) {
	desk := shared()
	desk.Kind = "desktop"
	budget := shared()
	budget.BudgetCPUs = 14
	blind := shared()
	blind.CPUPressureKnown = false
	for name, h := range map[string]Host{"desktop": desk, "budget": budget, "no pressure measured": blind} {
		if p := Decide(overcommitted(h, 8, 1.5)); len(p.Start) != 3 {
			t.Fatalf("%s: started %d, want 3", name, len(p.Start))
		}
	}
	s := overcommitted(shared(), 8, 1.5)
	s.PSICPUThreshold = 0
	if p := Decide(s); len(p.Start) != 3 {
		t.Fatalf("no cpu pressure threshold: started %d, want 3", len(p.Start))
	}
}

// Cpu pressure over the threshold closes the host as before; overcommit
// does not open it.
func TestCPUPressureStillStopsOvercommit(t *testing.T) {
	h := shared()
	h.PSICPU = 45
	p := Decide(overcommitted(h, 2, 1.5))
	if len(p.Start) != 0 || !strings.Contains(p.Defer[0].Reason, "big cpu pressure 45") {
		t.Fatalf("started %v, defers %+v", starts(p), p.Defer)
	}
	// It opens again only a fifth under the line (32), as before.
	h.PSICPU, h.Pressed = 35, []string{"cpu"}
	if p := Decide(overcommitted(h, 2, 1.5)); len(p.Start) != 0 {
		t.Fatalf("a pressed host at 35 took %v", starts(p))
	}
}

// An overflow class offers the room the overcommitted cpu leaves.
func TestCPUOvercommitInOverflowRoom(t *testing.T) {
	s := overcommitted(shared(), 0, 1.5)
	s.Demand = nil
	s.Classes = []Class{{Name: "light", CPUs: 4, MemBytes: 2 * gb, Overflow: true}}
	if r := Decide(s).Room["light"]; r != 5 {
		t.Fatalf("room %d, want 5", r)
	}
	s.CPUOvercommit = 1
	if r := Decide(s).Room["light"]; r != 3 {
		t.Fatalf("room without overcommit %d, want 3", r)
	}
}
