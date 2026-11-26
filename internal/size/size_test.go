package size

import (
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
