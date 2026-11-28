package decide

import (
	"strings"
	"testing"
	"time"
)

func sec(n float64) time.Time { return t0.Add(time.Duration(n * float64(time.Second))) }

// ran: one job of run in workflow "ci", came and ended in seconds from t0
// (ended < 0: not yet).
func ran(run, job, class string, came, ended float64) Ran {
	r := Ran{Run: run, Workflow: "ci", Job: job, Class: class, Came: sec(came)}
	if ended >= 0 {
		r.Ended = sec(ended)
	}
	return r
}

func TestLearnNeeds(t *testing.T) {
	var runs []Ran
	// Three runs: lint and unit come together; e2e comes 3s (4s, 5s) after
	// unit ends, the later of the two.
	for i, d := range []float64{3, 4, 5} {
		r := string(rune('a' + i))
		runs = append(runs,
			ran(r, "lint", "small", 0, 30),
			ran(r, "unit", "big", 1, 200),
			ran(r, "e2e", "big", 200+d, 400))
	}
	// A run re-run by hand an hour later: no job came for that.
	runs = append(runs, ran("d", "lint", "small", 0, 30), ran("d", "e2e", "big", 3600, -1))
	got := LearnNeeds(runs)
	if len(got) != 1 {
		t.Fatalf("needs %+v, want one", got)
	}
	if n := got[0]; n.After != "unit" || n.Job != "e2e" || n.Class != "big" || n.Delay != 4*time.Second || n.Seen != 3 {
		t.Fatalf("need %+v", n)
	}
	// Seen once: not kept.
	if got := LearnNeeds(runs[:3]); len(got) != 0 {
		t.Fatalf("one run: %+v", got)
	}
	// In fewer than half the runs where unit ended: not kept.
	for i := range 5 {
		r := string(rune('k' + i))
		runs = append(runs, ran(r, "unit", "big", 1, 200))
	}
	if got := LearnNeeds(runs); len(got) != 0 {
		t.Fatalf("e2e after unit in 3 of 8: %+v", got)
	}
}

func TestExpectedAndSoon(t *testing.T) {
	needs := []Need{{Workflow: "ci", After: "unit", Job: "e2e", Class: "big", Delay: 2 * time.Second}}
	going := []Going{
		{Run: "a", Workflow: "ci", Job: "unit", Ends: sec(64)},
		{Run: "b", Workflow: "ci", Job: "unit", Ends: sec(200)},
		{Run: "c", Workflow: "ci", Job: "unit", Ends: sec(50)}, // its e2e came
		{Run: "d", Workflow: "other", Job: "unit", Ends: sec(61)},
	}
	came := func(run, job string) bool { return run == "c" && job == "e2e" }
	exp := Expected(going, needs, came)
	if len(exp) != 2 || exp[0].Run != "a" || !exp[0].At.Equal(sec(66)) || exp[1].Run != "b" {
		t.Fatalf("expected %+v", exp)
	}
	// now = t0+60s, lead 8s: a's e2e at 66s is within it, b's is not.
	n, why := Soon(exp, sec(60), 8*time.Second)
	if n["big"] != 1 || why["big"] != "e2e expected in ~6s, when unit ends" {
		t.Fatalf("soon %v %v", n, why)
	}
}

// A class with jobs expected soon gets runners for them from the free
// room, sized for them, though it keeps none warm; one coming up counts.
func TestAheadStartsARunner(t *testing.T) {
	s := warmSnap(0, nil, nil)
	s.Classes[0].Soon, s.Classes[0].SoonCPUs, s.Classes[0].SoonMem = 2, 2, 2*gb
	s.Classes[0].SoonFor = "e2e expected in ~6s, when unit ends"
	p := Decide(s)
	if len(p.Start) != 2 {
		t.Fatalf("started %v, want 2", starts(p))
	}
	for _, st := range p.Start {
		if st.Job != "" || st.CPUs != 2 || st.MemBytes != 2*gb || !strings.HasPrefix(st.Reason, "ahead: e2e expected in ~6s, when unit ends") {
			t.Fatalf("start %+v", st)
		}
	}
	// One is up already and listening: one more.
	s.Runners = []Runner{idle("r1", s.Classes[0].Name, "server1", 5*time.Second, 2, 2*gb)}
	if p := Decide(s); len(p.Start) != 1 {
		t.Fatalf("with one up: %v", starts(p))
	}
	// Kept up past the idle timeout while the job is expected.
	s.Runners = []Runner{idle("r1", s.Classes[0].Name, "server1", 10*time.Minute, 2, 2*gb)}
	if p := Decide(s); len(p.Stop) != 0 {
		t.Fatalf("stopped %+v", p.Stop)
	}
	// Not for an overflow class.
	s.Runners, s.Classes[0].Overflow = nil, true
	if p := Decide(s); len(p.Start) != 0 {
		t.Fatalf("overflow: %v", starts(p))
	}
}
