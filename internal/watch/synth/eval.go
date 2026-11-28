package synth

import (
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
)

// EvalStart is the first day of every evaluated history (a Monday).
var EvalStart = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

// Scenario is one kind of injected change.
type Scenario struct {
	Name   string
	Metric string
	Factor float64
	Step   bool // the job's biggest step grows by Factor, the rest stays
	Creep  int  // the factor comes over this many merges
}

// Scenarios are the changes the detector is scored on.
var Scenarios = []Scenario{
	{Name: "time +20%", Metric: "duration", Factor: 1.2},
	{Name: "time +40%", Metric: "duration", Factor: 1.4},
	{Name: "memory +30%", Metric: "mem", Factor: 1.3},
	{Name: "cpu +25%", Metric: "cpu", Factor: 1.25},
	{Name: "one step +40%", Metric: "duration", Factor: 1.4, Step: true},
	{Name: "creep +25% over 30 merges", Metric: "duration", Factor: 1.25, Creep: 30},
}

// Result is how the detector did on one scenario.
type Result struct {
	Scenario    string
	Histories   int
	Found       int // the injected change was reported, on its job, metric and side
	RightCommit int // ... and its first run was the merge commit's on main
	InRange     int // ... or that commit's run was among those between, not counted
	Gradual     int // ... found, but called gradual (no commit named)
	StepNamed   int // ... and (one-step scenarios) the step that grew came first
	PRNamed     int // a pull request was named
	PRRight     int // ... and it was the one
	FalseAlarms int // other changes reported
	Weeks       float64
	// RatioErr: the reported ratio against the injected one, median of |error|.
	RatioErr float64
}

func (r Result) Recall() float64 { return frac(r.Found, r.Histories) }

// Attribution: of the changes found, the share whose commit was right.
func (r Result) Attribution() float64 { return frac(r.RightCommit, r.Found) }
func (r Result) FalsePerWeek() float64 {
	if r.Weeks == 0 {
		return 0
	}
	return float64(r.FalseAlarms) / r.Weeks
}

