package race

import (
	"fmt"
	"math"
	"strings"
	"text/tabwriter"
)

// Setup is one way to run a stream: a name, and the run.
type Setup struct {
	Name string
	Run  func(sc Scenario, jobs []Job) Result
}

// Setups are the ones LabTable compares: the two tuned fixed setups, and
// pumasi without runners kept warm and as configured.
var Setups = []Setup{
	{"fixed-per-class", func(sc Scenario, jobs []Job) Result { return RunFixed(PerClass(jobs), jobs) }},
	{"fixed-split", func(sc Scenario, jobs []Job) Result { return RunFixed(Split(jobs), jobs) }},
	{"pumasi-nowarm", func(sc Scenario, jobs []Job) Result { sc.Warm = 0; return RunPumasi(sc, jobs) }},
	{"pumasi", func(sc Scenario, jobs []Job) Result { return RunPumasi(sc, jobs) }},
	{"pumasi-nowarm+ahead", func(sc Scenario, jobs []Job) Result { sc.Warm, sc.Ahead = 0, true; return RunPumasi(sc, jobs) }},
	{"pumasi+ahead", func(sc Scenario, jobs []Job) Result { sc.Ahead = true; return RunPumasi(sc, jobs) }},
}

// Variant is a way the hosts come up and the jobs come.
type Variant struct {
	Name string
	Set  func(*Scenario)
}

// Variants: the 8s a runner took in the first runs; runners coming up as
// native runners did on real hosts; and that, with jobs that need others
// coming when those end.
var Variants = []Variant{
	{"8s", func(*Scenario) {}},
	{"native", func(sc *Scenario) { sc.Native, sc.Provision = true, 4.8 }},
	{"native, needs", func(sc *Scenario) { sc.Native, sc.Provision, sc.Needs = true, 4.8, true }},
}

// LabRow is a setup's figures in one variant at one pace, averaged over seeds.
type LabRow struct {
	Pace, Variant, Setup                string
	Median, P90, Max                    float64
	NeedsMedian, NeedsP90               float64 // jobs that came after the ones they need
	HeldMem, Gone, Delayed, DelayedSecs float64 // pumasi's idle runners, per run
	OOM, PressSecs                      float64
	AheadStarted, AheadServed           float64 // runners started ahead, and of them taken by a job, per run
}

// Lab runs each setup in each variant at the given paces over seeds 1..n.
func Lab(setups []Setup, variants []Variant, paces [][2]float64, n int) []LabRow {
	var out []LabRow
	for _, every := range paces {
		for _, v := range variants {
			for _, st := range setups {
				row := LabRow{Pace: fmt.Sprintf("%g-%gs", every[0], every[1]), Variant: v.Name, Setup: st.Name}
				var needs []float64
				for seed := 1; seed <= n; seed++ {
					sc := Film
					sc.Seed, sc.Every = uint64(seed), every
					v.Set(&sc)
					jobs := Stream(sc)
					x := st.Run(sc, jobs)
					f := 1 / float64(n)
					row.Median += x.Summary.MedianWait * f
					row.P90 += x.Summary.P90Wait * f
					row.Max += x.Summary.MaxWait * f
					row.HeldMem += x.Idle.HeldMem * f
					row.Gone += float64(x.Idle.Stopped) * f
					row.Delayed += float64(x.Idle.Delayed) * f
					row.DelayedSecs += x.Idle.DelayedSecs * f
					row.OOM += float64(x.Summary.OOM) * f
					row.PressSecs += x.Summary.MemPressSecs * f
					row.AheadStarted += float64(x.Idle.AheadStarted) * f
					row.AheadServed += float64(x.Idle.AheadServed) * f
					var w []float64
					for i, j := range jobs {
						if j.After > 0 {
							w = append(w, x.Waits[i])
						}
					}
					if len(w) > 0 {
						needs = append(needs, quantile(w, 0.5), quantile(w, 0.9))
					}
				}
				for i := 0; i+1 < len(needs); i += 2 {
					row.NeedsMedian += needs[i] / float64(len(needs)/2)
					row.NeedsP90 += needs[i+1] / float64(len(needs)/2)
				}
				out = append(out, row)
			}
		}
	}
	return out
}

// LabTable is Lab of Setups in Variants at the busy and the film's pace, seeds 1-10.
func LabTable() string { return labTable(Lab(Setups, Variants, [][2]float64{{35, 55}, {25, 40}}, 10)) }

func labTable(rows []LabRow) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pushes every\trunners come up\tsetup\tmedian wait\tp90\tmax\tneeds: median\tp90\tidle mem held\tgone without a job\tjobs warm delayed\tsecs\tahead started\ttaken\toom\tmem pressure secs\t")
	for _, r := range rows {
		nm, np := "-", "-"
		if r.NeedsMedian+r.NeedsP90 > 0 || strings.Contains(r.Variant, "needs") {
			nm, np = mmss(r.NeedsMedian), mmss(r.NeedsP90)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.1fG\t%.1f\t%.1f\t%.0f\t%.1f\t%.1f\t%.1f\t%.0f\t\n", r.Pace, r.Variant, r.Setup, mmss(r.Median), mmss(r.P90), mmss(r.Max),
			nm, np, r.HeldMem, r.Gone, r.Delayed, math.Round(r.DelayedSecs), r.AheadStarted, r.AheadServed, r.OOM, r.PressSecs)
	}
	w.Flush()
	b.WriteString("(seeds 1-10; needs: the jobs that came when the ones they need ended; idle, gone, delayed: pumasi's runners without a job, per run)\n")
	return b.String()
}
