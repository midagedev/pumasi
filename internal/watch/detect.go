package watch

import (
	"cmp"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// A regression is a step in a job's numbers that holds: from some run on,
// every run is slower (or bigger) by about the same, not one outlier. It is
// found among the runs that succeeded and whose host was not saturated by
// others, each run's number set against what its host usually gives for the
// job (a slower host is not a slower job). The first run after the step names
// the commit that brought it, and the steps of the job that grew.

// Options tune the search. The defaults are what the evaluation in
// detect_eval_test.go was measured with.
type Options struct {
	// MinEffect: a change smaller than this share is not reported (default 10%).
	MinEffect float64
	// MinRuns: runs on each side of a change, at least (default 8).
	MinRuns int
	// Z: how sure, as a rank test's z-score between the runs just before and
	// just after (default 3.5). The split is the best of every run, so this
	// is not a p-value; the bar was set on histories with nothing injected.
	Z float64
	// Local: runs on each side the test looks at (default 40).
	Local int
	// Transient: a step up that comes back down within this long is a bad
	// spell, not a regression (default 3 days).
	Transient time.Duration
	// MinBefore and MinAfter: a job's change is listed only once the runs
	// on each side of it span this long (default MinSide). Shorter sides
	// may be a few hours of a host's mood.
	MinBefore, MinAfter time.Duration
	// Plain: the search as it was before machines were told apart from
	// jobs (no machine shifts, no history rule); for the evaluation.
	Plain bool
}

// MinSide is how long the runs on each side of a job's change must span
// before it is listed. Set on the evaluation (watcheval -young): see
// docs/watch.md.
const MinSide = 24 * time.Hour

func (o Options) withDefaults() Options {
	if o.MinEffect == 0 {
		o.MinEffect = 0.10
	}
	if o.MinRuns == 0 {
		o.MinRuns = 8
	}
	if o.Z == 0 {
		o.Z = 3.5
	}
	if o.Local == 0 {
		o.Local = 40
	}
	if o.Transient == 0 {
		o.Transient = 72 * time.Hour
	}
	if o.MinBefore == 0 {
		o.MinBefore = MinSide
	}
	if o.MinAfter == 0 {
		o.MinAfter = MinSide
	}
	return o
}

// gradualDrift: a change whose two sides climb by this share of the step is
// called gradual. Set on the evaluation: at 0.5, 3% of +20% steps are called
// gradual (their commit goes unnamed) and 56% of jobs that crept 25% over 30
// merges (the rest are named by a commit that is only where the climb was
// steepest). Calling a step gradual costs less than naming a wrong commit.
const gradualDrift = 0.5

// Metrics are the numbers a job is watched by.
var Metrics = []string{"time", "memory", "cpu"}

func metricOf(m string, r ledger.JobRun) float64 {
	switch m {
	case "memory":
		return Mem(r)
	case "cpu":
		return r.CPUSec
	}
	return r.RunSec
}

// RunRef points at a run: enough to open it and to name its commit.
type RunRef struct {
	Runner  string    `json:"runner,omitempty"`
	RunID   int64     `json:"runId"`
	GHJobID int64     `json:"ghJobId,omitempty"`
	At      time.Time `json:"at"`
	Event   string    `json:"event,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	PR      int       `json:"pr,omitempty"`
	Host    string    `json:"host,omitempty"`
}

func refOf(r ledger.JobRun) RunRef {
	return RunRef{Runner: r.Runner, RunID: r.RunID, GHJobID: r.GHJobID, At: r.StartedAt, Event: r.Event, Branch: r.Branch, SHA: r.SHA, PR: r.PR, Host: r.Host}
}

// StepDelta is how a step of a job changed across a regression (median
// seconds of the runs on each side).
type StepDelta struct {
	Name   string  `json:"name"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
}

// Change is a step in one of a job's numbers.
type Change struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Job      string `json:"job"`
	Metric   string `json:"metric"` // time | memory | cpu
	// Before and After: the median on each side, in the job's usual host's
	// terms (seconds, bytes, cpu-seconds); Ratio = After/Before - 1.
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Ratio  float64 `json:"ratio"`
	// First is the first run after the step; on a branch that runs on
	// every push (main), the run of the commit that brought it.
	First RunRef `json:"first"`
	// LastBefore is the last run before the step on the same branch as First.
	LastBefore RunRef `json:"lastBefore"`
	// Between: runs of that branch between the two that are not counted (their
	// host was saturated, or they failed): the change may have come with one
	// of their commits instead.
	Between []RunRef `json:"between,omitempty"`
	// PR: a pull request whose own runs were already at the new level before
	// First (0 = none stood out).
	PR int `json:"pr,omitempty"`
	// Suspects: every pull request whose runs (one or more) before First were
	// all at the new level, the last within two days of it.
	Suspects   []int       `json:"suspects,omitempty"`
	RunsBefore int         `json:"runsBefore"`
	RunsAfter  int         `json:"runsAfter"`
	Z          float64     `json:"z"`
	Steps      []StepDelta `json:"steps,omitempty"` // time only: the steps that grew the most first
	// Now: the median of the newest runs; Holds says whether they are still
	// at the new level (a regression nobody fixed since).
	Now   float64 `json:"now"`
	Holds bool    `json:"holds"`
	// Gradual: the runs on both sides climb (or fall) too, as much as half the
	// step: the job crept, and First is only where the climb was steepest,
	// not the commit that did it.
	Gradual bool `json:"gradual,omitempty"`
	// Host: the host the step was on, when the runs around it were mostly
	// on one host, or when it showed on one host's runs only.
	Host string `json:"host,omitempty"`
	// OneHost: it showed on Host's runs and not on another host's runs of
	// the same days, so the host changed, not the job; OnHost is the step
	// on Host's runs.
	OneHost bool    `json:"oneHost,omitempty"`
	OnHost  float64 `json:"onHost,omitempty"`
	// Moved: the job's runs on the two sides ran on different machines
	// (host and class), and the runs on a machine it ran on before and after
	// do not show the step: it moved to another kind of machine.
	Moved *Move `json:"moved,omitempty"`
	// SpanBefore and SpanAfter: how long the runs on each side span.
	SpanBefore time.Duration `json:"-"`
	SpanAfter  time.Duration `json:"-"`
}

