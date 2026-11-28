package size

import (
	"strings"
	"testing"
	"time"
)

const gb = 1 << 30

func TestLearn(t *testing.T) {
	first := Estimate{CPUs: 2, MemBytes: 4 * gb}
	tests := []struct {
		name string
		runs []Sample
		cpus float64
		mem  int64
	}{
		{"a job never seen takes the class's first estimate", nil, 2, 4 * gb},
		{"one run: that run and a margin", []Sample{{CPUs: 1, MemBytes: 5 * gb}}, 1.2, 6 * gb},
		{"the largest of the window counts, not the newest", []Sample{{CPUs: 0.5, MemBytes: 1 * gb}, {CPUs: 3, MemBytes: 10 * gb}, {CPUs: 0.5, MemBytes: 1 * gb}}, 3.6, 12 * gb},
		{"a run cut short at its limit asks for more than it was seen with", []Sample{{CPUs: 1, MemBytes: 8 * gb, OOM: true}}, 1.2, 12 * gb},
		{"a tiny job still gets a floor", []Sample{{CPUs: 0.01, MemBytes: 20 << 20}}, MinCPUs, MinMem},
		{"memory is rounded up to 64M", []Sample{{CPUs: 1, MemBytes: 1000 << 20}}, 1.2, 1216 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Learn(tt.runs, first)
			if e.CPUs != tt.cpus || e.MemBytes != tt.mem || e.Runs != len(tt.runs) {
				t.Errorf("got %.2fcpu/%d from %d runs, want %.2fcpu/%d", e.CPUs, e.MemBytes, e.Runs, tt.cpus, tt.mem)
			}
		})
	}
}

// The estimate follows a job that grows, run by run, and comes back down
// only when the big runs have left the window.
func TestEstimateFollowsAJobThatGrows(t *testing.T) {
	first := Estimate{CPUs: 2, MemBytes: 4 * gb}
	var runs []Sample
	prev := int64(0)
	for i := 1; i <= 6; i++ {
		runs = append([]Sample{{CPUs: 1, MemBytes: int64(i) * gb}}, runs...)
		e := Learn(runs, first)
		if e.MemBytes <= prev || e.MemBytes < int64(i)*gb {
			t.Fatalf("after a %dG run the estimate is %d (before %d)", i, e.MemBytes, prev)
		}
		prev = e.MemBytes
	}
	for range Window {
		runs = append([]Sample{{CPUs: 1, MemBytes: gb}}, runs...)
		runs = runs[:min(len(runs), Window)]
	}
	if e := Learn(runs, first); e.MemBytes != 1280<<20 {
		t.Fatalf("after %d small runs the estimate is still %d", Window, e.MemBytes)
	}
}

func TestChargeIsTheLargerOfEstimateAndUse(t *testing.T) {
	est := Estimate{CPUs: 2, MemBytes: 4 * gb}
	if c, m := Charge(est, 0.3, 1*gb, false); c != 2 || m != 4*gb {
		t.Errorf("under the estimate: %v %v", c, m)
	}
	if c, m := Charge(est, 5.5, 9*gb, false); c != 5.5 || m != 9*gb {
		t.Errorf("over the estimate: %v %v", c, m)
	}
}

func TestBlindJobHoldsMoreUntilItHasRun(t *testing.T) {
	est := Estimate{CPUs: 2, MemBytes: 4 * gb, Blind: true}
	now := time.Unix(1_000_000, 0)
	if !est.Watched(time.Time{}, now) || !est.Watched(now.Add(-Settle+time.Second), now) {
		t.Fatal("a blind job is watched until it has run for Settle")
	}
	if est.Watched(now.Add(-Settle), now) {
		t.Fatal("after Settle it is not watched")
	}
	if (Estimate{MemBytes: 4 * gb}).Watched(time.Time{}, now) {
		t.Fatal("only a blind estimate is watched")
	}
	// Memory is held at BlindHold, cpu is not; real use above the hold still counts.
	if c, m := Charge(est, 0.3, 1*gb, true); c != 2 || m != BlindHold*4*gb {
		t.Errorf("watched: %v %v", c, m)
	}
	if _, m := Charge(est, 0.3, 20*gb, true); m != 20*gb {
		t.Errorf("watched and over the hold: %v", m)
	}
}

