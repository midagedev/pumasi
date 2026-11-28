package synth

import (
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
)

// ForecastCase is one history of a growing team: the forecast made after
// Base weeks, and what came of it in the weeks after.
type ForecastCase struct {
	Growth, GrowthSeen float64 // the merge rate's, and the one the forecast measured
	Cores              float64
	LoadSeen           float64
	// Predicted (with its interval) and actual weeks after Base (NaN = not
	// within the history).
	FullIn, FullSoon, FullLate, FullActual         float64
	DoubleIn, DoubleSoon, DoubleLate, DoubleActual float64
	// The share of jobs that waited a minute or more for room in the last
	// week read: the forecast's replay, and what the queue did.
	WaitOdds, WaitShare float64
}

// ForecastEval: weeks of history, the forecast after Base of them, the
// cores set so the busiest hours of the first week are at Load.
type ForecastEval struct {
	Histories int
	Weeks     int     // default 16
	Base      int     // default 8
	Growth    float64 // default 0.08 a week
	Load      float64 // default 0.35
	Merges    float64 // a weekday's merges at the start (default 8)
}

// EvalForecast makes each history twice, with no limit on cores (the demand)
// and with Cores (the waits), and sets the forecast against both.
func EvalForecast(fe ForecastEval) []ForecastCase {
	fe.Weeks, fe.Base = cmp(fe.Weeks, 16), cmp(fe.Base, 8)
	if fe.Growth == 0 {
		fe.Growth = 0.08
	}
	if fe.Load == 0 {
		fe.Load = 0.35
	}
	var out []ForecastCase
	for i := range fe.Histories {
		cfg := Config{Seed: uint64(5_000_000 + i), Start: EvalStart, Days: 7 * fe.Weeks, Growth: fe.Growth, Merges: fe.Merges}
		free := Generate(cfg)
		end := EvalStart.AddDate(0, 0, 7*fe.Weeks)
		demand := watch.Forecast(free.Runs, 0, end, fe.Weeks).Weeks
		cfg.Cores = demand[0].PeakCores / fe.Load
		h := Generate(cfg)
		at := EvalStart.AddDate(0, 0, 7*fe.Base)
		f := watch.Forecast(h.Runs, cfg.Cores, at, fe.Base)
		c := ForecastCase{Growth: fe.Growth, GrowthSeen: f.Growth, Cores: cfg.Cores, LoadSeen: f.Load,
			FullIn: f.FullIn, FullSoon: f.FullSoon, FullLate: f.FullLate, DoubleIn: f.DoubleIn, DoubleSoon: f.DoubleSoon, DoubleLate: f.DoubleLate, WaitOdds: f.WaitShare}
		// What came: by week, the busiest hours' demand, and the waits for
		// room (the wait less its runner's start, which the free history has).
		peak := make([]float64, fe.Weeks)
		share := make([]float64, fe.Weeks)
		mean := make([]float64, fe.Weeks)
		start := map[string]float64{}
		for _, r := range free.Runs {
			start[r.Runner] = r.WaitSec
		}
		n := make([]int, fe.Weeks)
		waited := make([]int, fe.Weeks)
		for _, r := range h.Runs {
			w := int(r.QueuedAt.Sub(EvalStart) / (7 * 24 * time.Hour))
			if w < 0 || w >= fe.Weeks {
				continue
			}
			d := r.WaitSec - start[r.Runner]
			n[w]++
			mean[w] += d
			if d >= 60 {
				waited[w]++
			}
		}
		for w := range fe.Weeks {
			peak[w] = demand[w].PeakCores
			if n[w] > 0 {
				mean[w] /= float64(n[w])
				share[w] = float64(waited[w]) / float64(n[w])
			}
		}
		// The weeks smoothed (each with its neighbours), so a forecast of the
		// trend is not scored on one week's luck.
		b := fe.Base - 1
		base := mean[b] // the week the forecast saw
		peak, mean = smooth(peak), smooth(mean)
		mean[b] = base
		c.FullActual = crossing(peak, b, cfg.Cores)
		c.WaitShare = share[b]
		if base >= 5 {
			c.DoubleActual = crossing(mean, b, 2*mean[b])
		} else {
			c.DoubleActual = math.NaN()
		}
		out = append(out, c)
	}
	return out
}

// smooth: each week the mean of it and the weeks beside it.
func smooth(x []float64) []float64 {
	out := make([]float64, len(x))
	for i := range x {
		sum, n := 0.0, 0
		for k := max(0, i-1); k <= min(len(x)-1, i+1); k++ {
			sum += x[k]
			n++
		}
		out[i] = sum / float64(n)
	}
	return out
}

// crossing: weeks after week b until the series first reaches v,
// interpolated between weeks; 0 if it already has; NaN if it never does.
func crossing(x []float64, b int, v float64) float64 {
	if x[b] >= v {
		return 0
	}
	for w := b + 1; w < len(x); w++ {
		if x[w] >= v {
			f := 1.0
			if x[w] > x[w-1] {
				f = (v - x[w-1]) / (x[w] - x[w-1])
			}
			return float64(w-1-b) + f
		}
	}
	return math.NaN()
}