// Move is where a job's runs went: the machines (host/class) that ran most
// of them on each side of a change.
type Move struct {
	From []string `json:"from"`
	To   []string `json:"to"`
}

// Slower says whether the change is for the worse.
func (c Change) Slower() bool { return c.Ratio > 0 }

// Find looks for changes in every job of the runs: the steps that are the
// job's, not its machine's (FindAll), and whose sides span long enough.
func Find(runs []ledger.JobRun, o Options) []Change {
	jobs, _ := FindAll(runs, o)
	return jobs
}

// find is every step found in every job, the machine's ones too.
func find(runs []ledger.JobRun, o Options) []Change {
	o = o.withDefaults()
	type key struct{ repo, wf, job string }
	by, left := map[key][]ledger.JobRun{}, map[key][]ledger.JobRun{}
	for _, r := range runs {
		k := key{r.Repo, r.Workflow, r.Job}
		if r.Result != "succeeded" || r.Contended() {
			left[k] = append(left[k], r)
			continue
		}
		by[k] = append(by[k], r)
	}
	var out []Change
	for k, rs := range by {
		slices.SortStableFunc(rs, func(a, b ledger.JobRun) int { return codeTime(a).Compare(codeTime(b)) })
		var job []Change
		for _, m := range Metrics {
			for _, c := range findIn(rs, m, o) {
				c.Repo, c.Workflow, c.Job, c.Metric = k.repo, k.wf, k.job, m
				job = append(job, c)
			}
		}
		job = agree(job)
		// The runs on main between the last before and the first after that
		// were left out (a busy host, a failure): the change may be theirs.
		when := map[string]time.Time{}
		for _, r := range rs {
			when[r.Runner] = codeTime(r)
		}
		for i := range job {
			from, to := when[job[i].LastBefore.Runner], when[job[i].First.Runner]
			if job[i].First.PR != 0 || job[i].LastBefore.Runner == "" {
				continue
			}
			for _, r := range left[k] {
				if t := codeTime(r); r.PR == 0 && r.Event != "pull_request" && t.After(from) && t.Before(to) {
					job[i].Between = append(job[i].Between, refOf(r))
				}
			}
		}
		out = append(out, job...)
	}
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(a.First.At.Compare(b.First.At), cmp.Compare(a.Job, b.Job), cmp.Compare(a.Metric, b.Metric))
	})
	return out
}

