// Package watch turns the finished jobs of the ledger into what a person or
// a coding agent asks about CI: how long each job takes day by day, where it
// got slower and since which commit, and whether a limit was crossed.
// Everything here is a pure function of the runs given, so the controller,
// the CLI on a copy of the ledger and the demo answer the same way.
package watch

import (
	"cmp"
	"math"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// Stats is the middle and the slow end of some runs' values.
type Stats struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
}

// Bucket is a day or a week of a job's (or a workflow's) runs. The time,
// memory and cpu numbers are of the runs that succeeded: a failed run stops
// early and says nothing of how long the job takes. Wait is of every run.
type Bucket struct {
	Start     time.Time `json:"start"`
	Runs      int       `json:"runs"`      // every run that finished
	Succeeded int       `json:"succeeded"` // the runs the numbers below are of
	Contended int       `json:"contended"` // runs whose host was saturated by others
	Sec       Stats     `json:"sec"`       // run time, seconds
	MemBytes  Stats     `json:"memBytes"`  // memory at its peak, page cache left out
	CPUSec    Stats     `json:"cpuSec"`    // cpu-seconds
	WaitSec   Stats     `json:"waitSec"`   // from first seen waiting to started
	CPUHours  float64   `json:"cpuHours"`  // cpu-hours of every run, failed ones too
}

// Series is a job's (or a whole workflow's: Job empty) buckets over a span.
type Series struct {
	Repo     string   `json:"repo"`
	Workflow string   `json:"workflow"`
	Job      string   `json:"job,omitempty"`
	Buckets  []Bucket `json:"buckets"` // one for each day or week of the span, empty ones too
	All      Bucket   `json:"all"`     // the whole span
}

// Name is how a series is shown: "ci.yml / unit-tests", "ci.yml (workflow)".
func (s Series) Name() string {
	wf := path.Base(s.Workflow)
	if s.Job == "" {
		return wf + " (workflow)"
	}
	return wf + " / " + s.Job
}

// Filter picks runs: every field that is set must match. Repo, Workflow and
// Job are globs (path.Match) or plain substrings; Workflow matches the file's
// name or its path.
type Filter struct {
	Repo, Workflow, Job string
	Branch              string // exact
	PR                  int
}

func match(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	if ok, _ := path.Match(pattern, s); ok {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(pattern))
}

// Matches says whether a run is picked by the filter.
func (f Filter) Matches(r ledger.JobRun) bool {
	return match(f.Repo, r.Repo) && (match(f.Workflow, path.Base(r.Workflow)) || match(f.Workflow, r.Workflow)) && match(f.Job, r.Job) &&
		(f.Branch == "" || f.Branch == r.Branch) && (f.PR == 0 || f.PR == r.PR)
}

// TrendQuery is the span and the grain of a trend.
type TrendQuery struct {
	From, To time.Time // [From, To)
	Week     bool      // buckets of a week (from Monday) instead of a day
	Loc      *time.Location
	Filter   Filter
}

// Trend is every job's series and every workflow's, the workflows first,
// each followed by its jobs.
func Trend(runs []ledger.JobRun, q TrendQuery) []Series {
	if q.Loc == nil {
		q.Loc = time.Local
	}
	starts := bucketStarts(q)
	type key struct{ repo, wf, job string }
	jobRuns := map[key][]ledger.JobRun{}
	for _, r := range runs {
		if r.FinishedAt.Before(q.From) || !r.FinishedAt.Before(q.To) || !q.Filter.Matches(r) {
			continue
		}
		k := key{r.Repo, r.Workflow, r.Job}
		jobRuns[k] = append(jobRuns[k], r)
	}
	wfRuns := map[key][]ledger.JobRun{}
	for k, rs := range jobRuns {
		w := key{k.repo, k.wf, ""}
		wfRuns[w] = append(wfRuns[w], rs...)
	}
	var out []Series
	for w, rs := range wfRuns {
		out = append(out, series(w.repo, w.wf, "", WorkflowRuns(rs), starts, q))
		for k, jr := range jobRuns {
			if k.repo == w.repo && k.wf == w.wf {
				out = append(out, series(k.repo, k.wf, k.job, jr, starts, q))
			}
		}
	}
	slices.SortFunc(out, func(a, b Series) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Workflow, b.Workflow), cmp.Compare(a.Job, b.Job))
	})
	return out
}

