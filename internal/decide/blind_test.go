package decide

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Jobs nobody has measured are not put side by side on a guess: a host takes
// one on the guess alone, and the next only with the hold free.
func TestBlindJobsAreNotPlacedSideBySide(t *testing.T) {
	jvm := Class{Name: "build", CPUs: 2, MemBytes: 4 * gb}
	host := Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb, KeepMem: 3 * gb} // 29G to lend
	blind := func(n int) []Demand {
		ds := demand("build", n)
		for i := range ds {
			ds[i].Blind = true
		}
		return ds
	}
	mk := func(hosts []Host, d []Demand, rs []Runner, hold float64) Plan {
		s := snap(hosts, d, rs)
		s.Classes, s.BlindHold = []Class{jvm}, hold
		return Decide(s)
	}

	// 8 jobs at 4G would all fit (32G > 29G: 7 of them); held at 12G each, two do.
	p := mk([]Host{host}, blind(8), nil, 3)
	if len(p.Start) != 2 {
		t.Fatalf("blind jobs started at once = %d, want 2 (12G held each of 29G)", len(p.Start))
	}
	if !strings.Contains(p.Start[0].Reason, "holds 12.0G while it is watched") || p.Start[0].MemBytes != 4*gb {
		t.Errorf("start: %+v", p.Start[0])
	}
	if len(p.Defer) != 1 || p.Defer[0].Count != 6 || !strings.Contains(p.Defer[0].Reason, "watches 2 unmeasured job") {
		t.Errorf("defer: %+v", p.Defer)
	}
	// Measured jobs of the same size are placed as before.
	if p := mk([]Host{host}, demand("build", 8), nil, 3); len(p.Start) != 7 {
		t.Errorf("measured jobs started = %d, want 7", len(p.Start))
	}
	// Without the hold nothing changes.
	if p := mk([]Host{host}, blind(8), nil, 0); len(p.Start) != 7 {
		t.Errorf("hold off: started = %d, want 7", len(p.Start))
	}

	// A host too small for the hold still takes one blind job: it must get measured somewhere.
	small := Host{Name: "h2", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb, KeepMem: 1 * gb}
	if p := mk([]Host{small}, blind(3), nil, 3); len(p.Start) != 1 {
		t.Errorf("small host: started = %d, want 1", len(p.Start))
	}
	// And none while it watches one (the runner's charge already is the hold).
	watching := []Runner{{Name: "r1", Class: "build", Host: "h2", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 12 * gb, Watched: true}}
	if p := mk([]Host{small}, blind(1), watching, 3); len(p.Start) != 0 {
		t.Errorf("small host that watches one: started = %d, want 0", len(p.Start))
	}
	// A measured job is not held back by a watched one where its size is free.
	big := []Runner{{Name: "r1", Class: "build", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 12 * gb, Watched: true}}
	if p := mk([]Host{host}, demand("build", 2), big, 3); len(p.Start) != 2 {
		t.Errorf("measured next to a watched one: started = %d, want 2", len(p.Start))
	}
	// Once the watched job has shown its size (9G, no longer watched) the next blind job is the first again.
	shown := []Runner{{Name: "r1", Class: "build", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 9 * gb}}
	if p := mk([]Host{host}, blind(4), shown, 3); len(p.Start) != 1 {
		// 29-9 = 20G: the first takes 4G and holds 12G, the next would need 12G of the 8G left.
		t.Errorf("after the first has shown its size: started = %d, want 1", len(p.Start))
	}
}

