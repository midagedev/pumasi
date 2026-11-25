// Package ledger is the controller's SQLite record of runners, waiting jobs and
// placement decisions. It outlives controller restarts: GitHub has no runner
// list API and stops repeating a waiting job after three reassignments.
package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
CREATE TABLE IF NOT EXISTS outbox(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL, payload TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, error TEXT, sent_at TEXT
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
	// Columns added after the first release; "duplicate column" means it is there.
	for _, col := range []string{"runners ADD COLUMN log_tail TEXT"} {
		if _, err := db.Exec("ALTER TABLE " + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("schema: %w", err)
		}
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
	JobID, Job               string // Job is the job's display name, known from JobStarted on
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

// CloseOpenDemand closes every waiting job, for when nothing will serve them anymore.
func (l *Ledger) CloseOpenDemand(servedBy string, at time.Time) error {
	return l.exec(`UPDATE demand SET served_by=?, closed_at=? WHERE closed_at IS NULL`, servedBy, ts(at))
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

const runnerCols = `name, class, host, state, COALESCE(runner_id,0), created_at, posted_at, listening_at, COALESCE(assigned_job_id,''), COALESCE(job,''), removed_at`

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
		if err := rows.Scan(&r.Name, &r.Class, &r.Host, &r.State, &r.RunnerID, &created, &posted, &listening, &r.JobID, &r.Job, &removed); err != nil {
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
	ExitCode    *int      `json:"exitCode"`
}

// RecentJobs are the newest runners that are over, newest first.
func (l *Ledger) RecentJobs(limit int) ([]FinishedJob, error) {
	rows, err := l.db.Query(`SELECT r.name, r.class, r.host, COALESCE(r.job,''), COALESCE(r.repo,''), COALESCE(r.run_id,0),
	    r.state, COALESCE(r.result,''), COALESCE(r.reason,''), r.created_at, r.started_at, r.finished_at,
	    COALESCE((julianday(r.started_at)-julianday(d.first_seen))*86400, 0),
	    COALESCE((julianday(r.finished_at)-julianday(r.started_at))*86400, 0),
	    COALESCE(r.provision_ms,0), COALESCE(r.cpu_sec,0), COALESCE(r.mem_peak,0), r.exit_code
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
			&created, &started, &finished, &j.WaitSec, &j.RunSec, &j.ProvisionMs, &j.CPUSec, &j.MemPeak, &exit); err != nil {
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
