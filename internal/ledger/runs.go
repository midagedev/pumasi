package ledger

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// Run facts: what a finished job was a run of (its commit, branch, pull
// request, event), how long each of its steps took, and whether its host was
// so busy that its time says more about the host than about the job. They are
// what `pumasi trend` and `pumasi why-slow` read, and what regressions are
// found in.

// The columns a runner row gained for them (added like every later column).
var runFactsColumns = []string{
	// event: what triggered the run (push, pull_request, schedule, …).
	"runners ADD COLUMN event TEXT",
	// From the workflow's ref when the job started, then from GitHub's job:
	// the branch, the pull request (refs/pull/N/merge), the commit.
	"runners ADD COLUMN head_branch TEXT", "runners ADD COLUMN pr INTEGER", "runners ADD COLUMN head_sha TEXT",
	// gh_job_id: the number in the job's URL; run_attempt: which attempt of the run it was.
	"runners ADD COLUMN gh_job_id INTEGER", "runners ADD COLUMN run_attempt INTEGER",
	// facts: NULL = GitHub not asked yet, "asked" = asked once (never again),
	// "ok", or why GitHub gave nothing.
	"runners ADD COLUMN facts TEXT",
	// Seconds of the job's run during which its host was saturated by others
	// (contention.go in the controller), and the last reason seen.
	"runners ADD COLUMN contended_sec REAL", "runners ADD COLUMN contended_why TEXT",
}

const runsSchema = `
CREATE TABLE IF NOT EXISTS job_steps(
  runner TEXT NOT NULL,            -- runners.name
  number INTEGER NOT NULL,         -- the step's number in its job, from 1
  name TEXT NOT NULL,
  started_at TEXT, completed_at TEXT,
  conclusion TEXT,                 -- success | failure | skipped | cancelled
  PRIMARY KEY(runner, number)
);
`

// Created after the columns they index.
const runsIndexes = `
CREATE INDEX IF NOT EXISTS runners_finished ON runners(finished_at);
`

// ContendedShare: a run whose host was saturated by others for this share of
// its time or more is contended. Its numbers are kept and shown, but left out
// of baselines and of the search for regressions.
const ContendedShare = 0.2

// RefFacts reads a workflow ref ("o/r/.github/workflows/ci.yml@refs/heads/main",
// "…@refs/pull/42/merge"): the branch it ran on, or the pull request.
func RefFacts(workflowRef string) (branch string, pr int) {
	_, ref, ok := strings.Cut(workflowRef, "@")
	if !ok {
		return "", 0
	}
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b, 0
	}
	if rest, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		n, _, _ := strings.Cut(rest, "/")
		pr, _ = strconv.Atoi(n)
	}
	return "", pr
}

// FactsWanted is a finished job whose facts GitHub has not been asked for.
type FactsWanted struct {
	Runner, Repo string
	RunID        int64
}