// An overflow class is told how many more jobs fit after the plan, its own
// waiting jobs first, each as large as the class's jobs are known to be.
func TestRoomOfAnOverflowClass(t *testing.T) {
	c := Class{Name: "build", CPUs: 2, MemBytes: 4 * gb, Overflow: true}
	host := Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb, KeepMem: 3 * gb} // 29G to lend
	mk := func(c Class, hosts []Host, d []Demand) Plan {
		s := snap(hosts, d, nil)
		s.Classes = []Class{c, {Name: "other", CPUs: 1, MemBytes: gb}}
		return Decide(s)
	}
	if p := mk(c, []Host{host}, nil); p.Room["build"] != 7 {
		t.Errorf("empty host: room %d, want 7 (4G of 29G)", p.Room["build"])
	}
	if _, ok := mk(c, []Host{host}, nil).Room["other"]; ok {
		t.Error("room is worked out only for overflow classes")
	}
	// Two jobs are started by this plan: five more fit.
	if p := mk(c, []Host{host}, demand("build", 2)); len(p.Start) != 2 || p.Room["build"] != 5 {
		t.Errorf("after 2 starts: %d started, room %d", len(p.Start), p.Room["build"])
	}
	// Its jobs are known to be 10G: two fit, not seven.
	big := c
	big.NextCPUs, big.NextMem = 2, 10*gb
	if p := mk(big, []Host{host}, nil); p.Room["build"] != 2 {
		t.Errorf("10G jobs: room %d, want 2", p.Room["build"])
	}
	// No host up, or jobs already waiting for room: nothing more is taken.
	down := host
	down.Up = false
	if p := mk(c, []Host{down}, nil); p.Room["build"] != 0 {
		t.Errorf("host down: room %d", p.Room["build"])
	}
	if p := mk(c, []Host{host}, demand("build", 9)); len(p.Start) != 7 || p.Room["build"] != 0 {
		t.Errorf("more waiting than fits: %d started, room %d", len(p.Start), p.Room["build"])
	}
	// One job waits that is larger than the class's jobs are taken to be: the
	// class takes no more, though seven of the usual size would fit.
	held := snap([]Host{host}, []Demand{{ID: "big", Class: "build", FirstSeen: t0, CPUs: 4, MemBytes: 40 * gb}}, nil)
	held.Classes = []Class{c}
	if p := Decide(held); len(p.Start) != 0 || p.Room["build"] != 0 {
		t.Errorf("a job waits for room: %d started, room %d, want 0 and 0", len(p.Start), p.Room["build"])
	}
	// A class that has measured nothing takes what it would place of jobs that
	// are a guess: one on the guess, the next with three times that free.
	guess := func(hosts []Host, rs []Runner) int {
		s := snap(hosts, nil, rs)
		s.Classes, s.BlindHold = []Class{c}, 3
		return Decide(s).Room["build"]
	}
	if n := guess([]Host{host}, nil); n != 2 {
		t.Errorf("nothing measured, 29G: room %d, want 2 (12G held each)", n)
	}
	small := Host{Name: "h2", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb, KeepMem: 1 * gb}
	if n := guess([]Host{small}, nil); n != 1 {
		t.Errorf("nothing measured, small host: room %d, want 1", n)
	}
	watching := []Runner{{Name: "r1", Class: "build", Host: "h2", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 4 * gb, Watched: true}}
	if n := guess([]Host{small}, watching); n != 0 {
		t.Errorf("nothing measured, small host that watches one: room %d, want 0", n)
	}
	// A class that has measured some jobs small still meets jobs nobody has
	// measured; where its host watches one, such a job waits for the hold, so
	// the room there is counted on the hold, not on the measured size.
	measured := c
	measured.NextCPUs, measured.NextMem = 0.5, gb/2
	sized := func(hosts []Host, rs []Runner) int {
		s := snap(hosts, nil, rs)
		s.Classes, s.BlindHold = []Class{measured}, 3
		return Decide(s).Room["build"]
	}
	if n := sized([]Host{small}, nil); n != 8 {
		t.Errorf("measured 0.5cpu/0.5G, small host that watches none: room %d, want 8 (cpu)", n)
	}
	if n := sized([]Host{small}, watching); n != 0 {
		t.Errorf("measured small, host that watches one with 3G left: room %d, want 0", n)
	}
	if n := sized([]Host{host}, []Runner{{Name: "r1", Class: "build", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 4 * gb, Watched: true}}); n != 2 {
		t.Errorf("measured small, big host that watches one: room %d, want 2 (12G held each of the 25G left)", n)
	}
	capped := c
	capped.MaxRunners = 3
	if p := mk(capped, []Host{host}, demand("build", 2)); p.Room["build"] != 1 {
		t.Errorf("maxRunners 3 with 2 started: room %d, want 1", p.Room["build"])
	}
}

// The invariant over everything placement has learned to do since: a runner
// that works a job is never stopped by it. Not for going over its estimate or
// over what its host lends, not while it is watched, not for a job of a
// higher level that waits for its room, not for another class's reserve, not
// because its class takes only what fits, not on a closed or pressed host.
func TestNothingPlacementDoesStopsARunningJob(t *testing.T) {
	n := 0
	for _, over := range []float64{1, 3, 50} {
		for _, watched := range []bool{false, true} {
			for _, hold := range []float64{0, 3} {
				for _, overflow := range []bool{false, true} {
					for _, closed := range []string{"", "disk 2% free"} {
						for _, pressure := range []float64{0, 99} {
							for _, waitingLevel := range []int{-1, 0, 1} {
								h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb, KeepMem: gb,
									BudgetCPUs: 2, BudgetMem: 2 * gb, NonRunnerMem: 6 * gb, Closed: closed, PSICPU: pressure, PSIMem: pressure}
								mine := Class{Name: "build", CPUs: 1, MemBytes: gb, Overflow: overflow, NextCPUs: 1, NextMem: 4 * gb}
								vip := Class{Name: "deploy", CPUs: 1, MemBytes: gb, Reserve: 2}
								var rs []Runner
								for i, state := range []string{"busy", "busy", "idle", "starting"} {
									r := Runner{Name: fmt.Sprintf("r%d", i), Class: "build", Host: "h1", State: state, Container: "running",
										JobKnown: i == 0, Working: state == "busy", CreatedAt: t0, ListeningAt: t0, Watched: watched,
										CPUs: over, MemBytes: int64(float64(gb) * over)}
									if state == "busy" {
										r.JobStartedAt = t0
									}
									rs = append(rs, r)
								}
								var ds []Demand
								for i, class := range []string{"deploy", "build", "build"} {
									ds = append(ds, Demand{ID: fmt.Sprintf("d%d", i), Class: class, FirstSeen: t0, Level: waitingLevel, Blind: i == 2, Ranked: "rule"})
								}
								s := snap([]Host{h}, ds, rs)
								s.Classes, s.BlindHold, s.PSIMemThreshold, s.HeadStart = []Class{mine, vip}, hold, 10, 30*time.Minute
								p := Decide(s)
								n++
								if len(p.Stop) != 0 {
									t.Fatalf("over %gx watched %v hold %g overflow %v closed %q pressure %g level %d: stopped %+v",
										over, watched, hold, overflow, closed, pressure, waitingLevel, p.Stop)
								}
							}
						}
					}
				}
			}
		}
	}
	if n != 3*2*2*2*2*2*3 {
		t.Fatalf("%d combinations", n)
	}
}

