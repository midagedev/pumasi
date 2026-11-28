package synth

import (
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
)

// WhyResult is how why-slow did on one scenario, asked about the pull
// request that made the change, AskAfter it was merged.
type WhyResult struct {
	Scenario  string
	Histories int
	Runs      int // the culprit's runs of the job, summed
	Slower    int // its job's verdict was "slower" ("bigger" for memory)
	Maybe     int // ... "maybe slower"
	OwnSure   int // the change on main was shown as its own, the only pull request that fits
	OwnMaybe  int // ... as one of several that fit
	Answered  int // a verdict other than usual, or the change shown as its own
	// Neighbours: other pull requests whose last run came within two days
	// before the change, asked the same; NeighbourSure: shown as the sure
	// cause (wrong), NeighbourMaybe: as one of several.
	Neighbours, NeighbourSure, NeighbourMaybe int
}

// WhyNoise is why-slow on pull requests of histories with nothing injected.
type WhyNoise struct {
	PRs       int
	Slower    int // a job called slower
	Maybe     int // a job called maybe slower (and none slower)
	Own       int // a change on main shown as its own
	JobsAsked int
	JobSlower int
	JobMaybe  int
}

// AskAfter: why-slow is asked this long after the culprit was merged.
const AskAfter = 3 * 24 * time.Hour

var whyScenarios = []Scenario{Scenarios[0], Scenarios[1], Scenarios[4], Scenarios[2]}

// EvalWhy scores why-slow on the time, step and memory scenarios.
func EvalWhy(ec EvalConfig) (WhyNoise, []WhyResult) {
	if ec.Days == 0 {
		ec.Days = 28
	}
	var noise WhyNoise
	for i := range ec.Histories {
		h := Generate(Config{Seed: uint64(1_000_000 + i), Start: EvalStart, Days: ec.Days, Merges: ec.Merges})
		rng := rand.New(rand.NewPCG(uint64(i), 11))
		prs := prsIn(h.Runs, EvalStart.Add(14*24*time.Hour), EvalStart.Add(time.Duration(ec.Days)*24*time.Hour))
		rng.Shuffle(len(prs), func(a, b int) { prs[a], prs[b] = prs[b], prs[a] })
		for _, pr := range prs[:min(20, len(prs))] {
			rep, err := watch.WhySlow(h.Runs, watch.Target{PR: pr})
			if err != nil {
				continue
			}
			noise.PRs++
			slower, maybe := false, false
			for _, j := range rep.Jobs {
				noise.JobsAsked++
				switch j.Time.Verdict {
				case "slower":
					slower = true
					noise.JobSlower++
				case "maybe slower":
					maybe = true
					noise.JobMaybe++
				}
			}
			switch {
			case slower:
				noise.Slower++
			case maybe:
				noise.Maybe++
			}
			if len(rep.Own) > 0 {
				noise.Own++
			}
		}
	}
	var per []WhyResult
	for si, sc := range whyScenarios {
		res := WhyResult{Scenario: sc.Name}
		for i := range ec.Histories {
			seed := uint64(si*100_000 + i + 7)
			rng := rand.New(rand.NewPCG(seed, 7))
			var jobs []Job
			for _, j := range Shop {
				if !j.Nightly {
					jobs = append(jobs, j)
				}
			}
			job := jobs[rng.IntN(len(jobs))]
			ch := Change{Job: job.Name, Metric: sc.Metric, Factor: sc.Factor}
			if sc.Step {
				ch.Step = slices.MaxFunc(job.Steps, func(a, b Step) int { return int(1000*a.Share) - int(1000*b.Share) }).Name
			}
			after := time.Duration(14*24+rng.IntN(7*24)) * time.Hour
			h := Generate(Config{Seed: seed, Start: EvalStart, Days: ec.Days, Merges: ec.Merges, Inject: []Planned{{Change: ch, After: after}}})
			if len(h.Injected) == 0 {
				continue
			}
			in := h.Injected[0]
			runs := until(h.Runs, in.At.Add(AskAfter))
			rep, err := watch.WhySlow(runs, watch.Target{PR: in.PR})
			if err != nil {
				continue
			}
			res.Histories++
			metric := metricName(sc.Metric)
			answered := false
			for _, j := range rep.Jobs {
				if j.Job != in.Job {
					continue
				}
				m := j.Time
				up := "slower"
				if metric == "memory" {
					m, up = j.Memory, "bigger"
				}
				res.Runs += m.N
				switch m.Verdict {
				case up:
					res.Slower++
					answered = true
				case "maybe " + up:
					res.Maybe++
					answered = true
				}
			}
			for k, c := range rep.Own {
				if c.Job == in.Job && c.Metric == metric {
					if rep.OwnSure[k] {
						res.OwnSure++
					} else {
						res.OwnMaybe++
					}
					answered = true
					break
				}
			}
			if answered {
				res.Answered++
			}
			// Its neighbours: other pull requests whose last run came within
			// two days before the change.
			for _, pr := range prsIn(runs, in.At.Add(-48*time.Hour), in.At) {
				if pr == in.PR {
					continue
				}
				rep, err := watch.WhySlow(runs, watch.Target{PR: pr})
				if err != nil {
					continue
				}
				res.Neighbours++
				for k, c := range rep.Own {
					if c.Job == in.Job && c.Metric == metric {
						if rep.OwnSure[k] {
							res.NeighbourSure++
						} else {
							res.NeighbourMaybe++
						}
						break
					}
				}
			}
		}
		per = append(per, res)
	}
	return noise, per
}