// WriteForecastReport prints EvalForecast's cases and how far off they were.
func WriteForecastReport(w io.Writer, fe ForecastEval, cs []ForecastCase) {
	fmt.Fprintf(w, "capacity forecast on %d made-up histories of a team whose merges (%.0f a weekday at first) grow %.0f%% a week: %d weeks, the forecast after %d, the cores set so the first week's busiest hours are at %.0f%%\n\n",
		len(cs), cmpOrF(fe.Merges, 8), 100*cmpOrF(fe.Growth, 0.08), cmp(fe.Weeks, 16), cmp(fe.Base, 8), 100*cmpOrF(fe.Load, 0.35))
	var seen []float64
	for _, c := range cs {
		seen = append(seen, c.GrowthSeen)
	}
	slices.Sort(seen)
	fmt.Fprintf(w, "growth measured from %d weeks: median %+.1f%% a week (p10 %+.1f%%, p90 %+.1f%%)\n\n", cmp(fe.Base, 8),
		100*watch.Percentile(seen, 0.5), 100*watch.Percentile(seen, 0.1), 100*watch.Percentile(seen, 0.9))
	horizon := float64(cmp(fe.Weeks, 16) - cmp(fe.Base, 8))
	row := func(name string, pred func(ForecastCase) (p, soon, late float64), actOf func(ForecastCase) float64) {
		act := actOf
		var errs, abs, rel []float64
		within, covered, scored, beyond, unscored := 0, 0, 0, 0, 0
		for _, c := range cs {
			p, soon, late := pred(c)
			a := act(c)
			if late < 0 {
				late = math.Inf(1)
			}
			switch {
			case math.IsNaN(a) && (p < 0 || p > horizon):
				beyond++ // neither within the history: they agree
				if soon < 0 || soon > horizon {
					covered++
				}
				continue
			case math.IsNaN(a):
				unscored++ // forecast within the history, came later
				if late > horizon {
					covered++
				}
				continue
			case p < 0:
				unscored++
				continue
			}
			scored++
			errs = append(errs, p-a)
			abs = append(abs, math.Abs(p-a))
			if a > 0 {
				rel = append(rel, math.Abs(p-a)/a)
			}
			if math.Abs(p-a) <= 1 {
				within++
			}
			if soon <= a && a <= late {
				covered++
			}
		}
		all := scored + beyond + unscored
		if scored == 0 {
			fmt.Fprintf(w, "%-32s nothing to score\n", name)
			return
		}
		slices.Sort(abs)
		slices.Sort(errs)
		slices.Sort(rel)
		var came []float64
		for _, c := range cs {
			if a := actOf(c); !math.IsNaN(a) {
				came = append(came, a)
			}
		}
		slices.Sort(came)
		fmt.Fprintf(w, "%s: came within the history in %d of %d (median %.1f weeks after the forecast)\n  off by %.1f weeks (median), %.0f%% of the time to it; bias %+.1f; within a week %.0f%%\n  inside the 80%% interval %.0f%% of all %d (%d forecast within the history but came after; %d forecast and came after)\n",
			name, scored, all, watch.Percentile(came, 0.5), watch.Percentile(abs, 0.5), 100*watch.Percentile(rel, 0.5), watch.Percentile(errs, 0.5),
			100*float64(within)/float64(scored), 100*float64(covered)/float64(all), all, unscored, beyond)
	}
	row("busiest hours need every core", func(c ForecastCase) (float64, float64, float64) { return c.FullIn, c.FullSoon, c.FullLate },
		func(c ForecastCase) float64 { return c.FullActual })
	row("the mean wait for room doubles", func(c ForecastCase) (float64, float64, float64) { return c.DoubleIn, c.DoubleSoon, c.DoubleLate },
		func(c ForecastCase) float64 { return c.DoubleActual })
	var odds, share, diff []float64
	for _, c := range cs {
		odds = append(odds, c.WaitOdds)
		share = append(share, c.WaitShare)
		diff = append(diff, math.Abs(c.WaitOdds-c.WaitShare))
	}
	slices.Sort(odds)
	slices.Sort(share)
	slices.Sort(diff)
	fmt.Fprintf(w, "jobs waiting a minute or more for room in week %d: the forecast's replay median %.0f%%, the queue's %.0f%%, off by %.0f points (median)\n",
		cmp(fe.Base, 8), 100*watch.Percentile(odds, 0.5), 100*watch.Percentile(share, 0.5), 100*watch.Percentile(diff, 0.5))
}

func pctOr(rel []float64) float64 {
	if len(rel) == 0 {
		return 0
	}
	return watch.Percentile(rel, 0.5)
}

func cmpOrF(a, b float64) float64 {
	if a == 0 {
		return b
	}
	return a
}
