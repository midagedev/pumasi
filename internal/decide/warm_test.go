package decide

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWarmCount(t *testing.T) {
	at := func(secs ...int) []time.Time {
		var out []time.Time
		for _, s := range secs {
			out = append(out, t0.Add(time.Duration(s)*time.Second))
		}
		return out
	}
	for _, c := range []struct {
		name string
		secs []int
		most int
		want int
	}{
		{"nothing", nil, 8, 0},
		{"one burst says nothing of the next", []int{0, 1, 2, 3}, 8, 0},
		{"the largest of the bursts", []int{0, 1, 2, 60, 61, 62, 63, 64, 200}, 8, 5},
		{"jobs one at a time", []int{0, 120, 240}, 8, 1},
		{"at most most", []int{0, 1, 2, 3, 4, 60, 61}, 3, 3},
		{"a burst is within Burst of its first", []int{0, 10, 20, 30, 100}, 8, 2},
	} {
		if got := WarmCount(at(c.secs...), c.most); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func warmSnap(warm int, d []Demand, rs []Runner) Snapshot {
	s := snap([]Host{server()}, d, rs)
	s.Classes[0].Warm = warm
	return s
}

// idle is a runner of class listening since ago without a job.
func idle(name, class, host string, ago time.Duration, cpus float64, mem int64) Runner {
	at := t0.Add(time.Minute - ago)
	return Runner{Name: name, Class: class, Host: host, CPUs: cpus, MemBytes: mem, State: "idle", Container: "running", CreatedAt: at.Add(-10 * time.Second), ListeningAt: at}
}

func busy(name, class, host string, cpus float64, mem int64) Runner {
	return Runner{Name: name, Class: class, Host: host, CPUs: cpus, MemBytes: mem, State: "busy", JobKnown: true, Working: true, Container: "running",
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0}
}

// With nothing waiting, a warm class gets its runners from the free room,
// for no job, each holding what the class's next job is taken to need.
func TestWarmStartsFromFreeRoom(t *testing.T) {
	p := Decide(warmSnap(3, nil, nil))
	if len(p.Start) != 3 {
		t.Fatalf("started %v, want 3", starts(p))
	}
	for _, st := range p.Start {
		if st.Job != "" || st.CPUs != 1 || st.MemBytes != gb || !strings.HasPrefix(st.Reason, "warm: ") {
			t.Fatalf("start %+v", st)
		}
	}
	// Sized for the next job: 2cpu/2G each, and the budget's 4cpu take two.
	s := warmSnap(3, nil, nil)
	s.Classes[0].NextCPUs, s.Classes[0].NextMem = 2, 2*gb
	if p := Decide(s); len(p.Start) != 2 || p.Start[0].CPUs != 2 || p.Start[0].MemBytes != 2*gb {
		t.Fatalf("sized for the next job: %+v", p.Start)
	}
	// None for a class that keeps none.
	if p := Decide(warmSnap(0, nil, nil)); len(p.Start) != 0 {
		t.Fatalf("warm 0 started %v", starts(p))
	}
}

// Runners up or coming up without a job count, unless a job of the class
// waits for each: then they are the job's, and the warm ones come on top.
func TestWarmCountsSpareRunners(t *testing.T) {
	rs := []Runner{idle("s1", "linux-s", "server1", time.Minute, 1, gb)}
	if p := Decide(warmSnap(3, nil, rs)); len(p.Start) != 2 {
		t.Fatalf("one spare: started %v, want 2", starts(p))
	}
	if p := Decide(warmSnap(3, demand("linux-s", 1), rs)); len(p.Start) != 3 {
		t.Fatalf("the runner is the waiting job's: started %v, want 3", starts(p))
	}
}

// Room a waiting job is kept for, or a class with a job that waits for
// room, gets no runner kept warm.
func TestWarmNotWhereAJobWaits(t *testing.T) {
	// A linux-l job (2cpu/3G) waits: 1cpu/2G left of the budget, and the host is kept for it.
	rs := []Runner{busy("l1", "linux-l", "server1", 3, 4*gb)}
	p := Decide(warmSnap(1, demand("linux-l", 1), rs))
	if len(p.Start) != 0 || len(p.Defer) != 1 {
		t.Fatalf("kept host: started %v, defers %v", starts(p), defers(p))
	}
	// The warm class itself waits behind a job of its own too large for the room left.
	d := demand("linux-s", 1)
	d[0].CPUs, d[0].MemBytes = 2, 2*gb
	if p := Decide(warmSnap(1, d, rs)); len(p.Start) != 0 {
		t.Fatalf("class behind: started %v", starts(p))
	}
}

// A runner kept warm has no idle timeout; one past the class's Warm has.
func TestWarmRunnerHasNoIdleTimeout(t *testing.T) {
	old := idle("s1", "linux-s", "server1", 10*time.Minute, 1, gb)
	if p := Decide(warmSnap(1, nil, []Runner{old})); len(p.Stop) != 0 {
		t.Fatalf("warm runner stopped: %+v", p.Stop)
	}
	if p := Decide(warmSnap(0, nil, []Runner{old})); len(p.Stop) != 1 || !strings.Contains(p.Stop[0].Reason, "idle timeout") {
		t.Fatalf("not warm: %+v", p.Stop)
	}
	older := idle("s0", "linux-s", "server1", 20*time.Minute, 1, gb)
	p := Decide(warmSnap(1, nil, []Runner{old, older}))
	if len(p.Stop) != 1 || p.Stop[0].Runner != "s0" {
		t.Fatalf("one past warm: %+v", p.Stop)
	}
}

// A waiting job of another class that fits once spare runners give up
// their room gets it in the same plan; runners a job is coming for do not.
func TestSpareRunnersGiveRoom(t *testing.T) {
	var rs []Runner
	for i := range 3 {
		rs = append(rs, idle(fmt.Sprintf("s%d", i), "linux-s", "server1", time.Minute, 1, gb))
	}
	// 1cpu/3G of the budget left; the linux-l job needs 2cpu/3G.
	p := Decide(warmSnap(3, demand("linux-l", 1), rs))
	if len(p.Stop) != 1 || !strings.Contains(p.Stop[0].Reason, "gives its room to a job of linux-l") {
		t.Fatalf("stops %+v", p.Stop)
	}
	if len(p.Start) != 1 || p.Start[0].Class != "linux-l" || !strings.Contains(p.Start[0].Reason, "once 1 spare runner(s) without a job give it up") {
		t.Fatalf("starts %+v", p.Start)
	}
	// warmMin 3: the class keeps them whatever waits.
	s := warmSnap(3, demand("linux-l", 1), rs)
	s.Classes[0].WarmMin = 3
	if p := Decide(s); len(p.Stop) != 0 || len(p.Start) != 0 {
		t.Fatalf("warmMin gave its room: stops %+v starts %v", p.Stop, starts(p))
	}
	// warmMin 2: one of the three may go.
	s.Classes[0].WarmMin = 2
	if p := Decide(s); len(p.Stop) != 1 || len(p.Start) != 1 {
		t.Fatalf("warmMin 2 of 3: stops %+v starts %v", p.Stop, starts(p))
	}
	// Three linux-s jobs wait: the runners are theirs, the linux-l job waits.
	p = Decide(warmSnap(3, append(demand("linux-s", 3), demand("linux-l", 1)...), rs))
	if len(p.Stop) != 0 || len(p.Start) != 0 || len(p.Defer) != 1 {
		t.Fatalf("runners of jobs: stops %+v starts %v defers %v", p.Stop, starts(p), defers(p))
	}
}

// A host that stops taking the class (its person is at it, its cpu is
// pressed) loses the spare runners of a warm class: GitHub would hand them
// the class's next job there.
func TestSpareWarmRunnerLeavesAClosedHost(t *testing.T) {
	h := desktop()
	h.Closed = "in use"
	s := snap([]Host{h}, nil, []Runner{idle("s1", "linux-s", "desk1", time.Minute, 1, gb)})
	s.Classes[0].Warm = 1
	p := Decide(s)
	if len(p.Stop) != 1 || !strings.Contains(p.Stop[0].Reason, "spare runner leaves: desk1 in use") {
		t.Fatalf("stops %+v", p.Stop)
	}
	if len(p.Start) != 0 {
		t.Fatalf("started on a closed host: %v", starts(p))
	}
	// Not a warm class: as before, the idle timeout decides.
	s.Classes[0].Warm = 0
	if p := Decide(s); len(p.Stop) != 0 {
		t.Fatalf("not warm: %+v", p.Stop)
	}
}

// The forecast plays the waiting jobs forward: a runner kept warm is no
// job's, and gives no time to anyone.
func TestForecastLeavesWarmOut(t *testing.T) {
	s := warmSnap(3, demand("linux-s", 1), nil)
	out := Forecast(s, nil, map[string]time.Duration{"linux-s-0": time.Minute}, 10*time.Second)
	if _, ok := out[""]; ok || len(out) != 1 {
		t.Fatalf("forecast %+v", out)
	}
}

// A runner whose worker runs has a job, whatever GitHub has said of it yet:
// it gives up no room.
func TestWorkingRunnerIsNoSpare(t *testing.T) {
	var rs []Runner
	for i := range 3 {
		r := idle(fmt.Sprintf("s%d", i), "linux-s", "server1", time.Minute, 1, gb)
		r.Working = true
		rs = append(rs, r)
	}
	if p := Decide(warmSnap(3, demand("linux-l", 1), rs)); len(p.Stop) != 0 || len(p.Start) != 0 {
		t.Fatalf("stops %+v starts %v", p.Stop, starts(p))
	}
}

// A runner kept by warmMin says so.
func TestWarmMinSaysSo(t *testing.T) {
	s := warmSnap(1, nil, nil)
	s.Classes[0].WarmMin = 1
	p := Decide(s)
	if len(p.Start) != 1 || !strings.Contains(p.Start[0].Reason, "(1 of 1, warmMin 1)") {
		t.Fatalf("%+v", p.Start)
	}
}
