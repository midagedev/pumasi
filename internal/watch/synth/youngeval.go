package synth

import (
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
)

// Young evaluation: how the history rule (Options.MinBefore, MinAfter) trades
// false changes in a young ledger against how soon a real one is listed.

const day = 24 * time.Hour

// YoungAges are the ledger ages scored, YoungSides the spans asked of the
// sides before and after (-1: none), YoungSoon how soon a change is looked for.
var (
	YoungAges  = []time.Duration{30 * time.Hour, 2 * day, 3 * day, 5 * day, 7 * day, 14 * day, 28 * day}
	YoungSides = [][2]time.Duration{{-1, -1}, {12 * time.Hour, 12 * time.Hour}, {day, day}, {2 * day, 2 * day}, {day, -1}, {2 * day, -1}, {3 * day, -1}, {2 * day, 12 * time.Hour}}
	YoungSoon  = []time.Duration{day, 2 * day, 3 * day, 7 * day}
)

// YoungResult is one side span's numbers.
type YoungResult struct {
	Side [2]time.Duration // before, after
	// Falsely: of histories with nothing injected, cut at each of YoungAges,
	// the share with a job's change listed.
	Falsely []float64
	// Soon: of histories with a job +20% (and +40%), the share where it was
	// listed within each of YoungSoon of its merge.
	Soon20, Soon40 []float64
}

// EvalYoung scores each of YoungSides.
func EvalYoung(ec EvalConfig) []YoungResult {
	type hist struct {
		h  History
		in Injected
		sc Scenario
	}
	noise := make([]History, ec.Histories)
	for i := range noise {
		noise[i] = Generate(Config{Seed: uint64(1_000_000 + i), Start: EvalStart, Days: 28, Merges: ec.Merges})
	}
	var inj []hist
	for si, sc := range Scenarios[:2] { // time +20%, +40%: the same histories as Eval's
		for i := range ec.Histories {
			seed := uint64(si*100_000 + i + 1)
			rng := rand.New(rand.NewPCG(seed, 7))
			var jobs []Job
			for _, j := range Shop {
				if !j.Nightly {
					jobs = append(jobs, j)
				}
			}
			job := jobs[rng.IntN(len(jobs))]
			after := time.Duration(14*24+rng.IntN(7*24)) * time.Hour
			h := Generate(Config{Seed: seed, Start: EvalStart, Days: 28, Merges: ec.Merges,
				Inject: []Planned{{Change: Change{Job: job.Name, Metric: sc.Metric, Factor: sc.Factor}, After: after}}})
			if len(h.Injected) > 0 {
				inj = append(inj, hist{h, h.Injected[0], sc})
			}
		}
	}
	var out []YoungResult
	for _, side := range YoungSides {
		o := ec.Options
		o.MinBefore, o.MinAfter = side[0], side[1]
		r := YoungResult{Side: side}
		for _, age := range YoungAges {
			n := 0
			for _, h := range noise {
				if len(watch.Find(until(h.Runs, EvalStart.Add(age)), o)) > 0 {
					n++
				}
			}
			r.Falsely = append(r.Falsely, frac(n, len(noise)))
		}
		for _, soon := range YoungSoon {
			var got20, n20, got40, n40 int
			for _, h := range inj {
				hit := hitOf(watch.Find(until(h.h.Runs, h.in.At.Add(soon)), o), h.in, h.sc)
				if h.sc.Factor < 1.3 {
					n20++
					if hit != nil {
						got20++
					}
				} else {
					n40++
					if hit != nil {
						got40++
					}
				}
			}
			r.Soon20, r.Soon40 = append(r.Soon20, frac(got20, n20)), append(r.Soon40, frac(got40, n40))
		}
		out = append(out, r)
	}
	return out
}

// WriteYoungReport prints EvalYoung's results.
func WriteYoungReport(w io.Writer, ec EvalConfig, rs []YoungResult) {
	one := func(d time.Duration) string {
		if d < 0 {
			return "-"
		}
		return watch.Span(d)
	}
	span := func(s [2]time.Duration) string { return one(s[0]) + " / " + one(s[1]) }
	fmt.Fprintf(w, "%d histories with nothing injected, cut at each age: share with a job's change listed\n", ec.Histories)
	head := []string{fmt.Sprintf("%-12s", "before/after")}
	for _, a := range YoungAges {
		head = append(head, fmt.Sprintf("%6s", watch.Span(a)))
	}
	fmt.Fprintln(w, strings.Join(head, " "))
	for _, r := range rs {
		row := []string{fmt.Sprintf("%-12s", span(r.Side))}
		for _, f := range r.Falsely {
			row = append(row, fmt.Sprintf("%6s", pct(f)))
		}
		fmt.Fprintln(w, strings.Join(row, " "))
	}
	fmt.Fprintf(w, "\n%d histories each with a job +20%% (and +40%%) merged in the third week: share listed within\n", ec.Histories)
	head = []string{fmt.Sprintf("%-12s", "before/after")}
	for _, s := range YoungSoon {
		head = append(head, fmt.Sprintf("%11s", watch.Span(s)))
	}
	fmt.Fprintln(w, strings.Join(head, " "))
	for _, r := range rs {
		row := []string{fmt.Sprintf("%-12s", span(r.Side))}
		for i := range r.Soon20 {
			row = append(row, fmt.Sprintf("%11s", pct(r.Soon20[i])+" / "+pct(r.Soon40[i])))
		}
		fmt.Fprintln(w, strings.Join(row, " "))
	}
}
