package watch

import (
	"cmp"
	"fmt"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

// An alarm is a limit of watch: that is crossed right now. The controller
// keeps an incident open for each alarm and resolves it when the alarm is
// no longer there, so the problems panel and notify see them like any other.

// Alarm is one crossed limit.
type Alarm struct {
	Key     string // "watch/…": the incident's key
	Kind    string
	Subject string
	Detail  string // the one line a person reads
}

// Live is a job that has not finished: waiting for a runner, or running.
type Live struct {
	Repo, Workflow, Job string // Workflow: the file in its repository
	Class               string
	RunID               int64
	Event, Branch       string
	PR                  int
	Since               time.Time // waiting or running since
	Waiting             bool
}

// AlarmInput is what the alarms are worked out from.
type AlarmInput struct {
	Now  time.Time
	Loc  *time.Location // midnight and finishBy are in it (default time.Local)
	Runs []ledger.JobRun
	Live []Live
	// Open: the alarms open now. A growth alarm stays open until the growth
	// is half its limit, so that it does not come and go at the line.
	Open map[string]bool
}

// LastRuns: a job's time, memory and wait are the median of this many of its
// newest runs that succeeded (at least 3 of them).
const LastRuns = 5

func ruleJob(class, repo, workflow, job, event, branch string, pr int) config.WaitingJob {
	ref := repo + "/" + workflow
	switch {
	case pr > 0:
		ref += "@refs/pull/" + strconv.Itoa(pr) + "/merge"
	case branch != "":
		ref += "@refs/heads/" + branch
	}
	return config.WaitingJob{Class: class, Repo: repo, WorkflowRef: ref, Name: job, Event: event}
}

func runMatches(m config.RuleMatch, r ledger.JobRun) bool {
	return m.Matches(ruleJob(r.Class, r.Repo, r.Workflow, r.Job, r.Event, r.Branch, r.PR))
}

// JobName is how a job is named in a line: "ci.yml / unit-tests".
func JobName(workflow, job string) string { return path.Base(workflow) + " / " + job }

func key(parts ...string) string { return "watch/" + strings.Join(parts, "/") }

// Alarms are the limits of w crossed at in.Now.
func Alarms(in AlarmInput, w config.Watch) []Alarm {
	if in.Loc == nil {
		in.Loc = time.Local
	}
	var out []Alarm
	for i, l := range w.Limits {
		var picked []ledger.JobRun
		for _, r := range in.Runs {
			if runMatches(l.Match, r) {
				picked = append(picked, r)
			}
		}
		if l.MaxJobTime > 0 || l.MaxMemory > 0 {
			out = append(out, jobLimits(picked, l)...)
		}
		if l.MaxRunTime > 0 {
			out = append(out, runLimits(picked, l)...)
		}
		if l.MaxWait > 0 {
			out = append(out, waitLimit(in, l, i)...)
		}
		if _, _, ok := l.Deadline(); ok {
			out = append(out, deadline(in, picked, l)...)
		}
	}
	if w.CPUHoursPerDay > 0 {
		out = append(out, cpuBudget(in, w.CPUHoursPerDay)...)
	}
	if w.Slope > 0 {
		out = append(out, growth(in, float64(w.Slope))...)
	}
	return out
}

// newest are the last LastRuns runs that succeeded on a host not saturated
// by others, nil when fewer than 3.
func newest(rs []ledger.JobRun) []ledger.JobRun {
	var ok []ledger.JobRun
	for _, r := range rs {
		if r.Result == "succeeded" && !r.Contended() {
			ok = append(ok, r)
		}
	}
	slices.SortStableFunc(ok, func(a, b ledger.JobRun) int { return a.FinishedAt.Compare(b.FinishedAt) })
	if len(ok) < 3 {
		return nil
	}
	return ok[max(0, len(ok)-LastRuns):]
}

func medianOf(rs []ledger.JobRun, f func(ledger.JobRun) float64) float64 {
	v := make([]float64, len(rs))
	for i, r := range rs {
		v[i] = f(r)
	}
	return median(v)
}

type jobKey struct{ repo, wf, job string }

func byJob(rs []ledger.JobRun) (map[jobKey][]ledger.JobRun, []jobKey) {
	m := map[jobKey][]ledger.JobRun{}
	var keys []jobKey
	for _, r := range rs {
		k := jobKey{r.Repo, r.Workflow, r.Job}
		if m[k] == nil {
			keys = append(keys, k)
		}
		m[k] = append(m[k], r)
	}
	slices.SortFunc(keys, func(a, b jobKey) int {
		return cmp.Or(cmp.Compare(a.repo, b.repo), cmp.Compare(a.wf, b.wf), cmp.Compare(a.job, b.job))
	})
	return m, keys
}

func jobLimits(rs []ledger.JobRun, l config.Limit) []Alarm {
	var out []Alarm
	m, keys := byJob(rs)
	for _, k := range keys {
		last := newest(m[k])
		if last == nil {
			continue
		}
		name := JobName(k.wf, k.job)
		if lim := l.MaxJobTime.D().Seconds(); lim > 0 {
			if t := medianOf(last, func(r ledger.JobRun) float64 { return r.RunSec }); t > lim {
				out = append(out, Alarm{Key: key("job-time", k.repo, k.wf, k.job), Kind: "ci-too-slow", Subject: name,
					Detail: fmt.Sprintf("%s takes %s (median of its last %d runs), over its %s limit", name, Secs(t), len(last), Secs(lim))})
			}
		}
		if lim := float64(l.MaxMemory); lim > 0 {
			if v := medianOf(last, Mem); v > lim {
				out = append(out, Alarm{Key: key("job-memory", k.repo, k.wf, k.job), Kind: "ci-too-big", Subject: name,
					Detail: fmt.Sprintf("%s peaks at %s of memory (median of its last %d runs), over its %s limit", name, GiB(v), len(last), GiB(lim))})
			}
		}
	}
	return out
}

func runLimits(rs []ledger.JobRun, l config.Limit) []Alarm {
	var out []Alarm
	type wfKey struct{ repo, wf string }
	m := map[wfKey][]ledger.JobRun{}
	var keys []wfKey
	for _, r := range WorkflowRuns(rs) {
		k := wfKey{r.Repo, r.Workflow}
		if m[k] == nil {
			keys = append(keys, k)
		}
		m[k] = append(m[k], r)
	}
	lim := l.MaxRunTime.D().Seconds()
	for _, k := range keys {
		last := newest(m[k])
		if last == nil {
			continue
		}
		if t := medianOf(last, func(r ledger.JobRun) float64 { return r.RunSec }); t > lim {
			name := path.Base(k.wf)
			out = append(out, Alarm{Key: key("run-time", k.repo, k.wf), Kind: "ci-too-slow", Subject: name,
				Detail: fmt.Sprintf("a run of %s takes %s, first job to last (median of the last %d), over its %s limit", name, Secs(t), len(last), Secs(lim))})
		}
	}
	return out
}

func waitLimit(in AlarmInput, l config.Limit, i int) []Alarm {
	lim := l.MaxWait.D()
	var over []Live
	for _, j := range in.Live {
		if j.Waiting && in.Now.Sub(j.Since) > lim && l.Match.Matches(ruleJob(j.Class, j.Repo, j.Workflow, j.Job, j.Event, j.Branch, j.PR)) {
			over = append(over, j)
		}
	}
	if len(over) == 0 {
		return nil
	}
	slices.SortFunc(over, func(a, b Live) int { return a.Since.Compare(b.Since) })
	o := over[0]
	detail := fmt.Sprintf("%s of run %d has waited %s for a runner, over the %s limit (%s)", JobName(o.Workflow, o.Job), o.RunID,
		Secs(in.Now.Sub(o.Since).Seconds()), Secs(lim.Seconds()), l.String())
	if len(over) > 1 {
		detail = fmt.Sprintf("%d jobs have waited over the %s limit (%s); the longest, %s of run %d, %s", len(over), Secs(lim.Seconds()), l.String(),
			JobName(o.Workflow, o.Job), o.RunID, Secs(in.Now.Sub(o.Since).Seconds()))
	}
	return []Alarm{{Key: key("wait", strconv.Itoa(i+1)), Kind: "ci-wait", Subject: l.String(), Detail: detail}}
}

// deadline: the workflow has to have finished a run, started within the day
// before the last deadline that passed, by that deadline. It is late while
// no such run has finished: none ran, or one still has jobs waiting or
// running. Only the jobs pumasi sees count: a later job of the run that
// GitHub has not queued yet is not known.
func deadline(in AlarmInput, rs []ledger.JobRun, l config.Limit) []Alarm {
	h, m, _ := l.Deadline()
	now := in.Now.In(in.Loc)
	due := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, in.Loc)
	if now.Before(due) {
		due = due.AddDate(0, 0, -1)
	}
	from := due.Add(-24 * time.Hour)
	type run struct {
		repo, wf     string
		start        time.Time
		live         []Live
		finishedJobs int
	}
	runs := map[int64]*run{}
	get := func(id int64, repo, wf string, start time.Time) *run {
		r := runs[id]
		if r == nil {
			r = &run{repo: repo, wf: wf, start: start}
			runs[id] = r
		}
		if start.Before(r.start) {
			r.start = start
		}
		return r
	}
	for _, r := range rs {
		get(r.RunID, r.Repo, r.Workflow, codeTime(r)).finishedJobs++
	}
	for _, j := range in.Live {
		if l.Match.Matches(ruleJob(j.Class, j.Repo, j.Workflow, j.Job, j.Event, j.Branch, j.PR)) {
			r := get(j.RunID, j.Repo, j.Workflow, j.Since)
			r.live = append(r.live, j)
		}
	}
	done, started := false, false
	var late *run
	for _, r := range runs {
		if r.start.Before(from) {
			continue
		}
		started = true
		if len(r.live) == 0 && r.finishedJobs > 0 {
			done = true
		} else if late == nil || r.start.Before(late.start) {
			late = r
		}
	}
	if done {
		return nil
	}
	name := l.String()
	detail := fmt.Sprintf("no run of %s finished by %s: none has started since %s", name, l.FinishBy, from.Format("Mon 15:04"))
	if started && late != nil {
		var still []string
		for _, j := range late.live {
			state := "running"
			if j.Waiting {
				state = "waiting"
			}
			still = append(still, fmt.Sprintf("%s %s %s", j.Job, state, Secs(in.Now.Sub(j.Since).Seconds())))
		}
		detail = fmt.Sprintf("the run of %s that started %s did not finish by %s (%s)", name, late.start.In(in.Loc).Format("Mon 15:04"), l.FinishBy, strings.Join(still, ", "))
	}
	return []Alarm{{Key: key("deadline", name), Kind: "ci-late", Subject: name, Detail: detail}}
}