// agree: steps of one job in several numbers within three days of each other are
// one change (more work takes more time and more cpu). They are all named by
// the run that the clearest of them points at: the number with the least
// noise places the step best.
func agree(cs []Change) []Change {
	for i := range cs {
		best := i
		for j := range cs {
			if cs[j].Slower() == cs[i].Slower() && math.Abs(cs[j].First.At.Sub(cs[i].First.At).Hours()) <= 72 &&
				math.Abs(cs[j].Z) > math.Abs(cs[best].Z) {
				best = j
			}
		}
		if best != i {
			cs[i].First, cs[i].LastBefore, cs[i].PR = cs[best].First, cs[best].LastBefore, cs[best].PR
		}
	}
	return cs
}

// codeTime orders runs by when their code was taken: when the job was first
// seen waiting, else when it started.
func codeTime(r ledger.JobRun) time.Time {
	if !r.QueuedAt.IsZero() {
		return r.QueuedAt
	}
	return r.StartedAt
}

// machine is the kind of machine a run had: its host and its runner class
// (a class sets the cores, the memory and the disk a runner gets, so the
// same host in two classes is two machines).
func machine(r ledger.JobRun) string {
	if r.Class == "" {
		return r.Host
	}
	return r.Host + "/" + r.Class
}

// hostFactors: what each machine (host and class) gives for the job against
// all of them (the median of its runs over the median of all), for machines
// with enough runs. Keyed by machine(r).
func hostFactors(rs []ledger.JobRun, m string) map[string]float64 {
	all := make([]float64, 0, len(rs))
	per := map[string][]float64{}
	for _, r := range rs {
		v := metricOf(m, r)
		all = append(all, v)
		per[machine(r)] = append(per[machine(r)], v)
	}
	mid := median(all)
	f := map[string]float64{}
	for h, vs := range per {
		if len(vs) >= 5 && mid > 0 {
			if hm := median(vs); hm > 0 {
				f[h] = hm / mid
			}
		}
	}
	return f
}

func findIn(rs []ledger.JobRun, m string, o Options) []Change {
	if len(rs) < 2*o.MinRuns {
		return nil
	}
	hf := hostFactors(rs, m)
	x := make([]float64, 0, len(rs))
	keep := make([]ledger.JobRun, 0, len(rs))
	for _, r := range rs {
		v := metricOf(m, r)
		if v <= 0 {
			continue
		}
		if f := hf[machine(r)]; f > 0 {
			v /= f
		}
		x = append(x, math.Log(v))
		keep = append(keep, r)
	}
	rs = keep
	if len(rs) < 2*o.MinRuns {
		return nil
	}
	cps := segment(x, 0, len(x), o, 0)
	sort.Ints(cps)
	minStep := math.Log1p(o.MinEffect)
	bounds, level, drop := levels(rs, x, cps, minStep, o)
	if !o.Plain {
		// Only the steps that are listed bound the levels, and a level held
		// for less than a side's span between two steps is not one: the
		// later step goes, so a regression that a slow spell hid at first is
		// one step, from its start.
		cps, bounds, level, drop = settle(rs, x, cps, minStep, o)
	}
	newest := median(slices.Clone(x[max(0, len(x)-o.MinRuns):]))
	var out []Change
	for i, k := range cps {
		d := level[i+1] - level[i]
		if drop[i] || math.Abs(d) < minStep {
			continue
		}
		lo, hi := max(bounds[i], k-o.Local), min(bounds[i+2], k+o.Local)
		c := Change{Before: math.Exp(level[i]), After: math.Exp(level[i+1]), Ratio: math.Expm1(d),
			RunsBefore: k - bounds[i], RunsAfter: bounds[i+2] - k, Z: mannWhitneyZ(x[lo:k], x[k:hi]), Now: math.Exp(newest)}
		c.Holds = math.Abs(newest-level[i]) >= minStep && math.Signbit(newest-level[i]) == math.Signbit(d)
		// A step is flat on both sides; a job that creeps up climbs on both.
		a, b := max(bounds[i], k-4*o.Local), min(bounds[i+2], k+4*o.Local)
		drift := theilSen(x[a:k])*float64(k-a) + theilSen(x[k:b])*float64(b-k)
		c.Gradual = math.Signbit(drift) == math.Signbit(d) && math.Abs(drift) >= gradualDrift*math.Abs(d)
		first, lastBefore := attribute(rs, x, k, level[i], level[i+1], lo, hi)
		c.First = refOf(rs[first])
		if lastBefore >= 0 {
			c.LastBefore = refOf(rs[lastBefore])
		}
		c.PR, c.Suspects = culpritPR(rs, x, first, level[i], level[i+1])
		if m == "time" {
			c.Steps = stepDeltas(rs[lo:k], rs[k:hi])
		}
		if !o.Plain {
			c.SpanBefore = codeTime(rs[k-1]).Sub(codeTime(rs[bounds[i]]))
			c.SpanAfter = codeTime(rs[bounds[i+2]-1]).Sub(codeTime(rs[k]))
			// Like with like: the step on the machines that ran the job on
			// both sides. When a third of the runs or more were on others,
			// and those machines do not show it (or there are none): the
			// job moved.
			if like, other, ok := sameMachineStep(rs, x, lo, k, hi); !ok || other >= 1.0/3 && (math.Abs(like) < minStep || math.Signbit(like) != math.Signbit(d)) {
				c.Moved = &Move{From: machines(rs[lo:k]), To: machines(rs[k:hi])}
			}
			c.Host, c.OneHost, c.OnHost = stepHost(rs, x, lo, k, hi, d, o)
		}
		out = append(out, c)
	}
	return out
}

