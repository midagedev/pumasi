package race

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
)

// ETAStats is how well pumasi's forecast of each waiting job's start
// (decide.Forecast) matched when it did start. Only what was said while the
// job still had at least MinLead to wait counts: a forecast of the next few
// seconds is right by construction.
type ETAStats struct {
	Waited int // jobs that waited at least MinLead
	Told   int // of those, the ones given a time the first time they were seen waiting
	// First: the error of that first time, seconds: median, p90 of its size,
	// and the share within max(60s, 30% of the wait it foretold).
	FirstMedian, FirstP90, InBand float64
	// Bias: the mean signed error of the first time (< 0: it said too early).
	Bias float64
	// Rolling: every time it was said, each round.
	Rolling                   int
	RollingMedian, RollingP90 float64
}

// MinLead: forecasts of a start nearer than this are left out of ETAStats.
const MinLead = 20.0

// ETA measures the forecast over seeds 1..seeds of sc.
func ETA(sc Scenario, seeds int) ETAStats {
	var st ETAStats
	var first, rolling []float64
	var band int
	for seed := 1; seed <= seeds; seed++ {
		sc.Seed = uint64(seed)
		jobs := Stream(sc)
		x := RunPumasi(sc, jobs)
		told := map[int]bool{}
		for _, tl := range x.Told {
			start := x.Arrived[tl.Job] + x.Waits[tl.Job]
			lead := start - tl.At
			if lead < MinLead {
				continue
			}
			e := tl.Starts - start
			rolling = append(rolling, math.Abs(e))
			if tl.First {
				told[tl.Job] = true
				first = append(first, math.Abs(e))
				st.Bias += e
				if math.Abs(e) <= math.Max(60, 0.3*lead) {
					band++
				}
			}
		}
		for i, w := range x.Waits {
			if w >= MinLead {
				st.Waited++
				if told[i] {
					st.Told++
				}
			}
		}
	}
	if len(first) > 0 {
		st.FirstMedian, st.FirstP90 = quantile(first, 0.5), quantile(first, 0.9)
		st.InBand, st.Bias = float64(band)/float64(len(first)), st.Bias/float64(len(first))
	}
	if len(rolling) > 0 {
		st.Rolling, st.RollingMedian, st.RollingP90 = len(rolling), quantile(rolling, 0.5), quantile(rolling, 0.9)
	}
	return st
}

// ETATable is ETA at three paces, with and without long-tailed run times.
func ETATable() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pushes every\trun times\twaited\tgiven a time\tfirst: median error\tp90 error\twithin 60s/30%\tbias\trolling: median\tp90\t")
	for _, tails := range []bool{false, true} {
		for _, every := range [][2]float64{{12, 20}, {25, 40}, {35, 55}} {
			sc := Film
			sc.Every, sc.Tails = every, tails
			st := ETA(sc, 10)
			kind := slices.Index([]bool{false, true}, tails)
			fmt.Fprintf(w, "%g-%gs\t%s\t%d\t%d\t%.0fs\t%.0fs\t%.0f%%\t%+.0fs\t%.0fs\t%.0fs\t\n", every[0], every[1], []string{"even", "long tail"}[kind],
				st.Waited, st.Told, st.FirstMedian, st.FirstP90, 100*st.InBand, st.Bias, st.RollingMedian, st.RollingP90)
		}
	}
	w.Flush()
	fmt.Fprintf(&b, "(seeds 1-10; a job counts if it waited %.0fs or more; long tail: 1 run in 10 takes twice as long, 1 in 20 fails a third in)\n", MinLead)
	return b.String()
}
