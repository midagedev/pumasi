package race

import (
	"encoding/json"
	"testing"
)

// The same scenario gives the same numbers: the film's can be made again.
func TestRunIsDeterministic(t *testing.T) {
	a, _ := json.Marshal(Run(Film))
	b, _ := json.Marshal(Run(Film))
	if string(a) != string(b) {
		t.Fatal("two runs of one scenario differ")
	}
}

// Each setup runs every job once, never before it arrived, and ends with none waiting.
func TestEverySetupRunsEveryJob(t *testing.T) {
	r := Run(Film)
	if len(r.Runs) != 5 {
		t.Fatalf("%d runs, want 5", len(r.Runs))
	}
	for _, x := range r.Runs {
		last := x.Frames[len(x.Frames)-1]
		if last.Done != len(r.Jobs) || len(last.Waiting) != 0 {
			t.Fatalf("%s: last frame done %d of %d, %d waiting", x.Name, last.Done, len(r.Jobs), len(last.Waiting))
		}
		for i, w := range x.Waits {
			if w < 0 {
				t.Fatalf("%s: job %d started before it came", x.Name, r.Jobs[i].ID)
			}
		}
		// A job runs on one host, in one stretch of frames.
		where := map[int]string{}
		ended := map[int]bool{}
		for _, f := range x.Frames {
			seen := map[int]bool{}
			for _, h := range f.Hosts {
				for _, j := range h.Jobs {
					if j.ID == 0 {
						continue
					}
					if ended[j.ID] || (where[j.ID] != "" && where[j.ID] != h.Name) || seen[j.ID] {
						t.Fatalf("%s: job %d runs twice or moves (t=%g)", x.Name, j.ID, f.T)
					}
					where[j.ID], seen[j.ID] = h.Name, true
				}
			}
			for id := range where {
				if !seen[id] {
					ended[id] = true
				}
			}
		}
		if len(where) != len(r.Jobs) {
			t.Fatalf("%s: %d jobs ran, want %d", x.Name, len(where), len(r.Jobs))
		}
	}
}

// Neither setup runs a host out of memory: the fixed runners are sized so
// that they cannot, and pumasi places within what is free.
func TestNoHostRunsOutOfMemory(t *testing.T) {
	r := Run(Film)
	room := map[string]float64{}
	for _, h := range r.Hosts {
		room[h.Name] = h.Mem
	}
	for _, f := range r.Fixed {
		held := map[string]float64{}
		for _, s := range f.Slots {
			held[s.Host] += s.Mem
		}
		for h, m := range held {
			if m > room[h]+1e-9 {
				t.Fatalf("%s: runners of %s are sized for %.1fG, room %.1fG", f.Name, h, m, room[h])
			}
		}
	}
	for _, x := range r.Runs {
		for _, f := range x.Frames {
			for _, h := range f.Hosts {
				var used float64
				for _, j := range h.Jobs {
					used += j.Mem
				}
				if used > room[h.Name]+1e-9 {
					t.Fatalf("%s: %s uses %.1fG at %gs, room %.1fG", x.Name, h.Name, used, f.T, room[h.Name])
				}
			}
		}
	}
}
