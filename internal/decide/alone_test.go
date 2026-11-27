package decide

import (
	"strings"
	"testing"
)

// A job measured larger than any host of its class could ever lend (a macOS
// sum over a runner's processes came out at 113.8G on a 32G machine, and the
// job's estimate went to 127.2G) does not wait for good: it is sized at what
// a host lends with no runner on it, and runs there alone.
func TestJobLargerThanAnyHostRunsAlone(t *testing.T) {
	mac := Class{Name: "mac-diag", OS: "macos", CPUs: 4, MemBytes: 8 * gb}
	host := func(name string, mem int64) Host {
		return Host{Name: name, Kind: "server", OS: "macos", Up: true, CPUs: 10, MemBytes: mem, KeepMem: 3 * gb, NonRunnerCPU: 1, NonRunnerMem: 5 * gb}
	}
	job := Demand{ID: "diag", Name: "PR Diagnostics", Class: "mac-diag", FirstSeen: t0, CPUs: 6.2, MemBytes: 127*gb + gb/5}
	mk := func(hosts []Host, rs []Runner) Plan {
		s := snap(hosts, []Demand{job}, rs)
		s.Classes = []Class{mac, {Name: "mac-pr", OS: "macos", CPUs: 2, MemBytes: 4 * gb}}
		return Decide(s)
	}

	// The host is empty: the job starts with all the host can lend (32G less
	// 3G kept free and 5G others use), and says why.
	p := mk([]Host{host("mac-a", 32*gb)}, nil)
	if len(p.Start) != 1 || p.Start[0].MemBytes != 24*gb || p.Start[0].CPUs != 6.2 {
		t.Fatalf("empty host: starts %+v defers %+v", p.Start, p.Defer)
	}
	if r := p.Start[0].Reason; !strings.Contains(r, "its size 6.2cpu/127.2G is more than any host of mac-diag can ever lend (10.0cpu/29.0G at most): it runs alone on mac-a") {
		t.Errorf("reason %q", r)
	}

	// A runner of another class is there: the job waits for the host to be
	// empty, at the size it will take, and says so.
	other := Runner{Name: "r1", Class: "mac-pr", Host: "mac-a", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 4 * gb}
	p = mk([]Host{host("mac-a", 32*gb)}, []Runner{other})
	if len(p.Start) != 0 || len(p.Defer) != 1 {
		t.Fatalf("busy host: starts %+v defers %+v", p.Start, p.Defer)
	}
	if r := p.Defer[0].Reason; !strings.HasPrefix(r, "no host with 6.2cpu/29.0G free (mac-a 7.0cpu/20.0G); its size") || !strings.HasSuffix(r, "it runs alone on one once one is empty") {
		t.Errorf("reason %q", r)
	}

	// Of two hosts, the one with the most to lend.
	p = mk([]Host{host("mac-a", 64*gb), host("mac-b", 32*gb)}, nil)
	if len(p.Start) != 1 || p.Start[0].Host != "mac-a" || p.Start[0].MemBytes != 56*gb {
		t.Fatalf("two hosts: starts %+v", p.Start)
	}

	// A budget is what a host lends: the job is sized by it.
	b := host("mac-a", 32*gb)
	b.BudgetCPUs, b.BudgetMem = 4, 16*gb
	if p := mk([]Host{b}, nil); len(p.Start) != 1 || p.Start[0].MemBytes != 16*gb || p.Start[0].CPUs != 4 {
		t.Fatalf("budget: starts %+v defers %+v", p.Start, p.Defer)
	}

	// A host of another OS, or one that has not answered, does not count:
	// the job still runs alone on the Mac.
	linux := Host{Name: "big-linux", Kind: "server", Up: true, CPUs: 64, MemBytes: 512 * gb}
	unknown := Host{Name: "mac-c", OS: "macos"}
	if p := mk([]Host{host("mac-a", 32*gb), linux, unknown}, nil); len(p.Start) != 1 || p.Start[0].Host != "mac-a" {
		t.Fatalf("other hosts: starts %+v defers %+v", p.Start, p.Defer)
	}

	// A job that some host of the class could take once it is open waits for it.
	closed := host("mac-z", 256*gb)
	closed.Closed = "in use"
	if p := mk([]Host{host("mac-a", 32*gb), closed}, nil); len(p.Start) != 0 || !strings.Contains(p.Defer[0].Reason, "mac-z in use") {
		t.Fatalf("a larger host closed: starts %+v defers %+v", p.Start, p.Defer)
	}
}

// An overflow class whose next job is known to be larger than any of its
// hosts could ever lend offers room for one where a host is empty, not none.
func TestOverflowRoomForAJobLargerThanAnyHost(t *testing.T) {
	c := Class{Name: "mac-diag", OS: "macos", CPUs: 4, MemBytes: 8 * gb, Overflow: true, NextCPUs: 6.2, NextMem: 127 * gb}
	h := Host{Name: "mac-a", Kind: "server", OS: "macos", Up: true, CPUs: 10, MemBytes: 32 * gb, KeepMem: 3 * gb}
	s := snap([]Host{h}, nil, nil)
	s.Classes = []Class{c, {Name: "mac-pr", OS: "macos", CPUs: 2, MemBytes: 4 * gb}}
	if p := Decide(s); p.Room["mac-diag"] != 1 {
		t.Fatalf("empty host: room %d, want 1", p.Room["mac-diag"])
	}
	s.Runners = []Runner{{Name: "r1", Class: "mac-pr", Host: "mac-a", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 4 * gb}}
	if p := Decide(s); p.Room["mac-diag"] != 0 {
		t.Fatalf("busy host: room %d, want 0", p.Room["mac-diag"])
	}
	s.Runners, s.BlindHold = nil, 3
	if p := Decide(s); p.Room["mac-diag"] != 1 {
		t.Fatalf("empty host, blind hold on: room %d, want 1", p.Room["mac-diag"])
	}
}
