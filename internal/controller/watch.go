package controller

import (
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
)

// Watching (watch: in the config): every minute the limits are checked and
// an incident is kept open for each one crossed; every quarter of an hour
// the regressions of the last days are looked for and each new one is
// announced once.

const (
	alarmEvery    = time.Minute
	announceEvery = 15 * time.Minute
	// alarmHistory: the finished jobs the limits are checked on (the growth
	// of a week needs two).
	alarmHistory = 15 * 24 * time.Hour
	// announceWithin: a regression whose first run is older than this is not
	// announced any more (the controller was away, or it is history).
	announceWithin = 7 * 24 * time.Hour
)

// regressionKey is the start of the keys of announced regressions; they are
// one-shot, never resolved by checkAlarms.
const regressionKey = "watch/regression/"

func (c *Controller) watchTick(now time.Time) {
	if now.Sub(c.alarmedAt) >= alarmEvery {
		c.alarmedAt = now
		c.checkAlarms(now)
	}
	if now.Sub(c.announcedAt) >= announceEvery {
		c.announcedAt = now
		c.announceRegressions(now)
	}
}

// liveJobs are the jobs waiting for a runner or running now.
func (c *Controller) liveJobs() []watch.Live {
	var out []watch.Live
	split := func(repo, ref string) (wf, branch string, pr int) {
		file, _, _ := strings.Cut(ref, "@")
		branch, pr = ledger.RefFacts(ref)
		return strings.TrimPrefix(file, repo+"/"), branch, pr
	}
	if ds, err := c.ledger.OpenDemand(); err == nil {
		for _, d := range ds {
			wf, branch, pr := split(d.Repo, d.Workflow)
			out = append(out, watch.Live{Repo: d.Repo, Workflow: wf, Job: d.Job, Class: d.Class, RunID: d.RunID, Event: d.Event,
				Branch: branch, PR: pr, Since: d.FirstSeen, Waiting: true})
		}
	}
	if rs, err := c.ledger.LiveRunners(); err == nil {
		for _, r := range rs {
			if r.State != "busy" || r.Repo == "" {
				continue
			}
			wf, branch, pr := split(r.Repo, r.Workflow)
			out = append(out, watch.Live{Repo: r.Repo, Workflow: wf, Job: r.Job, Class: r.Class, RunID: r.RunID, Event: r.Event,
				Branch: branch, PR: pr, Since: r.StartedAt})
		}
	}
	return out
}

// checkAlarms opens an incident for every limit crossed now and resolves the
// ones that are not any more (a limit taken out of the config too).
func (c *Controller) checkAlarms(now time.Time) {
	w := c.cfg.Watch
	open := map[string]bool{}
	if in, err := c.ledger.OpenIncidents(); err == nil {
		for _, i := range in {
			if strings.HasPrefix(i.Key, "watch/") && !strings.HasPrefix(i.Key, regressionKey) {
				open[i.Key] = true
			}
		}
	}
	if len(w.Limits) == 0 && w.CPUHoursPerDay == 0 && w.Slope == 0 && len(open) == 0 {
		return
	}
	runs, err := c.ledger.FinishedRunsBare(now.Add(-alarmHistory), time.Time{})
	if err != nil {
		c.log.Error("ledger runs", "err", err)
		return
	}
	alarms := watch.Alarms(watch.AlarmInput{Now: now, Loc: time.Local, Runs: runs, Live: c.liveJobs(), Open: open}, w)
	still := map[string]bool{}
	for _, a := range alarms {
		still[a.Key] = true
		c.book.open(a.Key, a.Kind, a.Subject, a.Detail)
	}
	for k := range open {
		if !still[k] {
			c.book.resolve(k)
		}
	}
}

// announceRegressions tells, once each, of the jobs that got slower or bigger
// by at least watch.regressions since a commit of the last days, and still are.
func (c *Controller) announceRegressions(now time.Time) {
	min := c.cfg.Watch.Announce()
	if min <= 0 {
		return
	}
	runs, err := c.ledger.FinishedRuns(now.Add(-announceWithin-watch.Baseline), time.Time{})
	if err != nil {
		c.log.Error("ledger runs", "err", err)
		return
	}
	for _, g := range watch.GroupChanges(watch.Find(runs, watch.Options{})) {
		ch := g[0]
		big := false
		for _, m := range g {
			big = big || m.Ratio >= min
		}
		if !big || !ch.Slower() || !ch.Holds || ch.Gradual || now.Sub(ch.First.At) > announceWithin {
			continue
		}
		key := regressionKey + ch.Repo + "/" + ch.Workflow + "/" + ch.Job + "/" + ch.First.Runner
		if seen, err := c.ledger.HasIncident(key); err != nil || seen {
			continue
		}
		detail := watch.ChangeLine(g)
		if more := watch.ChangeDetail(ch, c.webBase()); more != "" {
			detail += "; " + more
		}
		c.book.report("problem", key, "ci-regression", watch.JobName(ch.Workflow, ch.Job), detail, "")
	}
}

// webBase is where the repository's commits are shown: https://github.com.
func (c *Controller) webBase() string {
	u := c.cfg.URL
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "/"); j >= 0 {
			return u[:i+3+j]
		}
		return u
	}
	return "https://github.com"
}
