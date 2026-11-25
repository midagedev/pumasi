// Package ledger is the controller's SQLite record of runners, waiting jobs and
// placement decisions. It outlives controller restarts: GitHub has no runner
// list API and stops repeating a waiting job after three reassignments.
package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS runners(
  name TEXT PRIMARY KEY,
  class TEXT NOT NULL,
  host TEXT NOT NULL,
  state TEXT NOT NULL,             -- starting | idle | busy | done | failed | lost
  reason TEXT,
  runner_id INTEGER,               -- GitHub registration id from the JIT config
  created_at TEXT NOT NULL,        -- placement decision
  posted_at TEXT,                  -- start request sent to the agent
  listening_at TEXT,
  provision_ms INTEGER,            -- decision -> listening
  assigned_job_id TEXT, repo TEXT, workflow TEXT, job TEXT, run_id INTEGER,
  started_at TEXT, finished_at TEXT, result TEXT,
  cpu_sec REAL, mem_peak INTEGER, io_bytes INTEGER,
  psi_cpu_sec REAL, psi_mem_sec REAL, psi_io_sec REAL,
  exit_code INTEGER,
  removed_at TEXT,                 -- container removed on the host
  unregistered INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS demand(
  job_id TEXT PRIMARY KEY,
  class TEXT NOT NULL,
  repo TEXT, workflow TEXT, job TEXT, run_id INTEGER, labels TEXT,
  queue_time TEXT,
  first_seen TEXT NOT NULL,
  last_seen TEXT NOT NULL,
  assigned_count INTEGER NOT NULL DEFAULT 1,
  served_by TEXT,
  closed_at TEXT
);
CREATE TABLE IF NOT EXISTS decisions(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL, class TEXT, host TEXT, action TEXT NOT NULL, runner TEXT, reason TEXT
);
`

// TimeFormat is fixed width so text order is time order and SQLite's julianday() parses it.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(TimeFormat)
}

func parseTS(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(TimeFormat, s.String)
	return t
}

type Ledger struct {
	db *sql.DB
}

func Open(path string) (*Ledger, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Ledger{db: db}, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

func (l *Ledger) exec(q string, args ...any) error {
	_, err := l.db.Exec(q, args...)
	return err
}

// Runner is a ledger row (only the columns the controller reads back).
type Runner struct {
	Name, Class, Host, State string
	RunnerID                 int64
	CreatedAt, PostedAt      time.Time
	ListeningAt              time.Time
	JobID                    string
	RemovedAt                time.Time
}

func (l *Ledger) InsertRunner(name, class, host string, at time.Time) error {
	return l.exec(`INSERT INTO runners(name, class, host, state, created_at) VALUES(?,?,?,'starting',?)`, name, class, host, ts(at))
}

func (l *Ledger) SetRegistration(name string, runnerID int64, postedAt time.Time) error {
	return l.exec(`UPDATE runners SET runner_id=?, posted_at=? WHERE name=?`, runnerID, ts(postedAt), name)
}

// SetState sets a state with a reason. An empty state keeps the current one.
func (l *Ledger) SetState(name, state, reason string) error {
	return l.exec(`UPDATE runners SET state=COALESCE(NULLIF(?,''), state), reason=? WHERE name=?`, state, reason, name)
}

// MarkListening records the first time the runner listened; starting becomes idle.
func (l *Ledger) MarkListening(name string, at time.Time, provisionMs int64) error {
	if err := l.exec(`UPDATE runners SET listening_at=?, provision_ms=? WHERE name=? AND listening_at IS NULL`, ts(at), provisionMs, name); err != nil {
		return err
	}
	return l.exec(`UPDATE runners SET state='idle' WHERE name=? AND state='starting'`, name)
}

// MarkBusy records a job start seen in the runner's output, before GitHub's JobStarted message.
func (l *Ledger) MarkBusy(name string, at time.Time) error {
	return l.exec(`UPDATE runners SET state='busy', started_at=COALESCE(started_at, ?) WHERE name=? AND state IN ('starting','idle')`, ts(at), name)
}

// Usage is the last sampled cgroup values of a runner.
type Usage struct {
	CPUSec                         float64
	MemPeak, IOBytes               int64
	PSICPUSec, PSIMemSec, PSIIOSec float64
	ExitCode                       *int
}

func (l *Ledger) SetUsage(name string, u Usage) error {
	return l.exec(`UPDATE runners SET cpu_sec=MAX(COALESCE(cpu_sec,0),?), mem_peak=MAX(COALESCE(mem_peak,0),?),
	  io_bytes=MAX(COALESCE(io_bytes,0),?), psi_cpu_sec=?, psi_mem_sec=?, psi_io_sec=?, exit_code=COALESCE(?, exit_code) WHERE name=?`,
		u.CPUSec, u.MemPeak, u.IOBytes, u.PSICPUSec, u.PSIMemSec, u.PSIIOSec, u.ExitCode, name)
}

func (l *Ledger) MarkRemoved(name string, at time.Time) error {
	return l.exec(`UPDATE runners SET removed_at=? WHERE name=?`, ts(at), name)
}

func (l *Ledger) MarkUnregistered(name string) error {
	return l.exec(`UPDATE runners SET unregistered=1 WHERE name=?`, name)
}

// Job describes a job from a listener message.
type Job struct {
	ID, Class, Repo, Workflow, Name, Labels string
	RunID                                   int64
	QueueTime                               time.Time
}

// JobStarted binds a job to the runner that took it and closes its demand.
// It reports whether the runner is one of ours.
func (l *Ledger) JobStarted(runner string, j Job, at time.Time) (bool, error) {
	res, err := l.db.Exec(`UPDATE runners SET state='busy', assigned_job_id=?, repo=?, workflow=?, job=?, run_id=?, started_at=?
	  WHERE name=? AND state IN ('starting','idle','busy')`, j.ID, j.Repo, j.Workflow, j.Name, j.RunID, ts(at), runner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, l.CloseDemand(j.ID, runner, at)
}

// JobCompleted records a job's result on its runner.
func (l *Ledger) JobCompleted(runner string, j Job, result string, at time.Time) error {
	if err := l.exec(`UPDATE runners SET state=CASE WHEN state IN ('failed','lost') THEN state ELSE 'done' END,
	  result=?, finished_at=?, assigned_job_id=COALESCE(assigned_job_id, ?) WHERE name=?`, result, ts(at), j.ID, runner); err != nil {
		return err
	}
	return l.CloseDemand(j.ID, runner, at)
}

// UpsertDemand records a JobAssigned. When GitHub reassigns a job it comes
// with a new job id; the earlier, unassigned row of the same run and job name
// lends its first_seen and count so waiting time and order survive.
func (l *Ledger) UpsertDemand(j Job, at time.Time) error {
	return l.exec(`INSERT INTO demand(job_id, class, repo, workflow, job, run_id, labels, queue_time, first_seen, last_seen, assigned_count)
	  SELECT ?,?,?,?,?,?,?,?, COALESCE(MIN(first_seen), ?), ?, COUNT(*)+1
	  FROM demand WHERE run_id=? AND job=? AND class=? AND served_by LIKE 'unassigned-%'
	  ON CONFLICT(job_id) DO UPDATE SET last_seen=excluded.last_seen`,
		j.ID, j.Class, j.Repo, j.Workflow, j.Name, j.RunID, j.Labels, ts(j.QueueTime), ts(at), ts(at), j.RunID, j.Name, j.Class)
}

func (l *Ledger) CloseDemand(jobID, servedBy string, at time.Time) error {
	return l.exec(`UPDATE demand SET served_by=?, closed_at=? WHERE job_id=? AND closed_at IS NULL`, servedBy, ts(at), jobID)
}

type Demand struct {
	JobID, Class, Repo  string
	RunID               int64
	FirstSeen, LastSeen time.Time
}

func (l *Ledger) OpenDemand() ([]Demand, error) {
	rows, err := l.db.Query(`SELECT job_id, class, COALESCE(repo,''), COALESCE(run_id,0), first_seen, last_seen FROM demand WHERE closed_at IS NULL ORDER BY first_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Demand
	for rows.Next() {
		var d Demand
		var first, last sql.NullString
		if err := rows.Scan(&d.JobID, &d.Class, &d.Repo, &d.RunID, &first, &last); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = parseTS(first), parseTS(last)
		out = append(out, d)
	}
	return out, rows.Err()
}