// GitHub gives a class's jobs to its runners oldest first, whichever job a
// runner was started for. While an older job waits for room, no runner is
// started for a later, smaller job of its class: it would be handed the big
// one. A job of another class is not held up.
func TestNoJobPassesAnOlderJobOfItsClassThatWaitsForRoom(t *testing.T) {
	host := Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb, KeepMem: 3 * gb} // 29G to lend
	ds := []Demand{
		{ID: "big", Class: "build", FirstSeen: t0, CPUs: 4, MemBytes: 40 * gb},
		{ID: "small", Class: "build", FirstSeen: t0.Add(time.Second), CPUs: 1, MemBytes: gb},
		{ID: "other", Class: "other", FirstSeen: t0.Add(2 * time.Second)},
	}
	s := snap([]Host{host}, ds, nil)
	s.Classes = []Class{{Name: "build", CPUs: 2, MemBytes: 4 * gb}, {Name: "other", CPUs: 1, MemBytes: gb}}
	p := Decide(s)
	if got := starts(p); len(got) != 1 || got[0] != "other@h1" {
		t.Fatalf("started %v, want only other@h1", got)
	}
	waits := 0
	for _, d := range p.Defer {
		if d.Class == "build" {
			waits += d.Count
		}
	}
	if waits != 2 {
		t.Errorf("%d jobs of build wait, want 2", waits)
	}
	// With the big job first in line gone, the small one starts.
	s.Demand = ds[1:]
	if got := starts(Decide(s)); len(got) != 2 {
		t.Errorf("without the big job: started %v, want 2", got)
	}
}

// Jobs that waited together do not all start in the tick the room came, and
// a host whose disk is behind gets none.
func TestRunnersComeUpAFewAtATimeAndNotOnABusyDisk(t *testing.T) {
	host := Host{Name: "h1", Kind: "server", Up: true, CPUs: 32, MemBytes: 64 * gb}
	s := snap([]Host{host}, demand("build", 10), nil)
	s.Classes = []Class{{Name: "build", CPUs: 1, MemBytes: gb}}
	if n := len(Decide(s).Start); n != 10 {
		t.Fatalf("no limit: %d started, want 10", n)
	}
	s.MaxStarting = 4
	if n := len(Decide(s).Start); n != 4 {
		t.Errorf("at most 4 at a time: %d started", n)
	}
	// Three are still coming up from the tick before: one more.
	s.Runners = []Runner{
		{Name: "a", Class: "build", Host: "h1", State: "starting", Container: "running", CreatedAt: s.Now},
		{Name: "b", Class: "build", Host: "h1", State: "starting", Container: "running", CreatedAt: s.Now},
		{Name: "c", Class: "build", Host: "h1", State: "starting", Container: "running", CreatedAt: s.Now},
	}
	if n := len(Decide(s).Start); n != 1 {
		t.Errorf("3 coming up: %d started, want 1", n)
	}
	s.Runners, s.MaxStarting = nil, 0
	s.PSIIOThreshold = 50
	s.Hosts[0].PSIIO = 62
	p := Decide(s)
	if len(p.Start) != 0 || len(p.Defer) == 0 || !strings.Contains(p.Defer[0].Reason, "disk pressure 62") {
		t.Errorf("disk behind: %d started, deferred %+v", len(p.Start), p.Defer)
	}
	// Just under the line: a host that was over stays shut, one that was not takes.
	s.Hosts[0].PSIIO, s.Hosts[0].Pressed = 45, []string{"io"}
	if n := len(Decide(s).Start); n != 0 {
		t.Errorf("pressure 45 after being over 50: %d started, want 0", n)
	}
	s.Hosts[0].Pressed = nil
	if n := len(Decide(s).Start); n == 0 {
		t.Error("pressure 45, never over: nothing started")
	}
	s.Hosts[0].PSIIO, s.Hosts[0].Pressed = 39, []string{"io"}
	if n := len(Decide(s).Start); n == 0 {
		t.Error("pressure 39 after being over: still shut")
	}
}
