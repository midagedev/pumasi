// Package ledger is the controller's SQLite record of runners, waiting jobs and
// placement decisions. It outlives controller restarts: GitHub has no runner
// list API and stops repeating a waiting job after three reassignments.
package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/size"
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
CREATE TABLE IF NOT EXISTS incidents(
  key TEXT NOT NULL,               -- same key = same incident, e.g. host-unreachable/server1
  kind TEXT NOT NULL,
  subject TEXT,
  opened_at TEXT NOT NULL,
  resolved_at TEXT,
  detail TEXT,                     -- the one line a person reads
  action TEXT                      -- what pumasi did about it
);
CREATE INDEX IF NOT EXISTS incidents_open ON incidents(key) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS demand_first_seen ON demand(first_seen);
CREATE INDEX IF NOT EXISTS runners_assigned_job ON runners(assigned_job_id);
CREATE TABLE IF NOT EXISTS outbox(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL, payload TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, error TEXT, sent_at TEXT
);
CREATE TABLE IF NOT EXISTS reruns(
  run_id INTEGER PRIMARY KEY,      -- workflow run with a job lost to an infrastructure failure
  repo TEXT NOT NULL,
  reason TEXT,
  requested_at TEXT NOT NULL,
  pending INTEGER NOT NULL DEFAULT 1,
  count INTEGER NOT NULL DEFAULT 0 -- rerun-failed-jobs calls made for this run
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
	if _, err := db.Exec(schema + opsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Columns added after the first release; "duplicate column" means it is there.
	for _, col := range []string{"runners ADD COLUMN log_tail TEXT", "runners ADD COLUMN oom_kills INTEGER",
		// mem_work_peak: the most memory held that was not page cache (mem_peak counts the cache too).
		// est_*: what placement counted on for this runner; est_runs = finished runs it was learned from.
		"runners ADD COLUMN mem_work_peak INTEGER", "runners ADD COLUMN est_cpus REAL", "runners ADD COLUMN est_mem INTEGER", "runners ADD COLUMN est_runs INTEGER", "runners ADD COLUMN est_blind INTEGER",
		// limit_mem: the memory limit the OS held the runner to (NULL or 0 = none).
		"runners ADD COLUMN limit_mem INTEGER",
		// platform: the OS and driver the runner ran as ("linux/docker"); part of its job's key.
		"runners ADD COLUMN platform TEXT",
		// event: what triggered the job's run (push, schedule, …), for the priority rules.
		"demand ADD COLUMN event TEXT"} {
		if _, err := db.Exec("ALTER TABLE " + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	return &Ledger{db: db}, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

// Check runs SQLite's integrity check; an error means the file cannot be trusted.
func (l *Ledger) Check() error {
	var res string
	if err := l.db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New("integrity check: " + res)
	}
	return nil
}

// OpenOrRebuild opens the ledger. A file that does not open or fails its
// integrity check is moved aside (with its -wal and -shm) and an empty ledger
// takes its place; movedTo names the copy that was set aside.
func OpenOrRebuild(path string, now time.Time) (l *Ledger, movedTo string, err error) {
	l, err = Open(path)
	if err == nil {
		if err = l.Check(); err == nil {
			return l, "", nil
		}
		l.Close()
	}
	broken := err
	movedTo = path + ".broken-" + now.Format("20060102-150405")
	if err := os.Rename(path, movedTo); err != nil {
		return nil, "", fmt.Errorf("ledger %s is damaged (%v) and cannot be moved aside: %w", path, broken, err)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		_ = os.Rename(path+ext, movedTo+ext)
	}
	l, err = Open(path)
	return l, movedTo, err
}

// InsertAdopted records a runner found on a host that the ledger does not know
// (the ledger was rebuilt). Its job, if any, is known only by the runner.
func (l *Ledger) InsertAdopted(name, class, host string, runnerID int64, created, listening, jobStarted time.Time) error {
	state := "starting"
	if !listening.IsZero() {
		state = "idle"
	}
	if !jobStarted.IsZero() {
		state = "busy"
	}
	var id any
	if runnerID != 0 {
		id = runnerID
	}
	return l.exec(`INSERT OR IGNORE INTO runners(name, class, host, state, reason, runner_id, created_at, posted_at, listening_at, started_at)
	  VALUES(?,?,?,?,'adopted from the host',?,?,?,?,?)`, name, class, host, state, id, ts(created), ts(created), ts(listening), ts(jobStarted))
}

// Rerun is a workflow run waiting to have its failed jobs run again.
type Rerun struct {
	RunID       int64
	Repo        string
	Reason      string
	RequestedAt time.Time
}

// RequestRerun queues a run unless it has been rerun max times already. It
// reports whether the run is (now or already) queued.
func (l *Ledger) RequestRerun(repo string, runID int64, reason string, at time.Time, max int) (bool, error) {
	if err := l.exec(`INSERT INTO reruns(run_id, repo, reason, requested_at) VALUES(?,?,?,?)
	  ON CONFLICT(run_id) DO UPDATE SET pending=1, reason=excluded.reason, requested_at=excluded.requested_at
	  WHERE reruns.pending=0 AND reruns.count < ?`, runID, repo, reason, ts(at), max); err != nil {
		return false, err
	}
	var pending int
	err := l.db.QueryRow(`SELECT pending FROM reruns WHERE run_id=?`, runID).Scan(&pending)
	return pending == 1, err
}

func (l *Ledger) PendingReruns() ([]Rerun, error) {
	rows, err := l.db.Query(`SELECT run_id, repo, COALESCE(reason,''), requested_at FROM reruns WHERE pending=1 ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rerun
	for rows.Next() {
		var r Rerun
		var at sql.NullString
		if err := rows.Scan(&r.RunID, &r.Repo, &r.Reason, &at); err != nil {
			return nil, err
		}
		r.RequestedAt = parseTS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RerunSettled takes a run off the queue; called says whether GitHub was asked to rerun it.
func (l *Ledger) RerunSettled(runID int64, called bool) error {
	n := 0
	if called {
		n = 1
	}
	return l.exec(`UPDATE reruns SET pending=0, count=count+? WHERE run_id=?`, n, runID)
}

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
	JobID, Job               string // Job is the job's display name, known from JobStarted on
	Repo, Result             string
	RunID                    int64
	Workflow                 string  // the job's workflow file with its ref, as GitHub names it
	OOMKills                 int     // processes killed at the runner's memory limit
	MemPeak                  int64   // the most memory the runner has been seen using, page cache included
	MemWorkPeak              int64   // the same without page cache: what the job needs to be given
	EstCPUs                  float64 // what placement counts on for it (0 = a row from before estimates)
	EstMem                   int64
	EstRuns                  int
	EstBlind                 bool      // placed on the class's first estimate with nothing measured of the job or its workflow
	StartedAt                time.Time // JobStarted, or the runner's own "Running job" line
	RemovedAt                time.Time
}

// InsertRunner records a runner as placed. platform is what it runs as (see
// JobKey).
func (l *Ledger) InsertRunner(name, class, platform, host string, at time.Time, est size.Estimate) error {
	return l.exec(`INSERT INTO runners(name, class, platform, host, state, created_at, est_cpus, est_mem, est_runs, est_blind) VALUES(?,?,NULLIF(?,''),?,'starting',?,?,?,?,?)`,
		name, class, platform, host, ts(at), est.CPUs, est.MemBytes, est.Runs, est.Blind)
}

// SetPlatform fills in what a class's runners ran as on the rows from before
// that was recorded. A class's OS and driver are taken to be what they are
// now; the rows of a class that is gone stay without one and size nothing.
func (l *Ledger) SetPlatform(class, platform string) error {
	return l.exec(`UPDATE runners SET platform=? WHERE class=? AND platform IS NULL`, platform, class)
}

// SetEstimate replaces what placement counts on for a runner: GitHub has
// named its job, and it is not always the job the runner was started for.
func (l *Ledger) SetEstimate(name string, est size.Estimate) error {
	return l.exec(`UPDATE runners SET est_cpus=?, est_mem=?, est_runs=?, est_blind=? WHERE name=?`, est.CPUs, est.MemBytes, est.Runs, est.Blind, name)
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

// Usage is the last sampled values of a runner. Nil = the host's OS does not
// measure it; the column stays NULL.
type Usage struct {
	CPUSec                         float64
	MemPeak, MemWorkPeak           int64
	LimitMem                       int64 // the memory limit the runner ran under (0 = none)
	IOBytes                        *int64
	PSICPUSec, PSIMemSec, PSIIOSec *float64
	ExitCode                       *int
	OOMKills                       int
}

func (l *Ledger) SetUsage(name string, u Usage) error {
	return l.exec(`UPDATE runners SET cpu_sec=MAX(COALESCE(cpu_sec,0),?), mem_peak=MAX(COALESCE(mem_peak,0),?), mem_work_peak=MAX(COALESCE(mem_work_peak,0),?), limit_mem=MAX(COALESCE(limit_mem,0),?),
	  io_bytes=MAX(COALESCE(io_bytes,0),?), psi_cpu_sec=COALESCE(?, psi_cpu_sec), psi_mem_sec=COALESCE(?, psi_mem_sec), psi_io_sec=COALESCE(?, psi_io_sec), exit_code=COALESCE(?, exit_code),
	  oom_kills=MAX(COALESCE(oom_kills,0),?) WHERE name=?`,
		u.CPUSec, u.MemPeak, u.MemWorkPeak, u.LimitMem, u.IOBytes, u.PSICPUSec, u.PSIMemSec, u.PSIIOSec, u.ExitCode, u.OOMKills, name)
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
	Event                                   string // push, pull_request, schedule, workflow_dispatch, …
	RunID                                   int64
	QueueTime                               time.Time
}

// JobStarted binds a job to the runner that took it and closes its demand.
// It reports whether the runner is one of ours.
func (l *Ledger) JobStarted(runner string, j Job, at time.Time) (bool, error) {
	// The message can come after the runner is over (the controller was away while
	// the job ran): the row still learns which job it was, its state stays.
	res, err := l.db.Exec(`UPDATE runners SET state=CASE WHEN state IN ('starting','idle','busy') THEN 'busy' ELSE state END,
	  assigned_job_id=?, repo=?, workflow=?, job=?, run_id=?, started_at=CASE WHEN state IN ('starting','idle','busy') THEN ? ELSE COALESCE(started_at, ?) END
	  WHERE name=?`, j.ID, j.Repo, j.Workflow, j.Name, j.RunID, ts(at), ts(at), runner)
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
// lends its first_seen and count so waiting time and order survive. Only a
// row GitHub took back within the last half hour: the same run and job name
// hours later is somebody running the job again, and that wait starts anew.
func (l *Ledger) UpsertDemand(j Job, at time.Time) error {
	return l.exec(`INSERT INTO demand(job_id, class, repo, workflow, job, run_id, labels, event, queue_time, first_seen, last_seen, assigned_count)
	  SELECT ?,?,?,?,?,?,?,?,?, COALESCE(MIN(first_seen), ?), ?, COALESCE(MAX(assigned_count), 0)+1
	  FROM demand WHERE run_id=? AND job=? AND class=? AND served_by LIKE 'unassigned-%' AND closed_at >= ?
	  ON CONFLICT(job_id) DO UPDATE SET last_seen=excluded.last_seen`,
		j.ID, j.Class, j.Repo, j.Workflow, j.Name, j.RunID, j.Labels, j.Event, ts(j.QueueTime), ts(at), ts(at), j.RunID, j.Name, j.Class, ts(at.Add(-30*time.Minute)))
}

func (l *Ledger) CloseDemand(jobID, servedBy string, at time.Time) error {
	return l.exec(`UPDATE demand SET served_by=?, closed_at=? WHERE job_id=? AND closed_at IS NULL`, servedBy, ts(at), jobID)
}

// CloseOpenDemand closes every waiting job, for when nothing will serve them anymore.
func (l *Ledger) CloseOpenDemand(servedBy string, at time.Time) error {
	return l.exec(`UPDATE demand SET served_by=?, closed_at=? WHERE closed_at IS NULL`, servedBy, ts(at))
}

type Demand struct {
	JobID, Class, Repo  string
	Workflow, Job       string
	Event               string
	RunID               int64
	FirstSeen, LastSeen time.Time
	ClosedAt            time.Time // zero while it is open
	ServedBy            string    // what closed it: a runner, or "unassigned-…", "run-…", "ttl", "down"
}

func (l *Ledger) OpenDemand() ([]Demand, error) {
	return l.demand(`closed_at IS NULL ORDER BY first_seen`)
}

func (l *Ledger) demand(where string, args ...any) ([]Demand, error) {
	rows, err := l.db.Query(`SELECT job_id, class, COALESCE(repo,''), COALESCE(workflow,''), COALESCE(job,''), COALESCE(event,''), COALESCE(run_id,0), first_seen, last_seen, closed_at, COALESCE(served_by,'') FROM demand WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Demand
	for rows.Next() {
		var d Demand
		var first, last, closed sql.NullString
		if err := rows.Scan(&d.JobID, &d.Class, &d.Repo, &d.Workflow, &d.Job, &d.Event, &d.RunID, &first, &last, &closed, &d.ServedBy); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen, d.ClosedAt = parseTS(first), parseTS(last), parseTS(closed)
		out = append(out, d)
	}
	return out, rows.Err()
}

const runnerCols = `name, class, host, state, COALESCE(runner_id,0), created_at, posted_at, listening_at, COALESCE(assigned_job_id,''), COALESCE(job,''), removed_at,
  COALESCE(repo,''), COALESCE(result,''), COALESCE(run_id,0), started_at, COALESCE(oom_kills,0), COALESCE(mem_peak,0),
  COALESCE(workflow,''), COALESCE(mem_work_peak,0), COALESCE(est_cpus,0), COALESCE(est_mem,0), COALESCE(est_runs,0), COALESCE(est_blind,0)`

func (l *Ledger) queryRunners(where string, args ...any) ([]Runner, error) {
	rows, err := l.db.Query(`SELECT `+runnerCols+` FROM runners WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		var r Runner
		var created, posted, listening, removed, started sql.NullString
		if err := rows.Scan(&r.Name, &r.Class, &r.Host, &r.State, &r.RunnerID, &created, &posted, &listening, &r.JobID, &r.Job, &removed,
			&r.Repo, &r.Result, &r.RunID, &started, &r.OOMKills, &r.MemPeak,
			&r.Workflow, &r.MemWorkPeak, &r.EstCPUs, &r.EstMem, &r.EstRuns, &r.EstBlind); err != nil {
			return nil, err
		}
		r.CreatedAt, r.PostedAt, r.ListeningAt, r.RemovedAt = parseTS(created), parseTS(posted), parseTS(listening), parseTS(removed)
		r.StartedAt = parseTS(started)
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

// LastDefer is the reason of the newest defer decision for a class ("" = none).
func (l *Ledger) LastDefer(class string) string {
	var reason sql.NullString
	_ = l.db.QueryRow(`SELECT reason FROM decisions WHERE action='defer' AND class=? ORDER BY id DESC LIMIT 1`, class).Scan(&reason)
	return reason.String
}

// Revive brings back a runner that was written off (host unreachable) and
// turned out to be running a job. It reports whether a row was revived.
func (l *Ledger) Revive(name string) (bool, error) {
	res, err := l.db.Exec(`UPDATE runners SET state='busy', reason='found running a job', removed_at=NULL
	  WHERE name=? AND state IN ('lost','failed')`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// HoldsJob records GitHub's word that the runner has a job: it refused to drop
// the registration. The row is live and busy again, whatever closed it.
func (l *Ledger) HoldsJob(name string, at time.Time) error {
	return l.exec(`UPDATE runners SET state='busy', reason='GitHub has a job on it', removed_at=NULL, unregistered=0,
	  started_at=COALESCE(started_at, ?) WHERE name=? AND state<>'done'`, ts(at), name)
}

// CloseFromExit ends a busy runner whose JobCompleted message never came (the
// controller was away): the result is the container's exit code as the agent
// saw it. A later JobCompleted still overwrites it with GitHub's result.
func (l *Ledger) CloseFromExit(name string, exitCode int, at time.Time) error {
	return l.exec(`UPDATE runners SET state='done', result=?, finished_at=COALESCE(finished_at, ?)
	  WHERE name=? AND state='busy' AND result IS NULL`, fmt.Sprintf("exit-%d", exitCode), ts(at), name)
}

// Incident is something that went wrong, whether pumasi fixed it or not.
type Incident struct {
	Key        string    `json:"key"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject"`
	OpenedAt   time.Time `json:"openedAt"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
	Detail     string    `json:"detail"`
	Action     string    `json:"action,omitempty"`
}

// OpenIncident opens the incident unless one with this key is open already
// (then only its detail is refreshed). It reports whether it was newly opened.
// With oneShot the incident is recorded as already over: a fact, not a state.
func (l *Ledger) OpenIncident(in Incident, oneShot bool) (bool, error) {
	res, err := l.db.Exec(`UPDATE incidents SET detail=?, action=COALESCE(NULLIF(?,''), action) WHERE key=? AND resolved_at IS NULL`, in.Detail, in.Action, in.Key)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	var resolved any
	if oneShot {
		resolved = ts(in.OpenedAt)
	}
	return true, l.exec(`INSERT INTO incidents(key, kind, subject, opened_at, resolved_at, detail, action) VALUES(?,?,?,?,?,?,?)`,
		in.Key, in.Kind, in.Subject, ts(in.OpenedAt), resolved, in.Detail, in.Action)
}

// ResolveIncident closes the open incident with this key, if any, and returns it.
func (l *Ledger) ResolveIncident(key string, at time.Time) (Incident, bool, error) {
	open, err := l.incidents(`key=? AND resolved_at IS NULL`, key)
	if err != nil || len(open) == 0 {
		return Incident{}, false, err
	}
	in := open[0]
	in.ResolvedAt = at
	return in, true, l.exec(`UPDATE incidents SET resolved_at=? WHERE key=? AND resolved_at IS NULL`, ts(at), key)
}

func (l *Ledger) OpenIncidents() ([]Incident, error) { return l.incidents(`resolved_at IS NULL`) }

// ClosedIncidents are the incidents resolved since a moment, the oldest first.
func (l *Ledger) ClosedIncidents(since time.Time) ([]Incident, error) {
	return l.incidents(`resolved_at IS NOT NULL AND resolved_at >= ?`, ts(since))
}

// Arrivals is when each class's jobs first came since a moment, by class.
// A job given back and offered again is one arrival.
func (l *Ledger) Arrivals(since time.Time) (map[string][]time.Time, error) {
	rows, err := l.db.Query(`SELECT class, MIN(first_seen) FROM demand WHERE first_seen >= ?
	  GROUP BY class, CASE WHEN run_id IS NULL OR job IS NULL THEN job_id ELSE run_id || '/' || job END`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]time.Time{}
	for rows.Next() {
		var class string
		var first sql.NullString
		if err := rows.Scan(&class, &first); err != nil {
			return nil, err
		}
		out[class] = append(out[class], parseTS(first))
	}
	return out, rows.Err()
}

// RunJob is a job of a workflow run: when it first asked for a runner, and
// when the pumasi runner that had it started and finished it (zero: not
// yet, or not on a pumasi runner).
type RunJob struct {
	Repo     string
	RunID    int64
	Workflow string // the workflow file, without its ref
	Job      string
	Class    string
	Came     time.Time
	Started  time.Time
	Ended    time.Time
}

// RunJobs are the jobs of workflow runs that came since a moment, in the
// order they came, a job given back and offered again once.
func (l *Ledger) RunJobs(since time.Time) ([]RunJob, error) {
	rows, err := l.db.Query(`SELECT COALESCE(d.repo,''), d.run_id,
	    CASE WHEN instr(COALESCE(d.workflow,''),'@')>0 THEN substr(d.workflow,1,instr(d.workflow,'@')-1) ELSE COALESCE(d.workflow,'') END,
	    d.job, MIN(d.class), MIN(d.first_seen), MAX(r.started_at), MAX(r.finished_at)
	  FROM demand d LEFT JOIN runners r ON r.assigned_job_id = d.job_id
	  WHERE d.first_seen >= ? AND d.run_id IS NOT NULL AND d.run_id != 0 AND COALESCE(d.job,'') != ''
	  GROUP BY d.repo, d.run_id, d.job ORDER BY MIN(d.first_seen)`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunJob
	for rows.Next() {
		var j RunJob
		var came, started, ended sql.NullString
		if err := rows.Scan(&j.Repo, &j.RunID, &j.Workflow, &j.Job, &j.Class, &came, &started, &ended); err != nil {
			return nil, err
		}
		j.Came, j.Started, j.Ended = parseTS(came), parseTS(started), parseTS(ended)
		out = append(out, j)
	}
	return out, rows.Err()
}

// TakenBack are the jobs GitHub took back from their class before a runner
// had them, since a moment: jobs the class gave back for want of room, and
// jobs that were cancelled while they waited. Oldest first.
func (l *Ledger) TakenBack(since time.Time) ([]Demand, error) {
	return l.demand(`closed_at >= ? AND served_by LIKE 'unassigned-%' ORDER BY closed_at`, ts(since))
}

func (l *Ledger) incidents(where string, args ...any) ([]Incident, error) {
	rows, err := l.db.Query(`SELECT key, kind, COALESCE(subject,''), opened_at, resolved_at, COALESCE(detail,''), COALESCE(action,'') FROM incidents WHERE `+where+` ORDER BY opened_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		var in Incident
		var opened, resolved sql.NullString
		if err := rows.Scan(&in.Key, &in.Kind, &in.Subject, &opened, &resolved, &in.Detail, &in.Action); err != nil {
			return nil, err
		}
		in.OpenedAt, in.ResolvedAt = parseTS(opened), parseTS(resolved)
		out = append(out, in)
	}
	return out, rows.Err()
}

// Outbox holds notifications until they are delivered.
type Outgoing struct {
	ID      int64
	Payload string
}

func (l *Ledger) Enqueue(payload string, at time.Time) error {
	return l.exec(`INSERT INTO outbox(at, payload) VALUES(?,?)`, ts(at), payload)
}

func (l *Ledger) Unsent() ([]Outgoing, error) {
	rows, err := l.db.Query(`SELECT id, payload FROM outbox WHERE sent_at IS NULL ORDER BY id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outgoing
	for rows.Next() {
		var o Outgoing
		if err := rows.Scan(&o.ID, &o.Payload); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Sent records the outcome of a delivery attempt; a failed one stays unsent.
func (l *Ledger) Sent(id int64, at time.Time, sendErr error) error {
	if sendErr != nil {
		return l.exec(`UPDATE outbox SET attempts=attempts+1, error=? WHERE id=?`, sendErr.Error(), id)
	}
	return l.exec(`UPDATE outbox SET attempts=attempts+1, error=NULL, sent_at=? WHERE id=?`, ts(at), id)
}

// FinishedJob is a runner whose container is gone, with what its job cost.
type FinishedJob struct {
	Runner      string    `json:"runner"`
	Class       string    `json:"class"`
	Host        string    `json:"host"`
	Job         string    `json:"job"`
	Repo        string    `json:"repo"`
	RunID       int64     `json:"runId"`
	State       string    `json:"state"`
	Result      string    `json:"result"` // GitHub's result; empty when the runner never got a job
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"createdAt"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
	WaitSec     float64   `json:"waitSec"` // job first seen -> job started
	RunSec      float64   `json:"runSec"`
	ProvisionMs int64     `json:"provisionMs"`
	CPUSec      float64   `json:"cpuSec"`
	MemPeak     int64     `json:"memPeak"`
	// MemWorkPeak leaves the page cache out; Est* is what placement counted on.
	MemWorkPeak int64   `json:"memWorkPeak"`
	EstCPUs     float64 `json:"estCpus"`
	EstMem      int64   `json:"estMem"`
	ExitCode    *int    `json:"exitCode"`
	OOMKills    int     `json:"oomKills"`
}

// JobTotal is how many jobs the runners of a class finished with one result
// (GitHub's, or "exit-N" when only the runner's exit was seen), and how long
// those jobs ran in all.
type JobTotal struct {
	Class, Result string
	Jobs          int
	Seconds       float64
}

// JobTotals are the finished jobs of the whole ledger by class and result:
// counters that only grow while the ledger is kept.
func (l *Ledger) JobTotals() ([]JobTotal, error) {
	rows, err := l.db.Query(`SELECT class, result, COUNT(*),
	    ROUND(COALESCE(SUM(MAX(0, (julianday(finished_at)-julianday(started_at))*86400)), 0), 1)
	  FROM runners WHERE result IS NOT NULL AND result != '' AND started_at IS NOT NULL AND finished_at IS NOT NULL
	  GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobTotal
	for rows.Next() {
		var t JobTotal
		if err := rows.Scan(&t.Class, &t.Result, &t.Jobs, &t.Seconds); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RecentJobs are the newest runners that are over, newest first.
func (l *Ledger) RecentJobs(limit int) ([]FinishedJob, error) {
	rows, err := l.db.Query(`SELECT r.name, r.class, r.host, COALESCE(r.job,''), COALESCE(r.repo,''), COALESCE(r.run_id,0),
	    r.state, COALESCE(r.result,''), COALESCE(r.reason,''), r.created_at, r.started_at, r.finished_at,
	    COALESCE((julianday(r.started_at)-julianday(d.first_seen))*86400, 0),
	    COALESCE((julianday(r.finished_at)-julianday(r.started_at))*86400, 0),
	    COALESCE(r.provision_ms,0), COALESCE(r.cpu_sec,0), COALESCE(r.mem_peak,0), r.exit_code, COALESCE(r.oom_kills,0),
	    COALESCE(r.mem_work_peak,0), COALESCE(r.est_cpus,0), COALESCE(r.est_mem,0)
	  FROM runners r LEFT JOIN demand d ON d.job_id = r.assigned_job_id
	  WHERE r.removed_at IS NOT NULL OR r.state IN ('failed','lost')
	  ORDER BY COALESCE(r.finished_at, r.removed_at, r.created_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FinishedJob{}
	for rows.Next() {
		var j FinishedJob
		var created, started, finished sql.NullString
		var exit sql.NullInt64
		if err := rows.Scan(&j.Runner, &j.Class, &j.Host, &j.Job, &j.Repo, &j.RunID, &j.State, &j.Result, &j.Reason,
			&created, &started, &finished, &j.WaitSec, &j.RunSec, &j.ProvisionMs, &j.CPUSec, &j.MemPeak, &exit, &j.OOMKills,
			&j.MemWorkPeak, &j.EstCPUs, &j.EstMem); err != nil {
			return nil, err
		}
		j.CreatedAt, j.StartedAt, j.FinishedAt = parseTS(created), parseTS(started), parseTS(finished)
		if exit.Valid {
			c := int(exit.Int64)
			j.ExitCode = &c
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Decision is one placement decision as it was recorded.
type Decision struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Host   string    `json:"host"`
	Reason string    `json:"reason"`
}

// RunnerHistory is everything recorded about one runner: its decisions, the
// incidents that name it and the last lines of its output.
func (l *Ledger) RunnerHistory(name string) (ds []Decision, logTail string, err error) {
	rows, err := l.db.Query(`SELECT at, action, COALESCE(host,''), COALESCE(reason,'') FROM decisions WHERE runner=? ORDER BY id`, name)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	ds = []Decision{}
	for rows.Next() {
		var d Decision
		var at sql.NullString
		if err := rows.Scan(&at, &d.Action, &d.Host, &d.Reason); err != nil {
			return nil, "", err
		}
		d.At = parseTS(at)
		ds = append(ds, d)
	}
	var tail sql.NullString
	_ = l.db.QueryRow(`SELECT log_tail FROM runners WHERE name=?`, name).Scan(&tail)
	return ds, tail.String, rows.Err()
}

func (l *Ledger) SetLogTail(name, tail string) error {
	return l.exec(`UPDATE runners SET log_tail=? WHERE name=?`, tail, name)
}

// Span is something that occupied time: a runner on a host, or a job waiting.
type Span struct {
	Class, Host string
	From, To    time.Time // To is zero while it still lasts
}

// RunnerSpans are runners alive at some point since `since`.
func (l *Ledger) RunnerSpans(since time.Time) ([]Span, error) {
	return l.spans(`SELECT class, host, created_at, COALESCE(removed_at, CASE WHEN state IN ('failed','lost') THEN COALESCE(finished_at, created_at) END)
	  FROM runners WHERE COALESCE(removed_at, 'z') >= ?`, since)
}

// WaitSpans are jobs waiting at some point since `since`, from first seen to
// the moment a runner took them (or the wait ended otherwise).
func (l *Ledger) WaitSpans(since time.Time) ([]Span, error) {
	return l.spans(`SELECT class, CASE WHEN served_by LIKE 'unassigned-%' OR served_by IN ('down','ttl') OR served_by LIKE 'run-%' THEN '' ELSE 'served' END,
	  first_seen, closed_at FROM demand WHERE COALESCE(closed_at, 'z') >= ?`, since)
}

func (l *Ledger) spans(q string, since time.Time) ([]Span, error) {
	rows, err := l.db.Query(q, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Span
	for rows.Next() {
		var s Span
		var from, to sql.NullString
		if err := rows.Scan(&s.Class, &s.Host, &from, &to); err != nil {
			return nil, err
		}
		s.From, s.To = parseTS(from), parseTS(to)
		out = append(out, s)
	}
	return out, rows.Err()
}

// JobKey names a job across its runs: the repository, the workflow file, the
// job's name as GitHub shows it (a matrix job's name carries its values), and
// the platform it ran as — the OS and the driver, "linux/docker". The same
// name on another OS is another program, and a container and a process tree
// are not measured as the same thing.
type JobKey struct {
	Repo, Workflow, Job, Platform string
}

// KeyOf is the key of a job as GitHub's messages name it. The workflow comes
// with its ref ("o/r/.github/workflows/ci.yml@refs/heads/main"); runs of the
// same job on any branch are the same job.
func KeyOf(repo, workflowRef, job string) JobKey {
	wf, _, _ := strings.Cut(workflowRef, "@")
	return JobKey{Repo: repo, Workflow: wf, Job: job}
}

// SQL for a runner row's workflow without its ref.
const workflowFile = `CASE WHEN instr(COALESCE(workflow,''),'@')>0 THEN substr(workflow,1,instr(workflow,'@')-1) ELSE COALESCE(workflow,'') END`

// JobRuns are the newest finished runs of a job that were measured, newest
// first. A run counts when its runner is gone (its last sample is in) and its
// memory without page cache was seen: rows from before that was measured say
// too much and are left out. A run that lost a process at its memory limit
// needed more than the limit, whatever the samples caught of it before.
func (l *Ledger) JobRuns(k JobKey, limit int) ([]size.Sample, error) {
	return l.runs(`AND job=?`, limit, k.Repo, k.Workflow, k.Platform, k.Job)
}

// WorkflowRuns are the newest measured runs of any job of a workflow on a
// platform: what a job of it that has never run there is sized by.
func (l *Ledger) WorkflowRuns(repo, workflow, platform string, limit int) ([]size.Sample, error) {
	return l.runs(``, limit, repo, workflow, platform)
}

func (l *Ledger) runs(andJob string, limit int, args ...any) ([]size.Sample, error) {
	rows, err := l.db.Query(`SELECT COALESCE(cpu_sec,0),
	    MAX(mem_work_peak, CASE WHEN COALESCE(oom_kills,0) > 0 THEN COALESCE(limit_mem,0) ELSE 0 END), COALESCE(oom_kills,0),
	    COALESCE((julianday(COALESCE(finished_at, removed_at))-julianday(started_at))*86400, 0), host
	  FROM runners WHERE repo=? AND `+workflowFile+`=? AND COALESCE(platform,'')=? `+andJob+` AND started_at IS NOT NULL
	    AND mem_work_peak > 0 AND (removed_at IS NOT NULL OR state IN ('failed','lost'))
	  ORDER BY started_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []size.Sample
	for rows.Next() {
		var cpuSec, sec float64
		var oom int
		var s size.Sample
		if err := rows.Scan(&cpuSec, &s.MemBytes, &oom, &sec, &s.Host); err != nil {
			return nil, err
		}
		if sec >= 1 {
			s.CPUs, s.Secs = cpuSec/sec, sec
		}
		s.OOM = oom > 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// MeasuredJobs are the jobs with a measured run among the newest finished
// runners, the most recently run first.
func (l *Ledger) MeasuredJobs(limit int) ([]JobKey, error) {
	return l.measuredJobs(``, limit)
}

// AssignedJobs are the jobs GitHub gave a class, whether it ran them or not,
// the most recently given first (their platform is the class's, not set here).
func (l *Ledger) AssignedJobs(class string, limit int) ([]JobKey, error) {
	rows, err := l.db.Query(`SELECT repo, `+workflowFile+`, job FROM demand
	  WHERE class=? AND job IS NOT NULL AND job<>'' AND repo IS NOT NULL
	  GROUP BY 1,2,3 ORDER BY MAX(first_seen) DESC LIMIT ?`, class, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobKey
	for rows.Next() {
		var k JobKey
		if err := rows.Scan(&k.Repo, &k.Workflow, &k.Job); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (l *Ledger) measuredJobs(andClass string, args ...any) ([]JobKey, error) {
	rows, err := l.db.Query(`SELECT repo, `+workflowFile+`, job, platform FROM runners
	  WHERE job IS NOT NULL AND repo IS NOT NULL AND platform IS NOT NULL AND started_at IS NOT NULL AND mem_work_peak > 0
	    AND (removed_at IS NOT NULL OR state IN ('failed','lost')) `+andClass+`
	  GROUP BY 1,2,3,4 ORDER BY MAX(started_at) DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobKey
	for rows.Next() {
		var k JobKey
		if err := rows.Scan(&k.Repo, &k.Workflow, &k.Job, &k.Platform); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