const runnerCols = `name, class, host, state, COALESCE(runner_id,0), created_at, posted_at, listening_at, COALESCE(assigned_job_id,''), removed_at`

func (l *Ledger) queryRunners(where string, args ...any) ([]Runner, error) {
	rows, err := l.db.Query(`SELECT `+runnerCols+` FROM runners WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		var r Runner
		var created, posted, listening, removed sql.NullString
		if err := rows.Scan(&r.Name, &r.Class, &r.Host, &r.State, &r.RunnerID, &created, &posted, &listening, &r.JobID, &removed); err != nil {
			return nil, err
		}
		r.CreatedAt, r.PostedAt, r.ListeningAt, r.RemovedAt = parseTS(created), parseTS(posted), parseTS(listening), parseTS(removed)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveRunners are runners whose container has not been removed yet.
func (l *Ledger) LiveRunners() ([]Runner, error) {
	return l.queryRunners(`removed_at IS NULL AND state NOT IN ('failed','lost')`)
}

// PendingUnregister are finished runners whose GitHub registration may remain.
func (l *Ledger) PendingUnregister() ([]Runner, error) {
	return l.queryRunners(`unregistered=0 AND runner_id IS NOT NULL AND (removed_at IS NOT NULL OR state IN ('failed','lost'))`)
}

func (l *Ledger) Runner(name string) (Runner, error) {
	rs, err := l.queryRunners(`name=?`, name)
	if err != nil {
		return Runner{}, err
	}
	if len(rs) == 0 {
		return Runner{}, errors.New("no runner " + name)
	}
	return rs[0], nil
}

func (l *Ledger) AddDecision(at time.Time, class, host, action, runner, reason string) error {
	return l.exec(`INSERT INTO decisions(at, class, host, action, runner, reason) VALUES(?,?,?,?,?,?)`, ts(at), class, host, action, runner, reason)
}
