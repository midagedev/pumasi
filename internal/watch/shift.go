package watch

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// A step is the machine's, not the job's, when:
//
//   - the job moved: its runs on the two sides ran on different machines
//     (host and runner class), and the runs on a machine it ran on both
//     before and after do not show the step (Change.Moved);
//   - the job runs on several hosts and the step showed on one host's runs
//     and not on another's of the same days (Change.OneHost);
//   - several jobs stepped the same way at the same moment on the same
//     hosts, and something says it was not one commit: they are of more
//     than one workflow, or their first runs at the new level are of
//     different runs, or the jobs of those runs on other hosts did not step.
//
// Such steps are a Shift, one line, not a change of each job.

// Shift is a step that came with the machine, not the code.
type Shift struct {
	// Kind: "host" (one or more jobs stepped together on the same hosts)
	// or "moved" (a job went to another kind of machine).
	Kind  string   `json:"kind"`
	Hosts []string `json:"hosts"`
	// Slower: the jobs got slower (or bigger); else faster.
	Slower bool `json:"slower"`
	// At: the first run at the new level, of the earliest of its jobs.
	At time.Time `json:"at"`
	// Until: when it went the other way again (zero: it holds); Back: to
	// about where it was.
	Until time.Time `json:"until,omitzero"`
	Back  bool      `json:"back,omitempty"`
	// Jobs: how many jobs; Low and High: their steps' range, of time when
	// they stepped in time, else of the number they stepped in.
	Jobs    int      `json:"jobs"`
	Low     float64  `json:"low"`
	High    float64  `json:"high"`
	Changes []Change `json:"changes"`
}

// shiftSlack: a host's steps are one moment when the spans between each
// job's last run before and first run after meet, give or take this.
const shiftSlack = time.Hour

// shiftPlace: a job whose runs before and after a step are further apart
// than this cannot say when it stepped, so it is not set beside others. A
// job that runs once a night still can.
const shiftPlace = 36 * time.Hour

// shiftJobs: this many jobs stepping together are a host's step.
const shiftJobs = 3

// FindAll looks for changes in every job of the runs, and tells the job's
// from the machine's: jobs are the changes of the jobs whose sides span
// long enough (Options.MinBefore, MinAfter), shifts the machines' steps.
func FindAll(runs []ledger.JobRun, o Options) (jobs []Change, shifts []Shift) {
	o = o.withDefaults()
	raw := find(runs, o)
	if o.Plain {
		return raw, nil
	}
	units := unitsOf(raw)
	var rest, moved []unit
	for _, u := range units {
		if u.lead().Moved != nil {
			moved = append(moved, u)
			continue
		}
		rest = append(rest, u)
	}
	shifts = append(shifts, moves(moved)...)
	hostShifts, left := hostSteps(rest, runs)
	shifts = append(shifts, hostShifts...)
	for _, u := range left {
		for _, c := range u {
			if c.SpanBefore >= o.MinBefore && c.SpanAfter >= o.MinAfter {
				jobs = append(jobs, c)
			}
		}
	}
	slices.SortFunc(jobs, func(a, b Change) int {
		return cmp.Or(a.First.At.Compare(b.First.At), cmp.Compare(a.Job, b.Job), cmp.Compare(a.Metric, b.Metric))
	})
	slices.SortFunc(shifts, func(a, b Shift) int { return a.At.Compare(b.At) })
	return jobs, shifts
}

// unit is a job's changes that one run begins (time, memory, cpu).
type unit []Change

// lead is the change a unit is read by: its time's, else its first.
func (u unit) lead() Change {
	for _, c := range u {
		if c.Metric == "time" {
			return c
		}
	}
	return u[0]
}

func (u unit) job() string { return u[0].Repo + "\x00" + u[0].Workflow + "\x00" + u[0].Job }

func unitsOf(cs []Change) []unit {
	var out []unit
	for _, g := range GroupChanges(cs) {
		out = append(out, unit(g))
	}
	return out
}