func frac(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// EvalConfig is the size of an evaluation.
type EvalConfig struct {
	Histories int     // per scenario
	Days      int     // history length (default 28)
	Merges    float64 // weekday merges (default 8)
	Options   watch.Options
	// After: how long after the injection the detector looks (default: to
	// the end of the history, at least 6 days).
	After time.Duration
}

// Eval scores the detector: each scenario injected into its own histories at
// a random moment of the third week on a random job, and as many histories
// with nothing injected.
func Eval(ec EvalConfig) (noise Result, per []Result) {
	if ec.Days == 0 {
		ec.Days = 28
	}
	noise = Result{Scenario: "nothing injected", Histories: ec.Histories}
	for i := range ec.Histories {
		h := Generate(Config{Seed: uint64(1_000_000 + i), Start: EvalStart, Days: ec.Days, Merges: ec.Merges})
		cs := watch.Find(h.Runs, ec.Options)
		noise.FalseAlarms += alarms(cs, nil)
		noise.Weeks += float64(ec.Days) / 7
	}
	for si, sc := range Scenarios {
		res := Result{Scenario: sc.Name, Histories: ec.Histories}
		var errs []float64
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
			ch := Change{Job: job.Name, Metric: sc.Metric, Factor: sc.Factor, Creep: sc.Creep}
			if sc.Step {
				big := slices.MaxFunc(job.Steps, func(a, b Step) int {
					return int(1000*a.Share) - int(1000*b.Share)
				})
				ch.Step = big.Name
			}
			after := time.Duration(14*24+rng.IntN(7*24)) * time.Hour
			h := Generate(Config{Seed: seed, Start: EvalStart, Days: ec.Days, Merges: ec.Merges, Inject: []Planned{{Change: ch, After: after}}})
			if len(h.Injected) == 0 {
				continue
			}
			in := h.Injected[0]
			runs := h.Runs
			if ec.After > 0 {
				runs = until(runs, in.At.Add(ec.After))
				res.Weeks += in.At.Add(ec.After).Sub(EvalStart).Hours() / (24 * 7)
			} else {
				res.Weeks += float64(ec.Days) / 7
			}
			cs := watch.Find(runs, ec.Options)
			hit := hitOf(cs, in, sc)
			res.FalseAlarms += alarms(cs, hit)
			if hit == nil {
				continue
			}
			res.Found++
			if hit.Gradual {
				res.Gradual++
			}
			if hit.First.SHA == in.SHA && !hit.Gradual {
				res.RightCommit++
				res.InRange++
			} else if !hit.Gradual && slices.ContainsFunc(hit.Between, func(r watch.RunRef) bool { return r.SHA == in.SHA }) {
				res.InRange++
			}
			if sc.Step && len(hit.Steps) > 0 && hit.Steps[0].Name == in.Step {
				res.StepNamed++
			}
			if hit.PR != 0 {
				res.PRNamed++
				if hit.PR == in.PR {
					res.PRRight++
				}
			}
			want := sc.Factor - 1
			if sc.Step {
				share := 0.0
				for _, s := range job.Steps {
					if s.Name == ch.Step {
						share = s.Share
					}
				}
				want = (sc.Factor - 1) * share
			}
			errs = append(errs, abs(hit.Ratio-want))
		}
		if len(errs) > 0 {
			slices.Sort(errs)
			res.RatioErr = errs[len(errs)/2]
		}
		per = append(per, res)
	}
	return noise, per
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func until(runs []ledger.JobRun, t time.Time) []ledger.JobRun {
	var out []ledger.JobRun
	for _, r := range runs {
		if r.FinishedAt.Before(t) {
			out = append(out, r)
		}
	}
	return out
}

func metricName(m string) string {
	switch m {
	case "mem":
		return "memory"
	case "duration":
		return "time"
	}
	return m
}

// hitOf is the reported change that is the injected one: its job, metric and
// side, its first run no more than a day before the merge (a pull request's
// runs test it before) and no more than three days after.
func hitOf(cs []watch.Change, in Injected, sc Scenario) *watch.Change {
	for i, c := range cs {
		if c.Job == in.Job && c.Metric == metricName(sc.Metric) && c.Slower() &&
			c.First.At.After(in.At.Add(-24*time.Hour)) && c.First.At.Before(in.At.Add(72*time.Hour)) {
			return &cs[i]
		}
	}
	return nil
}

// alarms counts the changes reported other than the hit: one for each job
// and moment, whatever the metrics (one would be announced).
func alarms(cs []watch.Change, hit *watch.Change) int {
	seen := map[string]bool{}
	for _, c := range cs {
		if hit != nil && c.Job == hit.Job && abs(c.First.At.Sub(hit.First.At).Hours()) < 24 {
			continue
		}
		k := c.Job + "@" + c.First.At.Truncate(24*time.Hour).String()
		seen[k] = true
	}
	return len(seen)
}

// WriteReport prints an evaluation.
func WriteReport(w io.Writer, ec EvalConfig, noise Result, per []Result) {
	fmt.Fprintf(w, "%d histories per scenario, %d days each, %.0f merges a weekday, 7 jobs on two hosts\n\n", ec.Histories, cmpDays(ec.Days), cmpMerges(ec.Merges))
	fmt.Fprintf(w, "%-28s %7s %10s %9s %8s %9s %10s %10s %8s\n", "scenario", "recall", "commit ok", "in range", "step ok", "PR named", "PR right", "false/wk", "gradual")
	for _, r := range per {
		step := "-"
		if strings.Contains(r.Scenario, "step") {
			step = pct(frac(r.StepNamed, r.Found))
		}
		fmt.Fprintf(w, "%-28s %7s %10s %9s %8s %9s %10s %10.2f %8s\n", r.Scenario, pct(r.Recall()), pct(r.Attribution()), pct(frac(r.InRange, r.Found)), step,
			pct(frac(r.PRNamed, r.Found)), pct(frac(r.PRRight, r.PRNamed)), r.FalsePerWeek(), pct(frac(r.Gradual, r.Found)))
	}
	fmt.Fprintf(w, "%-28s %7s %10s %9s %8s %9s %10s %10.2f\n", noise.Scenario, "-", "-", "-", "-", "-", "-", noise.FalsePerWeek())
	fmt.Fprintln(w)
	for _, r := range per {
		fmt.Fprintf(w, "%s: ratio error median %.3f\n", r.Scenario, r.RatioErr)
	}
}

func cmpDays(d int) int {
	if d == 0 {
		return 28
	}
	return d
}

func cmpMerges(m float64) float64 {
	if m == 0 {
		return 8
	}
	return m
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", 100*f) }

// SlopeResult is how the growth alarm (watch.slope) did: checked every day
// at noon, on each scenario's histories and on histories with nothing in.
type SlopeResult struct {
	Scenario    string
	Histories   int
	Caught      int           // an alarm on the injected job within 7 days of the change
	Delays      time.Duration // their sum, from the change to the first alarm
	FalseAlarms int           // alarm episodes on other jobs (or on any job, nothing injected)
	Weeks       float64
}

func (r SlopeResult) MeanDelay() time.Duration {
	if r.Caught == 0 {
		return 0
	}
	return r.Delays / time.Duration(r.Caught)
}

// EvalSlope scores the growth alarm at a limit (0.10 = 10% a week).
func EvalSlope(ec EvalConfig, limit float64) []SlopeResult {
	if ec.Days == 0 {
		ec.Days = 28
	}
	w := config.Watch{Slope: config.Percent(limit)}
	check := func(h History, injected string, at time.Time) (caught bool, delay time.Duration, false_ int) {
		open := map[string]bool{}
		for d := 7; d < ec.Days; d++ {
			now := EvalStart.AddDate(0, 0, d).Add(12 * time.Hour)
			as := watch.Alarms(watch.AlarmInput{Now: now, Loc: time.UTC, Runs: until(h.Runs, now), Open: open}, w)
			next := map[string]bool{}
			for _, a := range as {
				next[a.Key] = true
				if open[a.Key] {
					continue
				}
				if injected != "" && strings.HasSuffix(a.Key, "/"+injected) && !now.Before(at) && now.Sub(at) <= 7*24*time.Hour {
					if !caught {
						caught, delay = true, now.Sub(at)
					}
					continue
				}
				if injected != "" && strings.HasSuffix(a.Key, "/"+injected) {
					continue // the injected job, later on: true, not counted
				}
				false_++
			}
			open = next
		}
		return
	}
	var out []SlopeResult
	noise := SlopeResult{Scenario: "nothing injected", Histories: ec.Histories}
	for i := range ec.Histories {
		h := Generate(Config{Seed: uint64(1_000_000 + i), Start: EvalStart, Days: ec.Days, Merges: ec.Merges})
		_, _, f := check(h, "", time.Time{})
		noise.FalseAlarms += f
		noise.Weeks += float64(ec.Days-7) / 7
	}
	for si, sc := range Scenarios {
		res := SlopeResult{Scenario: sc.Name, Histories: ec.Histories}
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
			if sc.Metric != "duration" {
				continue // the growth alarm watches time
			}
			ch := Change{Job: job.Name, Metric: sc.Metric, Factor: sc.Factor, Creep: sc.Creep}
			if sc.Step {
				ch.Step = slices.MaxFunc(job.Steps, func(a, b Step) int { return int(1000*a.Share) - int(1000*b.Share) }).Name
			}
			after := time.Duration(14*24+rng.IntN(7*24)) * time.Hour
			h := Generate(Config{Seed: seed, Start: EvalStart, Days: ec.Days, Merges: ec.Merges, Inject: []Planned{{Change: ch, After: after}}})
			if len(h.Injected) == 0 {
				continue
			}
			c, d, f := check(h, job.Name, h.Injected[0].At)
			if c {
				res.Caught++
				res.Delays += d
			}
			res.FalseAlarms += f
			res.Weeks += float64(ec.Days-7) / 7
		}
		if res.Weeks > 0 {
			out = append(out, res)
		}
	}
	return append(out, noise)
}

// WriteSlopeReport prints EvalSlope's results.
func WriteSlopeReport(w io.Writer, limit float64, rs []SlopeResult) {
	fmt.Fprintf(w, "growth alarm at %.0f%% a week, checked daily at noon\n", 100*limit)
	fmt.Fprintf(w, "%-28s %12s %12s %10s\n", "scenario", "caught (7d)", "mean delay", "false/wk")
	for _, r := range rs {
		caught, delay := "-", "-"
		if r.Scenario != "nothing injected" {
			caught = pct(frac(r.Caught, r.Histories))
			delay = fmt.Sprintf("%.1fd", r.MeanDelay().Hours()/24)
		}
		fp := 0.0
		if r.Weeks > 0 {
			fp = float64(r.FalseAlarms) / r.Weeks
		}
		fmt.Fprintf(w, "%-28s %12s %12s %10.2f\n", r.Scenario, caught, delay, fp)
	}
}
