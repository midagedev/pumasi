package decide

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// One host, room for one 2cpu/3G runner; jobs of three classes.
func expediteSnap(demand []Demand, runners []Runner) Snapshot {
	h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 2, MemBytes: 4 * gb}
	s := snap([]Host{h}, demand, runners)
	s.Now, s.HeadStart = t0, 30*time.Minute
	s.Classes = []Class{{Name: "build", CPUs: 2, MemBytes: 3 * gb}, {Name: "deploy", CPUs: 2, MemBytes: 3 * gb}, {Name: "docs", CPUs: 2, MemBytes: 3 * gb}}
	return s
}

func waiting(id, class string, ago time.Duration) Demand {
	return Demand{ID: id, Name: id, Class: class, FirstSeen: t0.Add(-ago)}
}

func expedite(d Demand, n int) Demand {
	d.Expedite, d.Ranked = n, "expedited"
	return d
}

func lineIDs(p Plan) []string {
	var out []string
	for _, l := range p.Line {
		out = append(out, l.ID)
	}
	return out
}

// An expedited job is first in line: before older jobs and before high ones,
// however long they have waited.
func TestExpeditedJobGoesFirst(t *testing.T) {
	high := waiting("release", "deploy", 2*time.Hour)
	high.Level, high.Ranked = 1, "high by rule 1"
	ds := []Demand{waiting("old", "build", 3*time.Hour), high, expedite(waiting("fix", "docs", time.Second), 1)}
	p := Decide(expediteSnap(ds, nil))
	if len(p.Start) != 1 || p.Start[0].Job != "fix" {
		t.Fatalf("started %+v, want the expedited job", p.Start)
	}
	if got := lineIDs(p); !reflect.DeepEqual(got, []string{"fix", "old", "release"}) {
		// old has waited 3h: the head start of release (30m) does not reach it
		t.Errorf("line %v", got)
	}
	if !strings.Contains(p.Start[0].Reason, "expedited") {
		t.Errorf("reason %q does not say it was expedited", p.Start[0].Reason)
	}
}

// Expedited jobs among themselves go in the order they were expedited.
func TestExpeditedJobsGoInTheOrderGiven(t *testing.T) {
	ds := []Demand{expedite(waiting("a", "build", time.Hour), 2), expedite(waiting("b", "docs", time.Minute), 1)}
	p := Decide(expediteSnap(ds, nil))
	if len(p.Start) != 1 || p.Start[0].Job != "b" {
		t.Fatalf("started %+v, want b (expedited first)", p.Start)
	}
	if !strings.Contains(p.Defer[0].Reason, "kept for expedited b") && !strings.Contains(p.Defer[0].Reason, "no host with") {
		t.Errorf("a waits for %q", p.Defer[0].Reason)
	}
}

// GitHub gives a class its oldest job first: the jobs ahead of an expedited
// one in its class go with it, and say why.
func TestJobsAheadInItsClassGoWithIt(t *testing.T) {
	ds := []Demand{waiting("lint", "build", 5*time.Minute), waiting("test", "build", 4*time.Minute),
		expedite(waiting("hotfix", "build", time.Minute), 1), waiting("after", "build", 0), waiting("docs", "docs", time.Hour)}
	p := Decide(expediteSnap(ds, nil))
	if got := lineIDs(p); !reflect.DeepEqual(got, []string{"lint", "test", "hotfix", "docs", "after"}) {
		t.Fatalf("line %v", got)
	}
	l := p.Line[0]
	if l.Expedite != 1 || l.Ranked != "ahead of hotfix in its class, which is expedited" || l.Host != "h1" {
		t.Errorf("lint: %+v", l)
	}
	if p.Line[4].Expedite != 0 {
		t.Errorf("the job behind it is not pulled: %+v", p.Line[4])
	}
}