// span is when a unit's step happened: between its last run before and its
// first run after, with the slack.
func (u unit) span() (time.Time, time.Time) {
	c := u.lead()
	from := c.LastBefore.At
	if from.IsZero() || from.After(c.First.At) {
		from = c.First.At
	}
	return from.Add(-shiftSlack), c.First.At.Add(shiftSlack)
}

// hostSteps takes out of the units the steps of hosts: jobs stepping the
// same way at one moment on the same hosts, and steps that showed on one
// host only.
func hostSteps(units []unit, runs []ledger.JobRun) ([]Shift, []unit) {
	slices.SortFunc(units, func(a, b unit) int { return a.lead().First.At.Compare(b.lead().First.At) })
	used := make([]bool, len(units))
	var out []Shift
	for i := range units {
		if used[i] {
			continue
		}
		// The units that step the same way as this one and whose spans all
		// meet it and each other.
		group := []int{i}
		from, to := units[i].span()
		if to.Sub(from) > shiftPlace {
			continue
		}
		slower, host := units[i].lead().Slower(), units[i].lead().Host
		for j := i + 1; j < len(units); j++ {
			if used[j] || units[j].lead().Slower() != slower || units[j].lead().Host != host {
				continue
			}
			f, t := units[j].span()
			if f.After(to) || t.Before(from) || t.Sub(f) > shiftPlace {
				continue
			}
			from, to = maxTime(from, f), minTime(to, t)
			group = append(group, j)
		}
		var us []unit
		for _, g := range group {
			us = append(us, units[g])
		}
		if !machineStep(us, runs) {
			continue
		}
		for _, g := range group {
			used[g] = true
		}
		out = append(out, shiftOf("host", us))
	}
	out = together(out)
	// Steps that showed on one host's runs only, alone.
	var left []unit
	for i, u := range units {
		switch {
		case used[i]:
		case u.lead().OneHost:
			out = append(out, shiftOf("host", []unit{u}))
		default:
			left = append(left, u)
		}
	}
	return backAgain(out), left
}

// moves: jobs that moved to the same machines the same way, each within
// two hours of the one before, are one line.
func moves(us []unit) []Shift {
	slices.SortFunc(us, func(a, b unit) int { return a.lead().First.At.Compare(b.lead().First.At) })
	var out []Shift
	var group []unit
	flush := func() {
		if len(group) > 0 {
			out = append(out, shiftOf("moved", group))
		}
		group = nil
	}
	for _, u := range us {
		if n := len(group); n > 0 {
			last := group[n-1].lead()
			c := u.lead()
			if c.Slower() != last.Slower() || !slices.Equal(c.Moved.To, last.Moved.To) || c.First.At.Sub(last.First.At) > 2*shiftSlack {
				flush()
			}
		}
		group = append(group, u)
	}
	flush()
	return out
}

// together: hosts' steps the same way within two hours of each other are
// one line (every host changed, or the hosts share what changed).
func together(ss []Shift) []Shift {
	slices.SortFunc(ss, func(a, b Shift) int { return a.At.Compare(b.At) })
	var out []Shift
	for _, s := range ss {
		if n := len(out); n > 0 && out[n-1].Slower == s.Slower && s.At.Sub(out[n-1].At) <= 2*shiftSlack {
			out[n-1] = shiftOf("host", unitsOf(append(slices.Clone(out[n-1].Changes), s.Changes...)))
			continue
		}
		out = append(out, s)
	}
	return out
}

// machineStep says whether units that stepped together on one host (or on
// every host the jobs ran on: Host "") are the host's: enough jobs, and not
// one commit.
func machineStep(us []unit, runs []ledger.JobRun) bool {
	jobs, wfs, firsts, hosts := map[string]bool{}, map[string]bool{}, map[int64]bool{}, map[string]bool{}
	oneHost := 0
	for _, u := range us {
		c := u.lead()
		jobs[u.job()] = true
		wfs[c.Repo+"\x00"+c.Workflow] = true
		firsts[c.First.RunID] = true
		if c.OneHost {
			oneHost++
		}
		hosts[c.Host] = true
	}
	if len(jobs) < shiftJobs {
		return false
	}
	if oneHost > 0 || len(wfs) > 1 || len(firsts) > 1 {
		return true
	}
	if hosts[""] {
		return false // every host, one workflow, one run: as a commit would
	}
	// One workflow, one run: a commit could do that too, unless the jobs of
	// that run on other hosts did not step.
	return othersQuiet(us, runs, hosts)
}