// prsIn: the pull requests whose last run started in [from, to).
func prsIn(runs []ledger.JobRun, from, to time.Time) []int {
	last := map[int]time.Time{}
	for _, r := range runs {
		if r.PR != 0 && r.StartedAt.After(last[r.PR]) {
			last[r.PR] = r.StartedAt
		}
	}
	var out []int
	for pr, t := range last {
		if !t.Before(from) && t.Before(to) {
			out = append(out, pr)
		}
	}
	slices.Sort(out)
	return out
}

// WriteWhyReport prints EvalWhy's numbers.
func WriteWhyReport(w io.Writer, ec EvalConfig, noise WhyNoise, per []WhyResult) {
	pct := func(a, b int) string { return fmt.Sprintf("%.0f%%", 100*frac(a, b)) }
	fmt.Fprintf(w, "why-slow on made-up histories: %d per scenario, %d days, %.0f merges a weekday; the culprit pull request asked %s after it was merged\n\n",
		ec.Histories, cmp(ec.Days, 28), cmpOr(ec.Merges, 8), AskAfter)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tRUNS/PR\tSLOWER\tMAYBE\tOWN (SURE)\tOWN (ONE OF SEVERAL)\tANSWERED\tNEIGHBOURS\tNEIGHBOUR SURE\tNEIGHBOUR ONE OF")
	for _, r := range per {
		fmt.Fprintf(tw, "%s\t%.1f\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.Scenario, float64(r.Runs)/float64(max(r.Histories, 1)),
			pct(r.Slower, r.Histories), pct(r.Maybe, r.Histories), pct(r.OwnSure, r.Histories), pct(r.OwnMaybe, r.Histories), pct(r.Answered, r.Histories),
			r.Neighbours, pct(r.NeighbourSure, r.Neighbours), pct(r.NeighbourMaybe, r.Neighbours))
	}
	tw.Flush()
	fmt.Fprintf(w, "\nnothing injected: %d pull requests; a job called slower in %s of them, maybe slower (none slower) in %s, a change shown as its own in %s\n",
		noise.PRs, pct(noise.Slower, noise.PRs), pct(noise.Maybe, noise.PRs), pct(noise.Own, noise.PRs))
	fmt.Fprintf(w, "  per job: slower %s, maybe slower %s of %d\n", pct(noise.JobSlower, noise.JobsAsked), pct(noise.JobMaybe, noise.JobsAsked), noise.JobsAsked)
}

func cmp(a, b int) int {
	if a == 0 {
		return b
	}
	return a
}
