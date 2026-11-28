package watch

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// why-slow: one run, a pull request's runs or a commit's, set against the
// usual runs of the same jobs on the main branch just before — which jobs
// and steps took longer, by how much, and since which commit if main itself
// changed.

// Target is what is asked about: a workflow run, a pull request or a commit.
type Target struct {
	RunID int64  `json:"runId,omitempty"`
	PR    int    `json:"pr,omitempty"`
	SHA   string `json:"sha,omitempty"` // a prefix will do
}

var (
	runURL = regexp.MustCompile(`/actions/runs/(\d+)`)
	prURL  = regexp.MustCompile(`/pull/(\d+)`)
	shaRE  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// ParseTarget reads what a person pastes: a run's URL (or a job's in it), a
// pull request's URL, #123 or pr/123 for a pull request, a run's number, or
// a commit's hash.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if m := runURL.FindStringSubmatch(s); m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		return Target{RunID: id}, nil
	}
	if m := prURL.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Target{PR: n}, nil
	}
	for _, p := range []string{"#", "pr/", "pr", "PR"} {
		if n, ok := strings.CutPrefix(s, p); ok {
			if v, err := strconv.Atoi(n); err == nil && v > 0 {
				return Target{PR: v}, nil
			}
		}
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
		return Target{RunID: v}, nil
	}
	if shaRE.MatchString(strings.ToLower(s)) {
		return Target{SHA: strings.ToLower(s)}, nil
	}
	return Target{}, fmt.Errorf("%q is not a run (its URL or number), a pull request (its URL or #123) or a commit", s)
}