// levels: the bounds of the segments between change points, the level
// (median) of each, and the steps dropped as a bad spell: a step that comes
// back within Transient to where it was.
func levels(rs []ledger.JobRun, x []float64, cps []int, minStep float64, o Options) (bounds []int, level []float64, drop []bool) {
	bounds = append(append([]int{0}, cps...), len(x))
	level = make([]float64, len(bounds)-1)
	for i := range level {
		level[i] = median(slices.Clone(x[bounds[i]:bounds[i+1]]))
	}
	drop = make([]bool, len(cps))
	for i := 0; i+1 < len(cps); i++ {
		a, b := cps[i], cps[i+1]
		up := level[i+1] - level[i]
		back := level[i+2] - level[i+1]
		if math.Abs(up) >= minStep && math.Signbit(up) != math.Signbit(back) && math.Abs(level[i+2]-level[i]) < minStep/2 &&
			codeTime(rs[b]).Sub(codeTime(rs[a])) <= o.Transient {
			drop[i], drop[i+1] = true, true
		}
	}
	return bounds, level, drop
}

// settle keeps the change points that are listed: not a bad spell, a step
// of MinEffect or more, and none that closes a level held for less than
// the sides' span (min of MinBefore and MinAfter) after another. Each round
// takes out one (a bad spell's two) and measures the levels again.
func settle(rs []ledger.JobRun, x []float64, cps []int, minStep float64, o Options) ([]int, []int, []float64, []bool) {
	side := min(o.MinBefore, o.MinAfter)
	for {
		bounds, level, drop := levels(rs, x, cps, minStep, o)
		gone := -1
		for i := range cps {
			if drop[i] {
				cps = slices.Delete(slices.Clone(cps), i, i+2)
				gone = i
				break
			}
		}
		if gone >= 0 {
			continue
		}
		small, smallest := -1, minStep
		for i := range cps {
			if d := math.Abs(level[i+1] - level[i]); d < smallest {
				small, smallest = i, d
			}
		}
		if small >= 0 {
			cps = slices.Delete(slices.Clone(cps), small, small+1)
			continue
		}
		short := -1
		for i := 1; side > 0 && i < len(cps); i++ {
			if codeTime(rs[cps[i]-1]).Sub(codeTime(rs[cps[i-1]])) < side {
				short = i
				break
			}
		}
		if short >= 0 {
			cps = slices.Delete(slices.Clone(cps), short, short+1)
			continue
		}
		return cps, bounds, level, drop
	}
}