func cpuBudget(in AlarmInput, budget float64) []Alarm {
	now := in.Now.In(in.Loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, in.Loc)
	per := map[string]float64{}
	total := 0.0
	for _, r := range in.Runs {
		if !r.FinishedAt.Before(midnight) && !r.FinishedAt.After(in.Now) {
			h := r.CPUSec / 3600
			total += h
			per[path.Base(r.Workflow)] += h
		}
	}
	if total <= budget {
		return nil
	}
	top, topH := "", 0.0
	for wf, h := range per {
		if h > topH || (h == topH && wf < top) {
			top, topH = wf, h
		}
	}
	return []Alarm{{Key: key("cpu-budget"), Kind: "ci-over-budget", Subject: "CI",
		Detail: fmt.Sprintf("CI used %.0f cpu-hours today, over the budget of %.0f a day (the most: %s, %.0f)", total, budget, top, topH)}}
}

// Weekly is how much a job's typical time grew in a week: the median of its
// last 7 days against the 7 before, each run set against what its host
// usually gives, among runs that succeeded on a host not saturated by others.
type Weekly struct {
	Repo, Workflow, Job string
	Before, After       float64 // median seconds
	Ratio               float64 // After/Before - 1
	Runs                [2]int  // runs in the week before and the last
}

// WeekGrowth is every job's growth over the week before now (jobs with at
// least 10 runs in each week).
func WeekGrowth(runs []ledger.JobRun, now time.Time) []Weekly {
	weekAgo, twoAgo := now.Add(-7*24*time.Hour), now.Add(-14*24*time.Hour)
	var recent []ledger.JobRun
	for _, r := range runs {
		if r.Result == "succeeded" && !r.Contended() && !r.FinishedAt.Before(twoAgo) && !r.FinishedAt.After(now) {
			recent = append(recent, r)
		}
	}
	m, keys := byJob(recent)
	var out []Weekly
	for _, k := range keys {
		rs := m[k]
		hf := hostFactors(rs, "time")
		var a, b []float64
		for _, r := range rs {
			v := r.RunSec
			if f := hf[machine(r)]; f > 0 {
				v /= f
			}
			if r.FinishedAt.Before(weekAgo) {
				a = append(a, v)
			} else {
				b = append(b, v)
			}
		}
		if len(a) < 10 || len(b) < 10 {
			continue
		}
		g := Weekly{Repo: k.repo, Workflow: k.wf, Job: k.job, Before: median(a), After: median(b), Runs: [2]int{len(a), len(b)}}
		if g.Before > 0 {
			g.Ratio = g.After/g.Before - 1
		}
		out = append(out, g)
	}
	return out
}

func growth(in AlarmInput, limit float64) []Alarm {
	var out []Alarm
	for _, g := range WeekGrowth(in.Runs, in.Now) {
		k := key("growth", g.Repo, g.Workflow, g.Job)
		bar := limit
		if in.Open[k] {
			bar = limit / 2
		}
		if g.Ratio < bar {
			continue
		}
		name := JobName(g.Workflow, g.Job)
		out = append(out, Alarm{Key: k, Kind: "ci-growing", Subject: name,
			Detail: fmt.Sprintf("%s grew %.0f%% in a week (%s -> %s, median), more than the %.0f%% a week watch.slope allows", name, 100*g.Ratio, Secs(g.Before), Secs(g.After), 100*limit)})
	}
	return out
}

// Secs is seconds as people read them: 45s, 4m02s, 1h10m.
func Secs(s float64) string {
	switch {
	case s <= 0:
		return "0s"
	case s < 60:
		return fmt.Sprintf("%.0fs", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
	}
	return fmt.Sprintf("%dh%02dm", int(s)/3600, int(s)%3600/60)
}

// GiB is bytes as 3.4G.
func GiB(b float64) string { return fmt.Sprintf("%.1fG", math.Max(0, b)/(1<<30)) }