// No room: the host the expedited job can go to is kept. No other job is
// started there, the running job is not touched, and the moment it ends the
// expedited job starts there. A host the expedited job cannot use is not kept.
func TestExpeditedJobKeepsItsHostUntilARunningJobEnds(t *testing.T) {
	h1 := Host{Name: "h1", Kind: "server", Up: true, CPUs: 8, MemBytes: 12 * gb}
	h2 := Host{Name: "h2", Kind: "server", Up: true, CPUs: 8, MemBytes: 12 * gb}
	big := Class{Name: "big", CPUs: 2, MemBytes: 8 * gb, Hosts: []string{"h1"}}
	small := Class{Name: "small", CPUs: 1, MemBytes: gb}
	run := Runner{Name: "r1", Class: "small", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 1, MemBytes: 6 * gb}
	ds := []Demand{expedite(waiting("release", "big", time.Minute), 1),
		waiting("s1", "small", time.Hour), waiting("s2", "small", 50*time.Minute)}
	s := snap([]Host{h1, h2}, ds, []Runner{run})
	s.Now, s.HeadStart, s.Classes = t0, 30*time.Minute, []Class{big, small}
	p := Decide(s)
	if len(p.Stop) != 0 {
		t.Fatalf("stopped %+v for an expedited job", p.Stop)
	}
	for _, st := range p.Start {
		if st.Host == "h1" {
			t.Fatalf("started %+v on the host kept for the expedited job", st)
		}
	}
	if len(p.Start) != 2 {
		t.Errorf("started %v, want both small jobs on h2 (the expedited job cannot go there)", starts(p))
	}
	// h2 full: the small jobs do not go to h1, and say why.
	s.Hosts[1].Up = false
	p = Decide(s)
	if len(p.Start) != 0 || len(p.Defer) != 2 || !strings.Contains(p.Defer[1].Reason, "h1 kept for expedited release of big") {
		t.Fatalf("h2 down: started %v, defers %+v", starts(p), p.Defer)
	}
	// The running job ends: the expedited job takes its room.
	s.Runners = nil
	p = Decide(s)
	if len(p.Start) == 0 || p.Start[0].Job != "release" || p.Start[0].Host != "h1" {
		t.Fatalf("after the running job: started %+v", p.Start)
	}
	if got := starts(p); !reflect.DeepEqual(got, []string{"big@h1", "small@h1", "small@h1"}) {
		t.Errorf("started %v: the room the expedited job leaves goes to the others as before", got)
	}
}

// An expedited job that would not fit a host even with every runner gone does
// not keep that host: nobody would gain from the wait. (It waits for a larger
// host of its class, closed now; were there none, it would run alone, see
// TestJobLargerThanAnyHostRunsAlone.)
func TestExpeditedJobThatNeverFitsKeepsNothing(t *testing.T) {
	ds := []Demand{expedite(Demand{ID: "huge", Name: "huge", Class: "build", FirstSeen: t0, CPUs: 2, MemBytes: 40 * gb}, 1), waiting("d", "docs", 0)}
	s := expediteSnap(ds, nil)
	s.Hosts = append(s.Hosts, Host{Name: "h9", Kind: "server", Up: true, Closed: "in use", CPUs: 16, MemBytes: 64 * gb, KeepMem: 4 * gb})
	p := Decide(s)
	if len(p.Start) != 1 || p.Start[0].Job != "d" {
		t.Fatalf("started %+v, want d", p.Start)
	}
}

// An expedited job GitHub has not given its overflow class yet (pending):
// no runner is started for it; with room the host is kept and the class
// offers it (Room), without room the host is kept until a running job ends.
// Another overflow class does not count the kept host as room.
func TestPendingExpeditedJobOfAnOverflowClass(t *testing.T) {
	h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb}
	over := Class{Name: "over", CPUs: 2, MemBytes: 3 * gb, Overflow: true, NextCPUs: 2, NextMem: 3 * gb}
	other := Class{Name: "other", CPUs: 2, MemBytes: 3 * gb, Overflow: true, NextCPUs: 2, NextMem: 3 * gb}
	plain := Class{Name: "plain", CPUs: 2, MemBytes: 3 * gb}
	pending := expedite(waiting("j", "over", 0), 1)
	pending.Pending = true
	run := Runner{Name: "r1", Class: "plain", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 3 * gb}

	s := snap([]Host{h}, []Demand{pending, waiting("p1", "plain", time.Hour)}, []Runner{run})
	s.Now, s.HeadStart, s.Classes = t0, 30*time.Minute, []Class{over, other, plain}
	p := Decide(s)
	if len(p.Start) != 0 || len(p.Stop) != 0 {
		t.Fatalf("room for one: started %v stopped %+v, want neither (the room is kept for j)", starts(p), p.Stop)
	}
	if p.Room["over"] != 1 || p.Room["other"] != 0 {
		t.Errorf("room %v, want over 1 (GitHub sends it the next job), other 0 (h1 is kept)", p.Room)
	}
	if len(p.Defer) != 1 || p.Defer[0].Class != "plain" || !strings.Contains(p.Defer[0].Reason, "kept for expedited j of over") {
		t.Errorf("defers %+v: the pending job is no defer of its class; plain waits for the kept host", p.Defer)
	}
	if l := p.Line[0]; l.ID != "j" || l.Host != "" || !strings.Contains(l.Reason, "GitHub still holds it") {
		t.Errorf("line %+v", p.Line)
	}
	// The class's usual next job is bigger than what is left, j is not: the
	// room was kept for j, so the class still takes one.
	s.Classes[0].NextMem = 6 * gb
	if p := Decide(s); p.Room["over"] != 1 {
		t.Errorf("room %v with a bigger usual job, want over 1", p.Room)
	}
	s.Classes[0].NextMem = 3 * gb

	// No room at all: the host is kept, the class takes nothing it cannot place.
	s.Runners = append(s.Runners, Runner{Name: "r2", Class: "plain", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 3 * gb})
	p = Decide(s)
	if len(p.Start) != 0 || p.Room["over"] != 0 || len(p.Stop) != 0 {
		t.Fatalf("full: started %v room %v stopped %+v", starts(p), p.Room, p.Stop)
	}

	// The job comes: GitHub assigned it to the class. It starts like any other.
	s.Runners = s.Runners[:1]
	s.Demand[0].Pending = false
	p = Decide(s)
	if len(p.Start) != 1 || p.Start[0].Job != "j" {
		t.Fatalf("assigned: started %+v", p.Start)
	}
}