// segment finds change points in x[lo:hi] by binary segmentation: the split
// that leaves the least absolute deviation from each side's median, kept when
// the runs around it differ by a rank test and by enough.
func segment(x []float64, lo, hi int, o Options, depth int) []int {
	if hi-lo < 2*o.MinRuns || depth > 6 {
		return nil
	}
	pre, suf := prefixL1(x[lo:hi], false), prefixL1(x[lo:hi], true)
	n := hi - lo
	best, bestCost := -1, math.Inf(1)
	for p := o.MinRuns; p <= n-o.MinRuns; p++ {
		if c := pre[p] + suf[n-p]; c < bestCost {
			best, bestCost = lo+p, c
		}
	}
	if best < 0 {
		return nil
	}
	a, b := max(lo, best-o.Local), min(hi, best+o.Local)
	z := mannWhitneyZ(x[a:best], x[best:b])
	d := median(slices.Clone(x[a:best])) - median(slices.Clone(x[best:b]))
	if math.Abs(z) < o.Z || math.Abs(d) < math.Log1p(o.MinEffect)*0.8 {
		return nil
	}
	out := []int{best}
	out = append(out, segment(x, lo, best, o, depth+1)...)
	return append(out, segment(x, best, hi, o, depth+1)...)
}

// attribute picks the first run after a change. Runs of pull requests
// interleave with main's, and a pull request's runs test its own change
// before it is merged; the commit is named by the branch that runs on every
// push: of the runs that are not of a pull request around the split, the one
// where the two levels divide best. Without such runs, the split's own run.
func attribute(rs []ledger.JobRun, x []float64, k int, before, after float64, lo, hi int) (first, lastBefore int) {
	var idx []int
	for i := lo; i < hi; i++ {
		if rs[i].PR == 0 && rs[i].Event != "pull_request" {
			idx = append(idx, i)
		}
	}
	if len(idx) < 2 {
		return k, k - 1
	}
	best, bestCost := -1, math.Inf(1)
	for j := 0; j <= len(idx); j++ {
		c := 0.0
		for n, i := range idx {
			if n < j {
				c += math.Abs(x[i] - before)
			} else {
				c += math.Abs(x[i] - after)
			}
		}
		if c < bestCost-1e-12 {
			best, bestCost = j, c
		}
	}
	if best == len(idx) {
		return k, k - 1
	}
	first = idx[best]
	lastBefore = -1
	if best > 0 {
		lastBefore = idx[best-1]
	}
	return first, lastBefore
}

// culpritPR: a pull request whose runs before the first slower run on main
// were already at the new level — all of them, at least two, and its last
// within two days of it — is the change's likely source. When more than one
// pull request fits, none is named. suspects: every pull request that fits
// with one run or more (one run can be at the new level by chance).
func culpritPR(rs []ledger.JobRun, x []float64, first int, before, after float64) (int, []int) {
	at := codeTime(rs[first])
	type tally struct {
		n, near int
		last    time.Time
	}
	prs := map[int]*tally{}
	for i := range first {
		r := rs[i]
		if r.PR == 0 || at.Sub(codeTime(r)) > 7*24*time.Hour {
			continue
		}
		t := prs[r.PR]
		if t == nil {
			t = &tally{}
			prs[r.PR] = t
		}
		t.n++
		if math.Abs(x[i]-after) < math.Abs(x[i]-before) {
			t.near++
		}
		t.last = codeTime(r)
	}
	found, many := 0, false
	var suspects []int
	for pr, t := range prs {
		if t.near != t.n || at.Sub(t.last) > 48*time.Hour {
			continue
		}
		suspects = append(suspects, pr)
		if t.n >= 2 {
			many = many || found != 0
			found = pr
		}
	}
	slices.Sort(suspects)
	if many {
		found = 0
	}
	return found, suspects
}