// othersQuiet: some job of the same workflow ran on other hosts, with runs
// before and after the moment, and did not step then.
func othersQuiet(us []unit, runs []ledger.JobRun, hosts map[string]bool) bool {
	c := us[0].lead()
	at := c.First.At
	stepped := map[string]bool{}
	for _, u := range us {
		stepped[u[0].Job] = true
	}
	type side struct{ before, after int }
	seen := map[string]*side{}
	for _, r := range runs {
		if r.Repo != c.Repo || r.Workflow != c.Workflow || stepped[r.Job] || hosts[r.Host] || r.Result != "succeeded" {
			continue
		}
		s := seen[r.Job]
		if s == nil {
			s = &side{}
			seen[r.Job] = s
		}
		d := r.StartedAt.Sub(at)
		switch {
		case d < 0 && d > -24*time.Hour:
			s.before++
		case d >= 0 && d < 24*time.Hour:
			s.after++
		}
	}
	for _, s := range seen {
		if s.before >= 3 && s.after >= 3 {
			return true
		}
	}
	return false
}

func shiftOf(kind string, us []unit) Shift {
	s := Shift{Kind: kind, Slower: us[0].lead().Slower(), Low: math.Inf(1), High: math.Inf(-1)}
	hosts := map[string]bool{}
	jobs := map[string]bool{}
	for _, u := range us {
		c := u.lead()
		r := c.Ratio
		if c.OneHost {
			r = c.OnHost
		}
		s.Low, s.High = math.Min(s.Low, r), math.Max(s.High, r)
		if s.At.IsZero() || c.First.At.Before(s.At) {
			s.At = c.First.At
		}
		if c.Host != "" {
			hosts[c.Host] = true
		}
		if c.Moved != nil {
			for _, m := range append(slices.Clone(c.Moved.From), c.Moved.To...) {
				hosts[hostOfMachine(m)] = true
			}
		}
		jobs[u.job()] = true
		s.Changes = append(s.Changes, u...)
	}
	s.Jobs = len(jobs)
	for h := range hosts {
		s.Hosts = append(s.Hosts, h)
	}
	slices.Sort(s.Hosts)
	return s
}

// backAgain: a host's step followed by one the other way on the same
// hosts, of most of the same jobs, is one spell: "slower from … to …".
func backAgain(ss []Shift) []Shift {
	slices.SortFunc(ss, func(a, b Shift) int { return a.At.Compare(b.At) })
	gone := make([]bool, len(ss))
	for i := range ss {
		a := &ss[i]
		if gone[i] || a.Kind != "host" {
			continue
		}
		for j := i + 1; j < len(ss); j++ {
			b := &ss[j]
			if gone[j] || b.Kind != "host" || b.Slower == a.Slower || !overlap(a.Hosts, b.Hosts) {
				continue
			}
			ja, jb := shiftJobSet(*a), shiftJobSet(*b)
			both, net := 0, []float64{}
			for k := range jb {
				if ra, ok := ja[k]; ok {
					both++
					net = append(net, math.Log1p(ra)+math.Log1p(jb[k]))
				}
			}
			if 2*both < len(ja) {
				continue
			}
			a.Until = b.At
			a.Back = math.Abs(median(net)) < math.Log1p(0.10)
			if both == len(jb) {
				a.Changes = append(a.Changes, b.Changes...)
				gone[j] = true
			}
			break
		}
	}
	var out []Shift
	for i, s := range ss {
		if !gone[i] {
			out = append(out, s)
		}
	}
	return out
}