func TestBorrowIsNeverUnderTheFirstEstimate(t *testing.T) {
	first := Estimate{CPUs: 2, MemBytes: 4 * gb}
	big := Borrow(first, Learn([]Sample{{CPUs: 1, MemBytes: 9 * gb}, {CPUs: 3, MemBytes: 1 * gb}}, Estimate{}))
	if big.MemBytes < 10*gb || big.CPUs < 3.6 || !big.Borrowed || big.Blind || big.Runs != 0 {
		t.Errorf("borrowed from a larger workflow: %+v", big)
	}
	small := Borrow(first, Learn([]Sample{{CPUs: 0.1, MemBytes: gb / 4}}, Estimate{}))
	if small.MemBytes != 4*gb || small.CPUs != 2 {
		t.Errorf("borrowed from a smaller workflow: %+v", small)
	}
}

// A run that held more memory than its host has is a measurement that counted
// shared memory once per process; it is left out, and the estimate says so.
// A run cut short at its limit counts, at most at the host's memory.
func TestLearnLeavesOutARunAboveItsHostsMemory(t *testing.T) {
	first := Estimate{CPUs: 2, MemBytes: 4 * gb}
	runs := []Sample{
		{CPUs: 6, MemBytes: 113 * gb, Host: "mac-a", HostMem: 32 * gb},
		{CPUs: 5, MemBytes: 10 * gb, Host: "mac-a", HostMem: 32 * gb},
	}
	e := Learn(runs, first)
	if e.MemBytes != 12*gb || e.CPUs != 6 || e.Runs != 1 || e.Ignored != 1 || e.TopMem != 10*gb {
		t.Fatalf("with a run of 113G on a 32G host: %+v", e)
	}
	if src := e.Source(); !strings.Contains(src, "1 run that held more than its host's memory left out") {
		t.Fatalf("source %q", src)
	}
	// Where the host's memory is not known, the run counts as it is.
	if e := Learn([]Sample{{CPUs: 1, MemBytes: 113 * gb}}, first); e.MemBytes < 113*gb {
		t.Fatalf("host memory unknown: %+v", e)
	}
	// Only such runs: the class's first estimate.
	if e := Learn(runs[:1], first); e.MemBytes != first.MemBytes || e.Runs != 0 || e.Ignored != 1 {
		t.Fatalf("only a run above its host: %+v", e)
	}
	// Cut short at a limit wider than the host: what the host has, with the OOM margin.
	if e := Learn([]Sample{{CPUs: 1, MemBytes: 40 * gb, OOM: true, HostMem: 32 * gb}}, first); e.MemBytes != 48*gb || e.Ignored != 0 {
		t.Fatalf("oom above the host: %+v", e)
	}
}

func TestRuntimeIsTheMedian(t *testing.T) {
	if _, ok := Runtime([]Sample{{CPUs: 1}}); ok {
		t.Fatal("a run without a time gave a runtime")
	}
	d, ok := Runtime([]Sample{{Secs: 300}, {Secs: 60}, {Secs: 90}, {}})
	if !ok || d != 90*time.Second {
		t.Fatalf("got %v %v, want 1m30s", d, ok)
	}
	if d, _ := Runtime([]Sample{{Secs: 60}, {Secs: 120}}); d != 90*time.Second {
		t.Fatalf("even count: %v", d)
	}
}

// A job past its class's hard memory limit is held at the limit: room past it
// would only be held, since the job is killed at the limit anyway.
func TestAtLimit(t *testing.T) {
	e := Learn([]Sample{{CPUs: 1, MemBytes: 20 * gb, OOM: true}}, Estimate{})
	if e.MemBytes <= 20*gb {
		t.Fatalf("a run cut short at 20G should ask for more, got %d", e.MemBytes)
	}
	want := e.MemBytes
	c := e.AtLimit(20 * gb)
	if c.MemBytes != 20*gb || c.Over != want {
		t.Fatalf("at a 20G limit: %d (over %d), want 20G (over %d)", c.MemBytes, c.Over, want)
	}
	if !strings.Contains(c.Source(), "held at its class's memory limit 20.0G") {
		t.Errorf("source does not say so: %s", c.Source())
	}
	if n := e.AtLimit(0); n.MemBytes != want || n.Over != 0 {
		t.Errorf("no limit: %d over %d", n.MemBytes, n.Over)
	}
	if n := e.AtLimit(64 * gb); n.MemBytes != want || n.Over != 0 {
		t.Errorf("a limit above it: %d over %d", n.MemBytes, n.Over)
	}
}
