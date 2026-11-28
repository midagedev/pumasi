package synth

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
)

// Whatever day the demo is opened, the changes it shows are the ones it was
// made with, each named by its own commit, and the creep by no commit.
func TestDemoTellsTheTruthEveryDay(t *testing.T) {
	monday := time.Date(2026, 11, 23, 0, 0, 0, 0, time.Local)
	for d := 0; d < 7; d++ {
		for _, hour := range []int{1, 13} {
			now := monday.AddDate(0, 0, d).Add(time.Duration(hour) * time.Hour)
			h := Demo(now)
			truth := map[string]Injected{}
			for _, in := range h.Injected {
				truth[in.Job] = in
			}
			found := map[string]bool{}
			for _, c := range watch.Find(h.Runs, watch.Options{}) {
				in, ok := truth[c.Job]
				switch {
				case !ok:
					t.Errorf("%s: a change nobody made: %s %s %+.2f", now.Format("Mon 15h"), c.Job, c.Metric, c.Ratio)
				case in.Creep > 0 && !c.Gradual:
					t.Errorf("%s: %s crept, but commit %s is named", now.Format("Mon 15h"), c.Job, c.First.SHA[:7])
				case in.Creep == 0 && c.First.SHA != in.SHA:
					t.Errorf("%s: %s %s since %s, made by %s", now.Format("Mon 15h"), c.Job, c.Metric, c.First.SHA[:7], in.SHA[:7])
				default:
					found[c.Job] = true
				}
			}
			for job, in := range truth {
				if in.Creep == 0 && !found[job] {
					t.Errorf("%s: %s not found", now.Format("Mon 15h"), in.Label)
				}
			}
		}
	}
}

// The demo's jobs end its history as DemoJobs say (the demo dashboard's),
// and demoAtStart is what working them back gives.
func TestDemoJobsWorkedBack(t *testing.T) {
	now := DemoNow(Generate(demoConfig(demoJobsAtStart())))
	for _, j := range DemoJobs {
		got := now[j.Name]
		if math.Abs(got[0]/j.Sec-1) > 0.02 || math.Abs(got[1]/j.MemGB-1) > 0.02 {
			t.Errorf("%s ends at %.0fs %.2fG, want %.0fs %.2fG", j.Name, got[0], got[1], j.Sec, j.MemGB)
		}
	}
	var table strings.Builder
	moved := false
	for _, j := range demoWorkBack() {
		v := demoAtStart[j.Name]
		fmt.Fprintf(&table, "\t%q: {%.4g, %.4g},\n", j.Name, j.Sec, j.MemGB)
		if math.Abs(j.Sec/v[0]-1) > 0.005 || math.Abs(j.MemGB/v[1]-1) > 0.005 {
			moved = true
		}
	}
	if moved {
		t.Errorf("demoAtStart moved; worked back again:\n%s", table.String())
	}
}