// shiftJobSet: each job of a shift and its lead step.
func shiftJobSet(s Shift) map[string]float64 {
	out := map[string]float64{}
	for _, u := range unitsOf(s.Changes) {
		c := u.lead()
		if _, ok := out[u.job()]; ok {
			continue
		}
		if c.OneHost {
			out[u.job()] = c.OnHost
		} else {
			out[u.job()] = c.Ratio
		}
	}
	return out
}

func overlap(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func hostOfMachine(m string) string {
	for i := range len(m) {
		if m[i] == '/' {
			return m[:i]
		}
	}
	return m
}

// sameMachineStep is the step (after's median against before's, as logs)
// of the runs in [lo,hi) on the machines with three or more runs on both
// sides of k; other is the share of the runs on either side (the larger)
// that were on other machines; ok is false when there is no such machine.
func sameMachineStep(rs []ledger.JobRun, x []float64, lo, k, hi int) (step, other float64, ok bool) {
	nb, na := map[string]int{}, map[string]int{}
	for i := lo; i < hi; i++ {
		if i < k {
			nb[machine(rs[i])]++
		} else {
			na[machine(rs[i])]++
		}
	}
	var before, after []float64
	for i := lo; i < hi; i++ {
		m := machine(rs[i])
		if nb[m] < 3 || na[m] < 3 {
			continue
		}
		if i < k {
			before = append(before, x[i])
		} else {
			after = append(after, x[i])
		}
	}
	if len(before) == 0 {
		return 0, 1, false
	}
	other = max(1-float64(len(before))/float64(k-lo), 1-float64(len(after))/float64(hi-k))
	return median(after) - median(before), other, true
}

// machines are the machines that ran a third or more of the runs, the most
// first.
func machines(rs []ledger.JobRun) []string {
	n := map[string]int{}
	for _, r := range rs {
		n[machine(r)]++
	}
	var out []string
	for m, c := range n {
		if 3*c >= len(rs) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(n[b]-n[a], cmp.Compare(a, b)) })
	return out
}

// stepHost: the host a step was on. When the job ran on several hosts with
// MinRuns runs on each side, and the step showed on one host's runs (twice
// MinEffect or more, the rank test's z 3 or more) while another host's
// stayed flat (z under 1.5, a quarter of that step or less), that host and
// oneHost. Else the host that ran two thirds or more of the runs around
// it, or none.
func stepHost(rs []ledger.JobRun, x []float64, lo, k, hi int, d float64, o Options) (host string, oneHost bool, onHost float64) {
	type sides struct{ before, after []float64 }
	per := map[string]*sides{}
	count := map[string]int{}
	for i := lo; i < hi; i++ {
		h := rs[i].Host
		count[h]++
		s := per[h]
		if s == nil {
			s = &sides{}
			per[h] = s
		}
		if i < k {
			s.before = append(s.before, x[i])
		} else {
			s.after = append(s.after, x[i])
		}
	}
	var stepped []string
	var steps, quietSteps []float64
	for h, s := range per {
		if len(s.before) < o.MinRuns || len(s.after) < o.MinRuns {
			continue
		}
		z := mannWhitneyZ(s.before, s.after)
		dh := median(slices.Clone(s.after)) - median(slices.Clone(s.before))
		switch {
		case math.Signbit(dh) == math.Signbit(d) && math.Abs(dh) >= 2*math.Log1p(o.MinEffect) && math.Abs(z) >= 3:
			stepped = append(stepped, h)
			steps = append(steps, dh)
		case math.Abs(z) < 1.5:
			quietSteps = append(quietSteps, math.Abs(dh))
		}
	}
	if len(stepped) == 1 && len(quietSteps) > 0 && slices.Max(quietSteps) <= math.Abs(steps[0])/4 {
		return stepped[0], true, math.Expm1(steps[0])
	}
	for h, n := range count {
		if 3*n >= 2*(hi-lo) {
			return h, false, 0
		}
	}
	return "", false, 0
}