// stepDeltas compares each step's median time on the two sides; the steps
// that grew the most come first.
func stepDeltas(before, after []ledger.JobRun) []StepDelta {
	med := func(rs []ledger.JobRun) (map[string]float64, []string) {
		per := map[string][]float64{}
		var order []string
		for _, r := range rs {
			for _, s := range r.Steps {
				if _, ok := per[s.Name]; !ok {
					order = append(order, s.Name)
				}
				per[s.Name] = append(per[s.Name], s.Sec)
			}
		}
		out := map[string]float64{}
		for n, v := range per {
			if len(v) >= 3 {
				out[n] = median(v)
			}
		}
		return out, order
	}
	b, _ := med(before)
	a, order := med(after)
	var out []StepDelta
	for _, n := range order {
		bv, ok1 := b[n]
		av, ok2 := a[n]
		if ok1 && ok2 {
			out = append(out, StepDelta{Name: n, Before: bv, After: av})
		}
	}
	slices.SortStableFunc(out, func(p, q StepDelta) int { return cmp.Compare(q.After-q.Before, p.After-p.Before) })
	return out
}

// prefixL1: out[p] is the sum of absolute deviations from the median of
// the first p values of x (of the last p, reversed), for every p, in
// O(n log n) with a Fenwick tree over the values' ranks.
func prefixL1(x []float64, reversed bool) []float64 {
	n := len(x)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(x[a], x[b]) })
	rank := make([]int, n)
	sorted := make([]float64, n)
	for r, i := range order {
		rank[i], sorted[r] = r, x[i]
	}
	cnt := make([]int, n+1)
	sum := make([]float64, n+1)
	add := func(r int, v float64) {
		for i := r + 1; i <= n; i += i & -i {
			cnt[i]++
			sum[i] += v
		}
	}
	// kth: the rank of the k-th smallest (1-based) and the count and sum of
	// the values up to it.
	logn := 1
	for 1<<logn <= n {
		logn++
	}
	kth := func(k int) (r, c int, s float64) {
		pos := 0
		for b := logn; b >= 0; b-- {
			if next := pos + 1<<b; next <= n && c+cnt[next] < k {
				pos = next
				c += cnt[next]
				s += sum[next]
			}
		}
		// pos is the count of ranks below the k-th; the k-th is rank pos.
		return pos, c + 1, s + sorted[pos]
	}
	out := make([]float64, n+1)
	total := 0.0
	for p := 1; p <= n; p++ {
		i := p - 1
		if reversed {
			i = n - p
		}
		add(rank[i], x[i])
		total += x[i]
		r, c, s := kth((p + 1) / 2)
		m := sorted[r]
		out[p] = m*float64(c) - s + (total - s) - m*float64(p-c)
	}
	return out
}

// theilSen is the median slope between every two values, per index.
func theilSen(x []float64) float64 {
	n := len(x)
	if n < 3 {
		return 0
	}
	sl := make([]float64, 0, n*(n-1)/2)
	for i := range n {
		for j := i + 1; j < n; j++ {
			sl = append(sl, (x[j]-x[i])/float64(j-i))
		}
	}
	return median(sl)
}

// median sorts v in place.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// mannWhitneyZ is the rank-sum test's z-score of b against a (positive when
// b is larger), with ties averaged and corrected for.
func mannWhitneyZ(a, b []float64) float64 {
	n1, n2 := len(a), len(b)
	if n1 == 0 || n2 == 0 {
		return 0
	}
	type obs struct {
		v float64
		b bool
	}
	all := make([]obs, 0, n1+n2)
	for _, v := range a {
		all = append(all, obs{v, false})
	}
	for _, v := range b {
		all = append(all, obs{v, true})
	}
	slices.SortFunc(all, func(p, q obs) int { return cmp.Compare(p.v, q.v) })
	rankB, tie := 0.0, 0.0
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		r := float64(i+j+1) / 2 // average of ranks i+1..j
		for k := i; k < j; k++ {
			if all[k].b {
				rankB += r
			}
		}
		t := float64(j - i)
		tie += t*t*t - t
		i = j
	}
	N := float64(n1 + n2)
	u := rankB - float64(n2*(n2+1))/2
	mu := float64(n1*n2) / 2
	sigma := math.Sqrt(float64(n1*n2) / 12 * (N + 1 - tie/(N*(N-1))))
	if sigma == 0 {
		return 0
	}
	return (u - mu) / sigma
}
