package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// Run facts (W1): once a job is over, GitHub is asked once for what the scale
// set's messages do not carry — the commit, the branch, the job's number and
// how long each step took. The messages already said the event, and the
// workflow's ref the branch or the pull request.

const (
	// factsDelay: GitHub fills in the last step a moment after the job ends.
	factsDelay = 15 * time.Second
	// factsMaxAge: a job that ended longer ago than this (the controller was
	// away) is not asked about any more.
	factsMaxAge = 24 * time.Hour
	factsEvery  = 20 * time.Second
)

// ghStep and ghRunJob are what GitHub's REST API says of a job of a run.
type ghStep struct {
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

type ghRunJob struct {
	ID         int64    `json:"id"`
	RunAttempt int      `json:"run_attempt"`
	Name       string   `json:"name"`
	RunnerName string   `json:"runner_name"`
	HeadSHA    string   `json:"head_sha"`
	HeadBranch string   `json:"head_branch"`
	Steps      []ghStep `json:"steps"`
}

// factsLoop asks for the facts of finished jobs until ctx ends.
func (c *Controller) factsLoop(ctx context.Context) {
	t := time.NewTicker(factsEvery)
	defer t.Stop()
	for {
		c.askFacts(ctx)
		c.pruneSteps()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// askFacts asks GitHub about the jobs that finished since the last time. One
// call lists a run's jobs: the jobs of one run that are waiting together share
// it, and no job is ever asked about twice, whatever the answer.
func (c *Controller) askFacts(ctx context.Context) {
	want, err := c.ledger.WantFacts(c.now(), factsDelay, factsMaxAge, 50)
	if err != nil {
		c.log.Error("ledger facts", "err", err)
		return
	}
	type runKey struct {
		repo string
		run  int64
	}
	byRun := map[runKey][]string{}
	var order []runKey
	for _, w := range want {
		k := runKey{w.Repo, w.RunID}
		if byRun[k] == nil {
			order = append(order, k)
		}
		byRun[k] = append(byRun[k], w.Runner)
	}
	for _, k := range order {
		if ctx.Err() != nil {
			return
		}
		runners := byRun[k]
		for _, r := range runners {
			if err := c.ledger.AskedFacts(r); err != nil {
				c.log.Error("ledger facts", "err", err)
				return
			}
		}
		jobs, err := c.runJobs(ctx, k.repo, k.run)
		for _, r := range runners {
			if err != nil {
				_ = c.ledger.NoFacts(r, firstLine(err.Error()))
				continue
			}
			f, ok := factsOf(jobs, r)
			if !ok {
				_ = c.ledger.NoFacts(r, "not among the run's first 100 jobs")
				continue
			}
			if err := c.ledger.SetFacts(r, f); err != nil {
				c.log.Error("ledger facts", "err", err)
			}
		}
		if err != nil {
			c.log.Debug("run facts", "repo", k.repo, "run", k.run, "err", err)
		}
	}
}

// runJobs lists every job of a run, of every attempt: a runner's name finds
// its job whichever attempt it ran.
func (c *Controller) runJobs(ctx context.Context, repo string, runID int64) ([]ghRunJob, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var page struct {
		Jobs []ghRunJob `json:"jobs"`
	}
	err := c.githubGet(cctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=all&per_page=100", repo, runID), &page)
	return page.Jobs, err
}

// factsOf picks a runner's job out of its run's jobs.
func factsOf(jobs []ghRunJob, runner string) (ledger.Facts, bool) {
	for _, j := range jobs {
		if j.RunnerName != runner {
			continue
		}
		f := ledger.Facts{SHA: j.HeadSHA, Branch: j.HeadBranch, GHJobID: j.ID, Attempt: j.RunAttempt}
		for _, s := range j.Steps {
			f.Steps = append(f.Steps, ledger.Step{Number: s.Number, Name: s.Name, Conclusion: s.Conclusion,
				StartedAt: realTime(s.StartedAt), CompletedAt: realTime(s.CompletedAt)})
		}
		return f, true
	}
	return ledger.Facts{}, false
}

// realTime is zero for GitHub's "no time" (a skipped step's 0001-01-01).
func realTime(t time.Time) time.Time {
	if t.Year() < 2000 {
		return time.Time{}
	}
	return t
}

// pruneSteps deletes step timings older than watch.keepSteps, once an hour.
func (c *Controller) pruneSteps() {
	now := c.now()
	if now.Sub(c.prunedAt) < time.Hour {
		return
	}
	c.prunedAt = now
	n, err := c.ledger.PruneSteps(now.Add(-c.cfg.Watch.KeepSteps.D()))
	if err != nil {
		c.log.Error("ledger prune", "err", err)
	} else if n > 0 {
		c.log.Debug(fmt.Sprintf("deleted %d step timings older than %s", n, c.cfg.Watch.KeepSteps.D()))
	}
}

// recordContention notes, for every runner running a job, whether its host
// was saturated by others during the last round: by cpu (the host's cores
// all but used, or its cpu pressure over the threshold) or by memory
// pressure, while others — other runners or the host's own use — had a real
// part in it. A job that alone fills its host is not contended: that is its
// own time, and it is the same every run.
func (c *Controller) recordContention(now time.Time, live []ledger.Runner) {
	last := c.contendedAt
	c.contendedAt = now
	if last.IsZero() {
		return
	}
	dt := now.Sub(last)
	if dt <= 0 || dt > 3*c.cfg.EvaluateEvery {
		return // the round before was long ago: nothing is known of the time between
	}
	busy := map[string]bool{}
	for _, r := range live {
		if r.State == "busy" {
			busy[r.Name] = true
		}
	}
	for _, h := range c.hosts {
		s := h.snap
		if s == nil || s.CPUs <= 0 {
			continue
		}
		total, running := s.NonRunnerCPU, 0
		for _, ar := range s.Runners {
			total += ar.CPUNow
			if busy[ar.Name] {
				running++
			}
		}
		cpuSat := total >= 0.9*s.CPUs || (c.cfg.PSICPUThreshold > 0 && s.PSI["cpu"] >= c.cfg.PSICPUThreshold)
		memSat := c.cfg.PSIMemThreshold > 0 && s.PSI["memory"] >= c.cfg.PSIMemThreshold
		if !cpuSat && !memSat {
			continue
		}
		for _, ar := range s.Runners {
			if !busy[ar.Name] {
				continue
			}
			others := total - ar.CPUNow
			if others < 0.25*s.CPUs && running < 2 {
				continue
			}
			var parts []string
			if cpuSat {
				parts = append(parts, fmt.Sprintf("host cpu %.0f%% busy, %.1f of %.0f cores by others", 100*min(1, total/s.CPUs), others, s.CPUs))
			}
			if memSat {
				parts = append(parts, fmt.Sprintf("memory pressure %.0f%%", s.PSI["memory"]))
			}
			if err := c.ledger.AddContention([]string{ar.Name}, dt.Seconds(), strings.Join(parts, "; ")); err != nil && !errors.Is(err, context.Canceled) {
				c.log.Error("ledger contention", "err", err)
			}
		}
	}
}