// WantFacts are finished jobs whose facts have not been asked for, that
// finished between maxAge and delay ago (GitHub fills the last step in a
// moment after the job ends), the oldest first.
func (l *Ledger) WantFacts(now time.Time, delay, maxAge time.Duration, limit int) ([]FactsWanted, error) {
	rows, err := l.db.Query(`SELECT name, repo, run_id FROM runners
	  WHERE facts IS NULL AND finished_at IS NOT NULL AND finished_at <= ? AND finished_at >= ?
	    AND started_at IS NOT NULL AND COALESCE(run_id,0) > 0 AND COALESCE(repo,'') <> ''
	  ORDER BY finished_at LIMIT ?`, ts(now.Add(-delay)), ts(now.Add(-maxAge)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactsWanted
	for rows.Next() {
		var f FactsWanted
		if err := rows.Scan(&f.Runner, &f.Repo, &f.RunID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AskedFacts records that GitHub is being asked about a runner's job, before
// the call: whatever happens, it is not asked twice.
func (l *Ledger) AskedFacts(runner string) error {
	return l.exec(`UPDATE runners SET facts='asked' WHERE name=? AND facts IS NULL`, runner)
}

// Step is one step of a job as GitHub reports it.
type Step struct {
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	StartedAt   time.Time `json:"-"`
	CompletedAt time.Time `json:"-"`
	Sec         float64   `json:"sec"`
	Conclusion  string    `json:"conclusion,omitempty"`
}

// Facts is what GitHub said of a job.
type Facts struct {
	SHA, Branch string
	GHJobID     int64
	Attempt     int
	Steps       []Step
}

// SetFacts stores what GitHub said of a runner's job. A branch GitHub names
// replaces the one read from the ref (a pull request's ref names no branch).
func (l *Ledger) SetFacts(runner string, f Facts) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runners SET facts='ok', head_sha=NULLIF(?,''), head_branch=COALESCE(NULLIF(?,''), head_branch),
	  gh_job_id=NULLIF(?,0), run_attempt=NULLIF(?,0) WHERE name=?`, f.SHA, f.Branch, f.GHJobID, f.Attempt, runner); err != nil {
		return err
	}
	for _, s := range f.Steps {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO job_steps(runner, number, name, started_at, completed_at, conclusion) VALUES(?,?,?,?,?,NULLIF(?,''))`,
			runner, s.Number, s.Name, ts(s.StartedAt), ts(s.CompletedAt), s.Conclusion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NoFacts records why GitHub gave nothing for a runner's job.
func (l *Ledger) NoFacts(runner, why string) error {
	return l.exec(`UPDATE runners SET facts=? WHERE name=?`, why, runner)
}

// AddContention adds seconds during which these runners' host was saturated
// by others, with what was seen.
func (l *Ledger) AddContention(runners []string, sec float64, why string) error {
	if len(runners) == 0 {
		return nil
	}
	args := []any{sec, why}
	for _, r := range runners {
		args = append(args, r)
	}
	return l.exec(`UPDATE runners SET contended_sec=COALESCE(contended_sec,0)+?, contended_why=? WHERE name IN (?`+strings.Repeat(",?", len(runners)-1)+`)`, args...)
}

// PruneSteps deletes the step timings of jobs that finished before a moment.
// The jobs' own rows stay: they are one a job, the steps a dozen or more.
func (l *Ledger) PruneSteps(before time.Time) (int64, error) {
	res, err := l.db.Exec(`DELETE FROM job_steps WHERE runner IN (SELECT name FROM runners WHERE finished_at < ?)`, ts(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// JobRun is one finished job with everything known about it.
type JobRun struct {
	Runner   string `json:"runner"`
	Class    string `json:"class"`
	Host     string `json:"host"`
	Platform string `json:"platform"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"` // the workflow file in its repository, ".github/workflows/ci.yml"
	Job      string `json:"job"`
	RunID    int64  `json:"runId"`
	GHJobID  int64  `json:"ghJobId,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
	Event    string `json:"event,omitempty"`
	Branch   string `json:"branch,omitempty"`
	SHA      string `json:"sha,omitempty"`
	PR       int    `json:"pr,omitempty"`
	Result   string `json:"result"`
	// QueuedAt is when pumasi first saw the job wait.
	QueuedAt     time.Time `json:"queuedAt,omitzero"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	WaitSec      float64   `json:"waitSec"`
	RunSec       float64   `json:"runSec"`
	CPUSec       float64   `json:"cpuSec"`
	MemPeak      int64     `json:"memPeak"`
	MemWorkPeak  int64     `json:"memWorkPeak"`
	ContendedSec float64   `json:"contendedSec,omitempty"`
	ContendedWhy string    `json:"contendedWhy,omitempty"`
	Steps        []Step    `json:"steps,omitempty"`
}

// Contended says whether the run's host was saturated by others for much of it.
func (r JobRun) Contended() bool {
	return r.RunSec > 0 && r.ContendedSec >= ContendedShare*r.RunSec
}

// FinishedRuns are the jobs that finished in [from, to), the oldest first, with
// their steps. A zero to means up to now.
func (l *Ledger) FinishedRuns(from, to time.Time) ([]JobRun, error) {
	if to.IsZero() {
		to = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rows, err := l.db.Query(`SELECT r.name, r.class, r.host, COALESCE(r.platform,''), r.repo, `+workflowFileR+`, r.job, COALESCE(r.run_id,0),
	    COALESCE(r.gh_job_id,0), COALESCE(r.run_attempt,0), COALESCE(r.event, d.event, ''), COALESCE(r.head_branch,''), COALESCE(r.head_sha,''), COALESCE(r.pr,0),
	    r.result, d.first_seen, r.started_at, r.finished_at,
	    COALESCE(r.cpu_sec,0), COALESCE(r.mem_peak,0), COALESCE(r.mem_work_peak,0), COALESCE(r.contended_sec,0), COALESCE(r.contended_why,'')
	  FROM runners r LEFT JOIN demand d ON d.job_id = r.assigned_job_id
	  WHERE r.finished_at >= ? AND r.finished_at < ? AND r.started_at IS NOT NULL
	    AND COALESCE(r.result,'') <> '' AND COALESCE(r.repo,'') <> '' AND COALESCE(r.job,'') <> ''
	  ORDER BY r.finished_at, r.name`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	var out []JobRun
	idx := map[string]int{}
	for rows.Next() {
		var r JobRun
		var first, started, finished sql.NullString
		if err := rows.Scan(&r.Runner, &r.Class, &r.Host, &r.Platform, &r.Repo, &r.Workflow, &r.Job, &r.RunID,
			&r.GHJobID, &r.Attempt, &r.Event, &r.Branch, &r.SHA, &r.PR,
			&r.Result, &first, &started, &finished,
			&r.CPUSec, &r.MemPeak, &r.MemWorkPeak, &r.ContendedSec, &r.ContendedWhy); err != nil {
			rows.Close()
			return nil, err
		}
		r.Workflow = strings.TrimPrefix(r.Workflow, r.Repo+"/")
		r.QueuedAt, r.StartedAt, r.FinishedAt = parseTS(first), parseTS(started), parseTS(finished)
		r.RunSec = max(0, r.FinishedAt.Sub(r.StartedAt).Seconds())
		if !r.QueuedAt.IsZero() {
			r.WaitSec = max(0, r.StartedAt.Sub(r.QueuedAt).Seconds())
		}
		idx[r.Runner] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	steps, err := l.db.Query(`SELECT s.runner, s.number, s.name, s.started_at, s.completed_at, COALESCE(s.conclusion,'')
	  FROM job_steps s JOIN runners r ON r.name = s.runner
	  WHERE r.finished_at >= ? AND r.finished_at < ? ORDER BY s.runner, s.number`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer steps.Close()
	for steps.Next() {
		var runner string
		var s Step
		var a, b sql.NullString
		if err := steps.Scan(&runner, &s.Number, &s.Name, &a, &b, &s.Conclusion); err != nil {
			return nil, err
		}
		s.StartedAt, s.CompletedAt = parseTS(a), parseTS(b)
		if !s.StartedAt.IsZero() && !s.CompletedAt.IsZero() {
			s.Sec = max(0, s.CompletedAt.Sub(s.StartedAt).Seconds())
		}
		if i, ok := idx[runner]; ok {
			out[i].Steps = append(out[i].Steps, s)
		}
	}
	return out, steps.Err()
}

// workflowFile for a query whose runners table is r.
const workflowFileR = `CASE WHEN instr(COALESCE(r.workflow,''),'@')>0 THEN substr(r.workflow,1,instr(r.workflow,'@')-1) ELSE COALESCE(r.workflow,'') END`

// PutRun writes a finished job as a whole: for the demo and for tests, which
// make up a history instead of living through it.
func (l *Ledger) PutRun(r JobRun) error {
	ref := r.Repo + "/" + r.Workflow
	switch {
	case r.PR > 0:
		ref += "@refs/pull/" + strconv.Itoa(r.PR) + "/merge"
	case r.Branch != "":
		ref += "@refs/heads/" + r.Branch
	}
	jobID := "put-" + r.Runner
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !r.QueuedAt.IsZero() {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO demand(job_id, class, repo, workflow, job, run_id, event, first_seen, last_seen, served_by, closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			jobID, r.Class, r.Repo, ref, r.Job, r.RunID, r.Event, ts(r.QueuedAt), ts(r.StartedAt), r.Runner, ts(r.StartedAt)); err != nil {
			return err
		}
	}
	facts := any(nil)
	if r.SHA != "" || len(r.Steps) > 0 {
		facts = "ok"
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO runners(name, class, platform, host, state, created_at, listening_at, assigned_job_id, repo, workflow, job, run_id,
	    started_at, finished_at, removed_at, result, cpu_sec, mem_peak, mem_work_peak, unregistered,
	    event, head_branch, pr, head_sha, gh_job_id, run_attempt, facts, contended_sec, contended_why)
	  VALUES(?,?,NULLIF(?,''),?,'done',?,?,?,?,?,?,?, ?,?,?,?,?,?,?,1, NULLIF(?,''),NULLIF(?,''),NULLIF(?,0),NULLIF(?,''),NULLIF(?,0),NULLIF(?,0),?,NULLIF(?,0.0),NULLIF(?,''))`,
		r.Runner, r.Class, r.Platform, r.Host, ts(r.StartedAt.Add(-10*time.Second)), ts(r.StartedAt.Add(-time.Second)), jobID, r.Repo, ref, r.Job, r.RunID,
		ts(r.StartedAt), ts(r.FinishedAt), ts(r.FinishedAt), r.Result, r.CPUSec, r.MemPeak, r.MemWorkPeak,
		r.Event, r.Branch, r.PR, r.SHA, r.GHJobID, r.Attempt, facts, r.ContendedSec, r.ContendedWhy); err != nil {
		return err
	}
	for _, s := range r.Steps {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO job_steps(runner, number, name, started_at, completed_at, conclusion) VALUES(?,?,?,?,?,NULLIF(?,''))`,
			r.Runner, s.Number, s.Name, ts(s.StartedAt), ts(s.CompletedAt), s.Conclusion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FactsStatus is every runner whose facts were asked for, with the outcome
// ("ok", "asked", or why GitHub gave nothing), by runner.
func (l *Ledger) FactsStatus() ([][2]string, error) {
	rows, err := l.db.Query(`SELECT name, facts FROM runners WHERE facts IS NOT NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var r [2]string
		if err := rows.Scan(&r[0], &r[1]); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ContendedSeconds is the contended time recorded of every runner that has any.
func (l *Ledger) ContendedSeconds() (map[string]float64, error) {
	rows, err := l.db.Query(`SELECT name, contended_sec FROM runners WHERE contended_sec > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var n string
		var s float64
		if err := rows.Scan(&n, &s); err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, rows.Err()
}
