package size

import "testing"

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
	if c, m := Charge(est, 0.3, 1*gb); c != 2 || m != 4*gb {
		t.Errorf("under the estimate: %v %v", c, m)
	}
	if c, m := Charge(est, 5.5, 9*gb); c != 5.5 || m != 9*gb {
		t.Errorf("over the estimate: %v %v", c, m)
	}
}