// Usual is the spread of the baseline's runs of a job.
type Usual struct {
	Runs int     `json:"runs"`
	P10  float64 `json:"p10"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
}

func usualOf(v []float64) Usual {
	if len(v) == 0 {
		return Usual{}
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return Usual{Runs: len(s), P10: Percentile(s, 0.1), P50: median(slices.Clone(s)), P90: Percentile(s, 0.9)}
}

// Measure is one number of the target against the baseline, both as on the
// host the target ran on (a slower host is not a slower job).
type Measure struct {
	This  float64 `json:"this"` // the target's (the median when it ran more than once)
	N     int     `json:"n"`    // the target's runs it is of
	Usual Usual   `json:"usual"`
	Ratio float64 `json:"ratio"` // This/Usual.P50 - 1
	// Z: how far This is from the usual, in the usual's own spread (robust,
	// on log values) and for N runs against Usual.Runs. Low: how far the
	// target's fastest run is, in the same spread (a slow outlier, a slow
	// host, a cold cache only ever make a run slower).
	Z   float64 `json:"z"`
	Low float64 `json:"low"`
	// Verdict: "slower" when it ran twice or more and every run was well
	// over the usual (Low ≥ 2) and 10% or more over the usual median in all;
	// "maybe slower" when the median of its runs is 10% or more over, Z ≥ 2
	// and every run over the usual's p84 (Low ≥ 1) — or it ran once and Z ≥ 3
	// (one run is never more than maybe: a cold cache); the same for faster
	// (bigger/smaller, more/less for memory and cpu); "usual"; or "unknown"
	// (fewer than 5 usual runs).
	Verdict string `json:"verdict"`
}

func measure(this, base []float64, up, down string) Measure {
	m := Measure{This: median(slices.Clone(this)), N: len(this), Usual: usualOf(base), Verdict: "usual"}
	if m.Usual.Runs < 5 || m.This <= 0 || m.Usual.P50 <= 0 {
		m.Verdict = "unknown"
		return m
	}
	m.Ratio = m.This/m.Usual.P50 - 1
	logs := make([]float64, 0, len(base))
	for _, v := range base {
		if v > 0 {
			logs = append(logs, math.Log(v))
		}
	}
	mid := median(slices.Clone(logs))
	dev := make([]float64, len(logs))
	for i, v := range logs {
		dev[i] = math.Abs(v - mid)
	}
	sigma := max(1.4826*median(dev), 0.02)
	m.Z = (math.Log(m.This) - mid) / (sigma * math.Sqrt(1/float64(m.N)+1/float64(len(logs))))
	lo, hi := slices.Min(this), slices.Max(this)
	if m.Ratio >= 0 {
		m.Low = (math.Log(max(lo, 1e-9)) - mid) / sigma
	} else {
		m.Low = (math.Log(max(hi, 1e-9)) - mid) / sigma
	}
	switch {
	case m.Ratio >= 0.10 && m.N >= 2 && m.Low >= 2:
		m.Verdict = up
	case m.Ratio >= 0.10 && m.Z >= 2 && (m.N == 1 && m.Z >= 3 || m.N >= 2 && m.Low >= 1):
		m.Verdict = "maybe " + up
	case m.Ratio <= -0.10 && m.N >= 2 && m.Low <= -2:
		m.Verdict = down
	case m.Ratio <= -0.10 && m.Z <= -2 && (m.N == 1 && m.Z <= -3 || m.N >= 2 && m.Low <= -1):
		m.Verdict = "maybe " + down
	}
	return m
}

// StepWhy is a step of a job against its baseline.
type StepWhy struct {
	Name  string  `json:"name"`
	This  float64 `json:"this"`
	Usual float64 `json:"usual"` // median seconds
	Delta float64 `json:"delta"` // This - Usual
}

// JobWhy is one job of the target.
type JobWhy struct {
	Workflow  string    `json:"workflow"`
	Job       string    `json:"job"`
	Runs      []RunRef  `json:"runs"` // the target's runs of the job
	Time      Measure   `json:"time"`
	CPU       Measure   `json:"cpu"`
	Memory    Measure   `json:"memory"`
	Steps     []StepWhy `json:"steps,omitempty"`     // the most grown first
	Contended bool      `json:"contended,omitempty"` // every run of it that succeeded was on a host saturated by others
	Failed    bool      `json:"failed,omitempty"`    // no run of it succeeded: time and steps say little
	// BaselineSince: main changed in this job before the target, at this
	// moment: only main's runs since are its baseline (zero = no change).
	BaselineSince time.Time `json:"baselineSince,omitzero"`
	// SlowHost: the other jobs on its host around then took this many times
	// their usual (1.10 or more, else 0); its time is divided by it.
	SlowHost float64 `json:"slowHost,omitempty"`
}

// WhyReport is `pumasi why-slow` and /api/why-slow.
type WhyReport struct {
	Target   Target    `json:"target"`
	What     string    `json:"what"`   // "run 123 (ci.yml, pull request #12, 4f2a1b3)"
	Branch   string    `json:"branch"` // the baseline's branch
	Baseline int       `json:"baseline"`
	From     time.Time `json:"from"` // the target's first run started
	Jobs     []JobWhy  `json:"jobs"` // the slowest against usual first
	// Total: the target's workflow runs, first job to last, against the
	// baseline's (when one workflow); cpu all jobs.
	Total *JobWhy `json:"total,omitempty"`
	// Changes: steps on the baseline's branch in these jobs between the
	// baseline and now (main itself got slower, not only the target).
	Changes []Change `json:"changes"`
	// Own: steps on the baseline's branch the target is the likely cause of:
	// they began at its commit, or (a pull request) right after it was
	// merged with its runs already at the new level. OwnSure: the pull
	// request was the only one that fits (else one of Change.Suspects).
	Own     []Change `json:"own"`
	OwnSure []bool   `json:"ownSure"`
	Note    string   `json:"note"`
}

// BaselineRuns: each job is set against this many of its runs on the main
// branch just before the target (that succeeded on a host not saturated by
// others), from the last two weeks.
const BaselineRuns = 20

// Trunk is the branch most runs that are not of a pull request ran on.
func Trunk(runs []ledger.JobRun) string {
	n := map[string]int{}
	for _, r := range runs {
		if r.PR == 0 && r.Event != "pull_request" && r.Branch != "" {
			n[r.Branch]++
		}
	}
	best := ""
	for b, c := range n {
		if c > n[best] || (c == n[best] && b < best) {
			best = b
		}
	}
	return best
}

// WhySlow sets the target's runs against the baseline. runs should reach
// two weeks and more before the target.
func WhySlow(runs []ledger.JobRun, t Target) (WhyReport, error) {
	rep := WhyReport{Target: t, Changes: []Change{}, Own: []Change{}, OwnSure: []bool{}, Note: "each job against the median (p50) and spread (p10-p90) " +
		"of its last runs on the baseline branch before the target that succeeded on a host not saturated by others, as on the target's host; " +
		"slower = 10% or more over p50 and z >= 3 (robust spread of the baseline) over two runs or more; maybe = z >= 2, or one run"}
	var mine []ledger.JobRun
	for _, r := range runs {
		switch {
		case t.RunID != 0 && r.RunID == t.RunID,
			t.PR != 0 && r.PR == t.PR,
			t.SHA != "" && r.SHA != "" && strings.HasPrefix(r.SHA, t.SHA):
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return rep, errors.New("no finished job of it in the ledger: " + targetWords(t) + " did not run on pumasi, or has not finished, or is older than the ledger keeps")
	}
	slices.SortFunc(mine, func(a, b ledger.JobRun) int { return codeTime(a).Compare(codeTime(b)) })
	rep.From = codeTime(mine[0])
	rep.What = describeTarget(t, mine)
	rep.Branch = Trunk(runs)
	before := func(r ledger.JobRun) bool {
		return r.Branch == rep.Branch && r.PR == 0 && r.Event != "pull_request" && r.Result == "succeeded" && !r.Contended() &&
			codeTime(r).Before(rep.From) && rep.From.Sub(codeTime(r)) <= 14*24*time.Hour
	}
	type jk struct{ wf, job string }
	byJob := map[jk][]ledger.JobRun{}
	var order []jk
	for _, r := range mine {
		k := jk{r.Workflow, r.Job}
		if byJob[k] == nil {
			order = append(order, k)
		}
		byJob[k] = append(byJob[k], r)
	}
	base := map[jk][]ledger.JobRun{}
	for _, r := range runs {
		if k := (jk{r.Workflow, r.Job}); byJob[k] != nil && before(r) {
			base[k] = append(base[k], r)
		}
	}
	slow := hostSlowness(runs, mine)
	var baseStart time.Time
	trimmed := map[jk][]ledger.JobRun{}
	for _, k := range order {
		b := base[k]
		slices.SortFunc(b, func(x, y ledger.JobRun) int { return codeTime(x).Compare(codeTime(y)) })
		if len(b) > BaselineRuns {
			b = b[len(b)-BaselineRuns:]
		}
		trimmed[k] = b
		if len(b) > 0 && (baseStart.IsZero() || codeTime(b[0]).Before(baseStart)) {
			baseStart = codeTime(b[0])
		}
	}
	// Did main itself change in these jobs? The changes that are the
	// target's own, and those since the oldest run of the baseline up to the
	// target (the baseline mixes two levels).
	var trunkRuns []ledger.JobRun
	for _, r := range runs {
		if byJob[jk{r.Workflow, r.Job}] != nil {
			trunkRuns = append(trunkRuns, r)
		}
	}
	last := codeTime(mine[len(mine)-1])
	for _, c := range Find(trunkRuns, Options{}) {
		if c.Gradual {
			continue
		}
		own, sure := false, false
		switch {
		case t.PR != 0 && c.PR == t.PR:
			own, sure = true, true
		case t.PR != 0 && c.PR == 0 && slices.Contains(c.Suspects, t.PR):
			own, sure = true, len(c.Suspects) == 1
		default:
			for _, r := range mine {
				if r.PR == 0 && r.SHA != "" && r.SHA == c.First.SHA {
					own, sure = true, true
				}
			}
		}
		switch {
		case own:
			rep.Own = append(rep.Own, c)
			rep.OwnSure = append(rep.OwnSure, sure)
		case !baseStart.IsZero() && !c.First.At.Before(baseStart) && !c.First.At.After(last):
			rep.Changes = append(rep.Changes, c)
		}
	}
	// A job main changed in before the target: its baseline is main since.
	var cutAll time.Time
	for _, k := range order {
		b := trimmed[k]
		var cut time.Time
		for _, c := range rep.Changes {
			if c.Workflow == k.wf && c.Job == k.job && !c.First.At.After(rep.From) && c.First.At.After(cut) {
				cut = c.First.At
			}
		}
		if !cut.IsZero() {
			b = slices.DeleteFunc(slices.Clone(b), func(r ledger.JobRun) bool { return codeTime(r).Before(cut) })
			if cut.After(cutAll) {
				cutAll = cut
			}
		}
		rep.Baseline = max(rep.Baseline, len(b))
		var all []ledger.JobRun
		for _, r := range runs {
			if r.Workflow == k.wf && r.Job == k.job {
				all = append(all, r)
			}
		}
		j := jobWhy(k.wf, k.job, byJob[k], b, all, slow)
		j.BaselineSince = cut
		rep.Jobs = append(rep.Jobs, j)
	}
	slices.SortStableFunc(rep.Jobs, func(a, b JobWhy) int {
		return cmp.Compare(b.Time.This-b.Time.Usual.P50, a.Time.This-a.Time.Usual.P50)
	})
	rep.Total = total(mine, runs, func(r ledger.JobRun) bool { return before(r) && !codeTime(r).Before(cutAll) })
	return rep, nil
}

func jobWhy(wf, job string, mine, base, all []ledger.JobRun, slow map[string]float64) JobWhy {
	j := JobWhy{Workflow: wf, Job: job}
	var ok []ledger.JobRun
	hosts := map[string]bool{}
	for _, r := range mine {
		j.Runs = append(j.Runs, refOf(r))
		if r.Contended() {
			j.Contended = true
		}
		if r.Result == "succeeded" {
			ok = append(ok, r)
		}
		hosts[machine(r)] = true
	}
	var calm []ledger.JobRun
	for _, r := range ok {
		if !r.Contended() {
			calm = append(calm, r)
		}
	}
	use := calm
	switch {
	case len(use) > 0:
		j.Contended = false // a run on a saturated host is left out
	case len(ok) > 0:
		use = ok
	default:
		use, j.Failed = mine, true
	}
	// Every value as on the target's machine (host and class, when it ran on
	// one), else as on the job's machines together.
	var clean []ledger.JobRun
	for _, r := range all {
		if r.Result == "succeeded" && !r.Contended() {
			clean = append(clean, r)
		}
	}
	// scale: what a run's value is multiplied by to be as on the target's host.
	scale := func(m string) func(ledger.JobRun) float64 {
		hf := hostFactors(clean, m)
		to := 1.0
		if len(hosts) == 1 {
			for h := range hosts {
				if v := hf[h]; v > 0 {
					to = v
				}
			}
		}
		return func(r ledger.JobRun) float64 {
			if from := hf[machine(r)]; from > 0 {
				return to / from
			}
			return 1
		}
	}
	timeScale := scale("time")
	vals := func(rs []ledger.JobRun, m string, f func(ledger.JobRun) float64) []float64 {
		sc := scale(m)
		if m == "time" {
			sc = timeScale
		}
		out := make([]float64, len(rs))
		for i, r := range rs {
			out[i] = f(r) * sc(r)
		}
		return out
	}
	secs := func(r ledger.JobRun) float64 { return r.RunSec }
	cpu := func(r ledger.JobRun) float64 { return r.CPUSec }
	mineSecs := func(r ledger.JobRun) float64 {
		if f := slow[r.Runner]; f > 0 {
			j.SlowHost = max(j.SlowHost, f)
			return r.RunSec / f
		}
		return r.RunSec
	}
	j.Time = measure(vals(use, "time", mineSecs), vals(base, "time", secs), "slower", "faster")
	j.CPU = measure(vals(use, "cpu", cpu), vals(base, "cpu", cpu), "more", "less")
	j.Memory = measure(vals(use, "memory", Mem), vals(base, "memory", Mem), "bigger", "smaller")
	// Steps: the target's median of each step against the baseline's, on the
	// same scale as the job's time.
	stepVals := func(rs []ledger.JobRun, mine bool) (map[string][]float64, []string) {
		m := map[string][]float64{}
		var names []string
		for _, r := range rs {
			k := timeScale(r)
			if f := slow[r.Runner]; mine && f > 0 {
				k /= f
			}
			for _, s := range r.Steps {
				if m[s.Name] == nil {
					names = append(names, s.Name)
				}
				m[s.Name] = append(m[s.Name], s.Sec*k)
			}
		}
		return m, names
	}
	tm, names := stepVals(use, true)
	bm, _ := stepVals(base, false)
	for _, n := range names {
		if len(bm[n]) == 0 {
			j.Steps = append(j.Steps, StepWhy{Name: n, This: median(tm[n]), Delta: median(tm[n])})
			continue
		}
		s := StepWhy{Name: n, This: median(slices.Clone(tm[n])), Usual: median(slices.Clone(bm[n]))}
		s.Delta = s.This - s.Usual
		j.Steps = append(j.Steps, s)
	}
	slices.SortStableFunc(j.Steps, func(a, b StepWhy) int { return cmp.Compare(b.Delta, a.Delta) })
	return j
}

// total: the target's workflow runs against the baseline's, when they are of
// one workflow.
func total(mine, runs []ledger.JobRun, before func(ledger.JobRun) bool) *JobWhy {
	wf := mine[0].Workflow
	for _, r := range mine {
		if r.Workflow != wf {
			return nil
		}
	}
	jobs := map[string]bool{}
	for _, r := range mine {
		jobs[r.Job] = true
	}
	var b []ledger.JobRun
	for _, r := range runs {
		if r.Workflow == wf && jobs[r.Job] && before(r) {
			b = append(b, r)
		}
	}
	// Only whole baseline runs: those with every job the target has.
	var whole []ledger.JobRun
	count := map[int64]int{}
	for _, r := range b {
		count[r.RunID]++
	}
	for _, r := range b {
		if count[r.RunID] == len(jobs) {
			whole = append(whole, r)
		}
	}
	wb := WorkflowRuns(whole)
	if len(wb) > BaselineRuns {
		wb = wb[len(wb)-BaselineRuns:]
	}
	t := jobWhy(wf, "", WorkflowRuns(mine), wb, nil, nil)
	t.Steps = nil
	return &t
}

func targetWords(t Target) string {
	switch {
	case t.RunID != 0:
		return fmt.Sprintf("run %d", t.RunID)
	case t.PR != 0:
		return fmt.Sprintf("pull request #%d", t.PR)
	}
	return "commit " + t.SHA
}

func describeTarget(t Target, mine []ledger.JobRun) string {
	r := mine[len(mine)-1]
	wfs := map[string]bool{}
	runs := map[int64]bool{}
	for _, m := range mine {
		wfs[path.Base(m.Workflow)] = true
		runs[m.RunID] = true
	}
	var names []string
	for w := range wfs {
		names = append(names, w)
	}
	slices.Sort(names)
	var parts []string
	parts = append(parts, strings.Join(names, ", "))
	if r.PR != 0 && t.PR == 0 {
		parts = append(parts, fmt.Sprintf("pull request #%d", r.PR))
	}
	if r.Branch != "" && r.PR == 0 {
		parts = append(parts, "on "+r.Branch)
	}
	if r.SHA != "" {
		parts = append(parts, "commit "+Short(r.SHA))
	}
	n := len(runs)
	what := targetWords(t)
	if t.RunID == 0 {
		what += fmt.Sprintf(", %d run%s", n, plural(n))
	}
	return fmt.Sprintf("%s (%s)", what, strings.Join(parts, ", "))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Pct is a ratio as +38%.
func Pct(f float64) string {
	if math.IsNaN(f) {
		return "-"
	}
	return fmt.Sprintf("%+.0f%%", 100*f)
}

// hostSlowness: for each of the target's runs, how slow its host was for the
// other jobs that ended on it from half an hour after it started to half an
// hour after it ended (each against its own median on that host, the median
// of those), when three or more did and it was 1.10 or more. Keyed by runner.
func hostSlowness(runs, mine []ledger.JobRun) map[string]float64 {
	type jh struct{ wf, job, host string }
	secs := map[jh][]float64{}
	byHost := map[string][]ledger.JobRun{}
	for _, r := range runs {
		if r.Result != "succeeded" || r.RunSec <= 0 {
			continue
		}
		k := jh{r.Workflow, r.Job, r.Host}
		secs[k] = append(secs[k], r.RunSec)
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	med := map[jh]float64{}
	for k, v := range secs {
		if len(v) >= 5 {
			med[k] = median(v)
		}
	}
	for _, rs := range byHost {
		slices.SortFunc(rs, func(a, b ledger.JobRun) int { return a.FinishedAt.Compare(b.FinishedAt) })
	}
	out := map[string]float64{}
	for _, r := range mine {
		rs := byHost[r.Host]
		from, to := r.StartedAt.Add(-30*time.Minute), r.FinishedAt.Add(30*time.Minute)
		i, _ := slices.BinarySearchFunc(rs, from, func(x ledger.JobRun, t time.Time) int { return x.FinishedAt.Compare(t) })
		var ratios []float64
		for ; i < len(rs) && !rs[i].FinishedAt.After(to); i++ {
			n := rs[i]
			if n.RunID == r.RunID || (r.PR != 0 && n.PR == r.PR) {
				continue
			}
			if m := med[jh{n.Workflow, n.Job, n.Host}]; m > 0 {
				ratios = append(ratios, n.RunSec/m)
			}
		}
		if len(ratios) >= 3 {
			if f := median(ratios); f >= 1.10 {
				out[r.Runner] = f
			}
		}
	}
	return out
}
