package race

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// Drifts are the changes DriftTable puts the setups through, one at a time.
var Drifts = []Drift{
	{Name: "jobs grow 30%", Grow: 1.3},
	{Name: "heavy jobs every push", Heavy: 1},
	{Name: "a new 5-7G pr check", New: true},
	{Name: "nightly batches", Nightly: true},
}

// DriftRun is sc after its drift on the fixed runners sized before it (by
// the stream of sc.Before()), on per-class runners sized again by the
// stream after it (the one setup that saw the change), and on pumasi, which
// measured the jobs as they were before.
func DriftRun(sc Scenario) []Result {
	before, after := Stream(sc.Before()), Stream(sc)
	refit := PerClass(after)
	refit.Name = "fixed-per-class, sized again"
	return []Result{
		RunFixed(PerClass(before), after),
		RunFixed(Split(before), after),
		RunFixed(Uniform(), after),
		RunFixed(refit, after),
		RunPumasi(sc, after),
	}
}

// DriftTable is DriftRun for each of Drifts and for none, at two paces,
// averaged over seeds 1-10.
func DriftTable() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pushes every\tchange\tsetup\tmedian wait\tp90 wait\tmax wait\tjobs past their runner's memory\tmost host memory\tsecs over a host's memory\t")
	for _, every := range [][2]float64{{35, 55}, {25, 40}} {
		for _, d := range append([]Drift{{Name: "none"}}, Drifts...) {
			sum := map[string]*Summary{}
			var names []string
			for seed := uint64(1); seed <= 10; seed++ {
				sc := Film
				sc.Seed, sc.Every, sc.Drift = seed, every, d
				for _, x := range DriftRun(sc) {
					t := sum[x.Name]
					if t == nil {
						t = &Summary{}
						sum[x.Name] = t
						names = append(names, x.Name)
					}
					s := x.Summary
					t.MedianWait += s.MedianWait / 10
					t.P90Wait += s.P90Wait / 10
					t.MaxWait += s.MaxWait / 10
					t.OverRunner += s.OverRunner
					t.HostMemPeak = max(t.HostMemPeak, s.HostMemPeak)
					t.HostOverSecs += s.HostOverSecs / 10
				}
			}
			for _, n := range names {
				t := sum[n]
				fmt.Fprintf(w, "%g-%gs\t%s\t%s\t%s\t%s\t%s\t%.1f\t%.0f%%\t%.0f\t\n", every[0], every[1], d.Name, n, mmss(t.MedianWait), mmss(t.P90Wait), mmss(t.MaxWait),
					float64(t.OverRunner)/10, 100*t.HostMemPeak, t.HostOverSecs)
			}
		}
	}
	w.Flush()
	b.WriteString("(seeds 1-10; fixed runners sized by the stream before the change, pumasi measured it; jobs past memory: per run; most host memory: the highest of any seed, share of the host's room)\n")
	return b.String()
}
