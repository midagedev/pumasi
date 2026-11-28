package controller

import (
	"fmt"
	"time"

	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

const (
	// needHistory: how far back the ledger is read for which job follows
	// which; needEvery: how often.
	needHistory = 7 * 24 * time.Hour
	needEvery   = 10 * time.Minute
	// recentRuns: how far back the jobs running now and those just ended
	// are looked for.
	recentRuns = 6 * time.Hour
)

func ranOf(j ledger.RunJob) decide.Ran {
	return decide.Ran{Run: fmt.Sprintf("%s#%d", j.Repo, j.RunID), Workflow: j.Workflow, Job: j.Job, Class: j.Class, Came: j.Came, Ended: j.Ended}
}

// learnNeeds learns again which job follows which, needEvery at most.
func (c *Controller) learnNeeds(now time.Time) {
	if !c.needsAt.IsZero() && now.Sub(c.needsAt) < needEvery {
		return
	}
	c.needsAt = now
	jobs, err := c.ledger.RunJobs(now.Add(-needHistory))
	if err != nil {
		c.log.Error("ledger run jobs", "err", err)
		return
	}
	var runs []decide.Ran
	for _, j := range jobs {
		runs = append(runs, ranOf(j))
	}
	c.needs = decide.LearnNeeds(runs)
}

// soonLocked is, by class, the jobs expected within lead of now that no
// runner is up for yet (decide.Soon), the reason for the first, and what
// the largest of them is expected to use: what the jobs running now, or
// just ended, bring by c.needs. recent: RunJobs of recentRuns.
func (c *Controller) soonLocked(now time.Time, recent []ledger.RunJob, lead time.Duration) (map[string]int, map[string][]string, map[string]size.Estimate) {
	if len(c.needs) == 0 {
		return nil, nil, nil
	}
	var going []decide.Going
	came := map[[2]string]bool{}
	for _, j := range recent {
		r := ranOf(j)
		came[[2]string{r.Run, r.Job}] = true
		switch {
		case !j.Ended.IsZero():
			if now.Sub(j.Ended) < decide.NeedGap {
				going = append(going, decide.Going{Run: r.Run, Workflow: r.Workflow, Job: r.Job, Ends: j.Ended})
			}
		case !j.Started.IsZero():
			cl := c.classByName(j.Class)
			if cl == nil {
				continue
			}
			d, ok := c.runtimeLocked(cl.cfg, ledger.KeyOf(j.Repo, j.Workflow, j.Job))
			// A job far past its time has likely lost its runner without a word.
			if !ok || now.Sub(j.Started) > 3*d {
				continue
			}
			going = append(going, decide.Going{Run: r.Run, Workflow: r.Workflow, Job: r.Job, Ends: decide.EndOf(j.Started, now, d)})
		}
	}
	exp := decide.Expected(going, c.needs, func(run, job string) bool { return came[[2]string{run, job}] })
	n, why := decide.Soon(exp, now, lead)
	est := map[string]size.Estimate{}
	repo := map[string]string{}
	for _, j := range recent {
		repo[ranOf(j).Run] = j.Repo
	}
	for _, e := range exp {
		cl := c.classByName(e.Class)
		if cl == nil || e.At.Sub(now) > lead {
			continue
		}
		x := c.estimateLocked(cl.cfg, ledger.KeyOf(repo[e.Run], e.Workflow, e.Job))
		cur := est[e.Class]
		cur.CPUs, cur.MemBytes = max(cur.CPUs, x.CPUs), max(cur.MemBytes, x.MemBytes)
		est[e.Class] = cur
	}
	return n, why, est
}
