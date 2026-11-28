package race

import (
	"fmt"
	"math"
	"time"

	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/size"
)

// rans is a stream's jobs as the ledger keeps them, came and ended (in
// seconds; ended 0 = not yet), for decide.LearnNeeds.
func rans(jobs []Job, came, ended []float64) []decide.Ran {
	var out []decide.Ran
	for i, j := range jobs {
		if came[i] < 0 {
			continue
		}
		r := decide.Ran{Run: runOf(j), Workflow: kindOf(j.Kind).Workflow, Job: j.Kind, Class: kindOf(j.Kind).Class, Came: at(came[i])}
		if ended[i] > 0 {
			r.Ended = at(ended[i])
		}
		out = append(out, r)
	}
	return out
}

// Days is how many days before the stream the ledger keeps for
// decide.LearnNeeds: a chain that runs once a day is seen in two of them.
const Days = 3

// before is what the ledger has of the days before: streams like this one,
// the jobs as they came and ended when nobody waited.
func before(sc Scenario) []decide.Ran {
	var out []decide.Ran
	for d := range Days {
		prev := sc.Before()
		prev.Seed += 7919 * uint64(d+1)
		jobs := Stream(prev)
		came, ended := make([]float64, len(jobs)), make([]float64, len(jobs))
		byID := map[int]int{}
		for i, j := range jobs {
			byID[j.ID] = i
			came[i] = -1
		}
		var when func(i int)
		when = func(i int) {
			if came[i] >= 0 {
				return
			}
			j := jobs[i]
			came[i] = j.At
			if j.After != 0 {
				p := byID[j.After]
				when(p)
				came[i] = ended[p] + j.Delay
			}
			ended[i] = came[i] + j.Dur
		}
		for i := range jobs {
			when(i)
		}
		for _, r := range rans(jobs, came, ended) {
			r.Run = fmt.Sprintf("day%d/%s", d+1, r.Run)
			out = append(out, r)
		}
	}
	return out
}

// ahead is the classes with the jobs expected within lead of t
// (decide.Soon), sized for the largest of them, from what the running jobs
// are expected to bring when they end.
func ahead(classes []decide.Class, sc Scenario, needs []decide.Need, jobs []Job, arr *arrivals, ends []float64, runners []runner, measured map[string][]size.Sample, t float64) []decide.Class {
	if len(needs) == 0 {
		return classes
	}
	now := at(t)
	var going []decide.Going
	for _, r := range runners {
		if r.run == nil {
			continue
		}
		j := jobs[r.run.job]
		d, ok := size.Runtime(measured[j.Kind])
		if !ok {
			continue
		}
		going = append(going, decide.Going{Run: runOf(j), Workflow: kindOf(j.Kind).Workflow, Job: j.Kind, Ends: decide.EndOf(at(r.run.started), now, d)})
	}
	for i, e := range ends {
		if e > 0 && t-e < decide.NeedGap.Seconds() {
			going = append(going, decide.Going{Run: runOf(jobs[i]), Workflow: kindOf(jobs[i].Kind).Workflow, Job: jobs[i].Kind, Ends: at(e)})
		}
	}
	came := map[[2]string]bool{}
	for _, i := range arr.came() {
		came[[2]string{runOf(jobs[i]), jobs[i].Kind}] = true
	}
	exp := decide.Expected(going, needs, func(run, job string) bool { return came[[2]string{run, job}] })
	lead := time.Duration((sc.Provision + Tick) * float64(time.Second))
	n, why := decide.Soon(exp, now, lead)
	if len(n) == 0 {
		return classes
	}
	out := append([]decide.Class(nil), classes...)
	for i := range out {
		c := &out[i]
		c.Soon, c.SoonFor = n[c.Name], why[c.Name]
		for _, e := range exp {
			if e.Class == c.Name && e.At.Sub(now) <= lead {
				est := estimate(measured, e.Job)
				c.SoonCPUs, c.SoonMem = math.Max(c.SoonCPUs, est.CPUs), max(c.SoonMem, est.MemBytes)
			}
		}
	}
	return out
}

// runOf is the workflow run a job is of, as the ledger names it: a push
// starts one run of each workflow.
func runOf(j Job) string { return fmt.Sprintf("%s#%d", kindOf(j.Kind).Workflow, j.Run) }