// WorkflowRuns makes one run of each workflow run (and attempt) out of its
// jobs: from its first job's start to its last job's end, the cpu of all its
// jobs, the memory of the biggest, the longest wait. It succeeded when every
// job did. Only the jobs pumasi ran are seen.
func WorkflowRuns(jobs []ledger.JobRun) []ledger.JobRun {
	type key struct {
		repo, wf string
		run      int64
		attempt  int
	}
	idx := map[key]int{}
	var out []ledger.JobRun
	for _, j := range jobs {
		k := key{j.Repo, j.Workflow, j.RunID, j.Attempt}
		i, ok := idx[k]
		if !ok {
			w := j
			w.Job, w.Runner, w.Steps, w.GHJobID = "", "", nil, 0
			idx[k] = len(out)
			out = append(out, w)
			continue
		}
		w := &out[i]
		if j.StartedAt.Before(w.StartedAt) {
			w.StartedAt = j.StartedAt
		}
		if j.FinishedAt.After(w.FinishedAt) {
			w.FinishedAt = j.FinishedAt
		}
		if !j.QueuedAt.IsZero() && (w.QueuedAt.IsZero() || j.QueuedAt.Before(w.QueuedAt)) {
			w.QueuedAt = j.QueuedAt
		}
		w.CPUSec += j.CPUSec
		w.MemPeak, w.MemWorkPeak = max(w.MemPeak, j.MemPeak), max(w.MemWorkPeak, j.MemWorkPeak)
		w.WaitSec = max(w.WaitSec, j.WaitSec)
		w.ContendedSec = max(w.ContendedSec, j.ContendedSec)
		if j.Result != "succeeded" {
			w.Result = j.Result
		}
	}
	for i := range out {
		out[i].RunSec = out[i].FinishedAt.Sub(out[i].StartedAt).Seconds()
	}
	slices.SortFunc(out, func(a, b ledger.JobRun) int { return a.FinishedAt.Compare(b.FinishedAt) })
	return out
}

func series(repo, wf, job string, runs []ledger.JobRun, starts []time.Time, q TrendQuery) Series {
	s := Series{Repo: repo, Workflow: wf, Job: job}
	per := make([][]ledger.JobRun, len(starts))
	for _, r := range runs {
		i := sortSearch(starts, r.FinishedAt)
		if i >= 0 {
			per[i] = append(per[i], r)
		}
	}
	for i, st := range starts {
		b := bucket(per[i])
		b.Start = st
		s.Buckets = append(s.Buckets, b)
	}
	s.All = bucket(runs)
	if len(starts) > 0 {
		s.All.Start = starts[0]
	}
	return s
}

// sortSearch is the bucket a moment falls in (-1 = before the first).
func sortSearch(starts []time.Time, t time.Time) int {
	i, _ := slices.BinarySearchFunc(starts, t, func(s, t time.Time) int { return s.Compare(t) })
	if i < len(starts) && starts[i].Equal(t) {
		return i
	}
	return i - 1
}

// Mem is a run's memory figure: its peak without page cache, or with it on
// a row from before that was measured.
func Mem(r ledger.JobRun) float64 {
	if r.MemWorkPeak > 0 {
		return float64(r.MemWorkPeak)
	}
	return float64(r.MemPeak)
}

func bucket(runs []ledger.JobRun) Bucket {
	b := Bucket{Runs: len(runs)}
	var sec, mem, cpu, wait []float64
	for _, r := range runs {
		b.CPUHours += r.CPUSec / 3600
		wait = append(wait, r.WaitSec)
		if r.Contended() {
			b.Contended++
		}
		if r.Result != "succeeded" {
			continue
		}
		b.Succeeded++
		sec = append(sec, r.RunSec)
		mem = append(mem, Mem(r))
		cpu = append(cpu, r.CPUSec)
	}
	b.CPUHours = math.Round(b.CPUHours*1000) / 1000
	b.Sec, b.MemBytes, b.CPUSec, b.WaitSec = stats(sec), stats(mem), stats(cpu), stats(wait)
	return b
}

func stats(v []float64) Stats {
	if len(v) == 0 {
		return Stats{}
	}
	slices.Sort(v)
	return Stats{P50: Percentile(v, 0.5), P90: Percentile(v, 0.9)}
}

// Percentile is the nearest-rank percentile of sorted values: the smallest
// value that at least p of them are at or below.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func bucketStarts(q TrendQuery) []time.Time {
	day := func(t time.Time) time.Time {
		t = t.In(q.Loc)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, q.Loc)
	}
	first := day(q.From)
	if q.Week {
		first = first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7))
	}
	var out []time.Time
	for t := first; t.Before(q.To); {
		out = append(out, t)
		if q.Week {
			t = t.AddDate(0, 0, 7)
		} else {
			t = t.AddDate(0, 0, 1)
		}
	}
	return out
}
