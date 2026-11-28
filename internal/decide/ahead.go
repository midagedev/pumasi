package decide

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// A job that needs another comes only once that one ended: GitHub asks for
// a runner then, and a runner started then makes the job wait the time it
// takes to come up. The runs before tell which job follows which, and how
// soon; the run times tell when the one running now ends. A runner started
// that much ahead is up as the job comes.

// Ran is one job of a workflow run, as the ledger keeps it.
type Ran struct {
	Run      string // the workflow run
	Workflow string
	Job      string
	Class    string
	Came     time.Time // when it asked for a runner
	Ended    time.Time // zero: it has not ended
}

// Need: in Workflow, Job (a job of Class) comes Delay after After ends, as
// it did in Seen runs.
type Need struct {
	Workflow string
	After    string
	Job      string
	Class    string
	Delay    time.Duration
	Seen     int
}

// NeedGap: a job that came more than this after the last job of its run to
// end did not come for that one: it was asked for by hand, re-run, or held
// by an approval.
const NeedGap = time.Minute

// LearnNeeds is which job follows which: a job that came later than the
// run's first jobs (Burst after them) came for the job of its run that
// ended last before it, NeedGap at most. A pair is kept when it was seen in
// two runs and in at least half of the runs where After ended; Delay is
// its median.
func LearnNeeds(runs []Ran) []Need {
	byRun := map[string][]Ran{}
	for _, r := range runs {
		byRun[r.Run] = append(byRun[r.Run], r)
	}
	type key struct{ wf, after, job string }
	delays := map[key][]time.Duration{}
	class := map[key]string{}
	ended := map[[2]string]int{} // (workflow, job) -> runs where it ended
	for _, rs := range byRun {
		first := slices.MinFunc(rs, func(a, b Ran) int { return a.Came.Compare(b.Came) }).Came
		for _, a := range rs {
			if !a.Ended.IsZero() {
				ended[[2]string{a.Workflow, a.Job}]++
			}
		}
		for _, b := range rs {
			if b.Came.Sub(first) <= Burst {
				continue
			}
			var after *Ran
			for i, a := range rs {
				if a.Job == b.Job || a.Ended.IsZero() || a.Ended.After(b.Came) || b.Came.Sub(a.Ended) > NeedGap {
					continue
				}
				if after == nil || a.Ended.After(after.Ended) {
					after = &rs[i]
				}
			}
			if after == nil {
				continue
			}
			k := key{b.Workflow, after.Job, b.Job}
			delays[k] = append(delays[k], b.Came.Sub(after.Ended))
			class[k] = b.Class
		}
	}
	var out []Need
	for k, ds := range delays {
		if len(ds) < 2 || 2*len(ds) < ended[[2]string{k.wf, k.after}] {
			continue
		}
		slices.Sort(ds)
		out = append(out, Need{Workflow: k.wf, After: k.after, Job: k.job, Class: class[k], Delay: ds[len(ds)/2], Seen: len(ds)})
	}
	slices.SortFunc(out, func(a, b Need) int {
		return cmp.Or(cmp.Compare(a.Workflow, b.Workflow), cmp.Compare(a.After, b.After), cmp.Compare(a.Job, b.Job))
	})
	return out
}

// Going is a job running now and when it is expected to end (EndOf), or
// one that ended less than NeedGap ago and when it did: what it brings may
// not have come yet.
type Going struct {
	Run      string
	Workflow string
	Job      string
	Ends     time.Time
}

// Coming is a job expected to come: At, Delay after After ends.
type Coming struct {
	Run      string
	Workflow string
	Job      string
	Class    string
	After    string
	At       time.Time
}

// Expected is the jobs the running ones bring when they end, by needs, but
// for those that came already (came[run, job]). One step ahead only: when
// a job that has not come yet ends nobody can say well.
func Expected(going []Going, needs []Need, came func(run, job string) bool) []Coming {
	next := map[[2]string][]Need{}
	for _, n := range needs {
		k := [2]string{n.Workflow, n.After}
		next[k] = append(next[k], n)
	}
	var out []Coming
	for _, g := range going {
		for _, n := range next[[2]string{g.Workflow, g.Job}] {
			if came(g.Run, n.Job) {
				continue
			}
			out = append(out, Coming{Run: g.Run, Workflow: g.Workflow, Job: n.Job, Class: n.Class, After: g.Job, At: g.Ends.Add(n.Delay)})
		}
	}
	slices.SortFunc(out, func(a, b Coming) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Run, b.Run), cmp.Compare(a.Job, b.Job))
	})
	return out
}

// Soon is how many of the expected jobs of each class come within lead of
// now (the time a runner takes to come up), and for the first of them,
// which job after what, in words. A job past its time that has not come is
// still expected: its After ran long (EndOf moves its end on).
func Soon(expected []Coming, now time.Time, lead time.Duration) (map[string]int, map[string]string) {
	n, why := map[string]int{}, map[string]string{}
	for _, e := range expected {
		in := e.At.Sub(now)
		if in > lead {
			continue
		}
		n[e.Class]++
		if why[e.Class] == "" {
			why[e.Class] = fmt.Sprintf("%s expected in ~%ds, when %s ends", e.Job, max(0, int(in.Seconds()+0.5)), e.After)
		}
	}
	return n, why
}