// A job of the overflow class the class holds is older than the pending one:
// GitHub gives the class that one first, so it is pulled along and keeps the host.
func TestPendingJobPullsTheJobsItsClassHolds(t *testing.T) {
	h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb}
	over := Class{Name: "over", CPUs: 2, MemBytes: 3 * gb, Overflow: true}
	plain := Class{Name: "plain", CPUs: 2, MemBytes: 3 * gb}
	pending := expedite(waiting("j", "over", time.Hour), 1) // queued long ago at GitHub
	pending.Pending = true
	s := snap([]Host{h}, []Demand{pending, waiting("held", "over", time.Minute), waiting("p", "plain", 2*time.Hour)}, nil)
	s.Now, s.HeadStart, s.Classes = t0, 30*time.Minute, []Class{over, plain}
	p := Decide(s)
	if got := lineIDs(p); !reflect.DeepEqual(got, []string{"held", "j", "p"}) {
		t.Fatalf("line %v, want the held job first, then the pending one", got)
	}
	if p.Line[0].Ranked != "ahead of j in its class, which is expedited" || p.Line[0].Host != "h1" {
		t.Errorf("held: %+v", p.Line[0])
	}
}

// high-only: only expedited and high jobs (and the jobs ahead of a high one
// in its class) get new runners; the others wait with the mode as reason. An
// overflow class that no rule can make high takes nothing meanwhile.
func TestHighOnlyMode(t *testing.T) {
	h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb}
	hi := waiting("deploy", "deploy", 0)
	hi.Level, hi.Ranked = 1, "high by rule 1"
	ds := []Demand{waiting("build", "build", time.Hour), waiting("lint", "deploy", time.Minute), hi,
		expedite(waiting("fix", "docs", 0), 1)}
	s := snap([]Host{h}, ds, nil)
	over := Class{Name: "over", CPUs: 1, MemBytes: gb, Overflow: true}
	overHigh := over
	overHigh.Name, overHigh.MayBeHigh = "overhigh", true
	s.Now, s.HeadStart, s.HighOnly = t0, 30*time.Minute, "mode high-only until 13:00"
	s.Classes = []Class{{Name: "build", CPUs: 2, MemBytes: 3 * gb}, {Name: "deploy", CPUs: 2, MemBytes: 3 * gb}, {Name: "docs", CPUs: 2, MemBytes: 3 * gb}, over, overHigh}
	p := Decide(s)
	var got []string
	for _, st := range p.Start {
		got = append(got, st.Job)
	}
	if !reflect.DeepEqual(got, []string{"fix", "lint", "deploy"}) {
		t.Fatalf("started %v, want fix (expedited), lint (ahead of deploy), deploy", got)
	}
	if len(p.Defer) != 1 || p.Defer[0].Class != "build" || p.Defer[0].Reason != "mode high-only until 13:00" {
		t.Errorf("defers %+v", p.Defer)
	}
	if p.Room["over"] != 0 || p.Room["overhigh"] == 0 {
		t.Errorf("room %v: over takes nothing, overhigh may take a high job", p.Room)
	}
	s.HighOnly = ""
	if p := Decide(s); len(p.Start) != 4 {
		t.Errorf("normal again: started %v", starts(p))
	}
}

// Whatever is expedited, pending or held back by the mode, no runner that
// works a job is stopped for it.
func TestExpediteAndModeStopNothing(t *testing.T) {
	for _, mode := range []string{"", "mode high-only"} {
		for _, pending := range []bool{false, true} {
			h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 2, MemBytes: 4 * gb}
			var rs []Runner
			for i, state := range []string{"busy", "idle", "starting"} {
				rs = append(rs, Runner{Name: string(rune('a' + i)), Class: "build", Host: "h1", State: state, Container: "running",
					JobKnown: i == 0, Working: state == "busy", CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 3 * gb})
			}
			e := expedite(Demand{ID: "e", Name: "e", Class: "docs", FirstSeen: t0, CPUs: 2, MemBytes: 3 * gb}, 1)
			e.Pending = pending
			s := snap([]Host{h}, []Demand{e, waiting("w", "build", time.Hour)}, rs)
			s.Now, s.HighOnly = t0.Add(time.Minute), mode
			s.Classes = []Class{{Name: "build", CPUs: 2, MemBytes: 3 * gb}, {Name: "docs", CPUs: 2, MemBytes: 3 * gb, Overflow: true}}
			if p := Decide(s); len(p.Stop) != 0 || len(p.Start) != 0 {
				t.Errorf("mode %q pending %v: stopped %+v started %v", mode, pending, p.Stop, starts(p))
			}
		}
	}
}
