package ledger

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// What operators change while `pumasi up` runs: jobs put first in line, rules
// added before the config file's, and the mode. Kept here so that a restart
// keeps them, and so that what was done, by whom and with what effect can be
// read back.
const opsSchema = `
CREATE TABLE IF NOT EXISTS expedites(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo TEXT NOT NULL, run_id INTEGER NOT NULL,
  job TEXT NOT NULL,               -- the job's name as GitHub shows it; with repo and run it outlives a reassignment
  gh_job_id INTEGER,               -- the number in the job's URL, when known
  class TEXT,                      -- the class it waits in, or that is to take it
  workflow TEXT, labels TEXT, event TEXT,
  queued_at TEXT,                  -- when the job began to wait
  by TEXT NOT NULL,                -- who expedited it: "cli alice@127.0.0.1", "api 10.0.0.3"
  at TEXT NOT NULL,                -- when
  ended_at TEXT, ended TEXT,       -- started | started elsewhere | cleared | run over | ...
  started_at TEXT, runner TEXT     -- at -> started_at is what expediting it bought
);
CREATE TABLE IF NOT EXISTS priority_rules(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  rule TEXT NOT NULL,              -- {"match": {...}, "level": "high"} as JSON
  by TEXT NOT NULL, at TEXT NOT NULL,
  expires_at TEXT,                 -- NULL = until removed
  removed_at TEXT, removed_by TEXT
);
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY, value TEXT NOT NULL, at TEXT NOT NULL
);
`

// Expedite is a job an operator put before every other one.
type Expedite struct {
	ID       int64
	Repo     string
	RunID    int64
	Job      string // its name as GitHub shows it
	GHJobID  int64  // 0 = not known
	Class    string
	Workflow string // the workflow file with its ref, as the job's demand has it
	Labels   string
	Event    string
	QueuedAt time.Time
	By       string
	At       time.Time
	// Over: EndedAt is set; Ended says how. StartedAt and Runner when it started.
	EndedAt   time.Time
	Ended     string
	StartedAt time.Time
	Runner    string
}

const expediteCols = `id, repo, run_id, job, COALESCE(gh_job_id,0), COALESCE(class,''), COALESCE(workflow,''), COALESCE(labels,''), COALESCE(event,''),
  queued_at, by, at, ended_at, COALESCE(ended,''), started_at, COALESCE(runner,'')`

// AddExpedite records a job put first in line and returns it with its id.
func (l *Ledger) AddExpedite(e Expedite) (Expedite, error) {
	res, err := l.db.Exec(`INSERT INTO expedites(repo, run_id, job, gh_job_id, class, workflow, labels, event, queued_at, by, at) VALUES(?,?,?,NULLIF(?,0),?,?,?,?,?,?,?)`,
		e.Repo, e.RunID, e.Job, e.GHJobID, e.Class, e.Workflow, e.Labels, e.Event, ts(e.QueuedAt), e.By, ts(e.At))
	if err != nil {
		return e, err
	}
	e.ID, err = res.LastInsertId()
	return e, err
}

// ActiveExpedites are the expedited jobs that have not started or ended, in
// the order they were expedited.
func (l *Ledger) ActiveExpedites() ([]Expedite, error) {
	return l.expedites(`ended_at IS NULL ORDER BY id`)
}

// RecentExpedites are the expedited jobs that are over, the newest first.
func (l *Ledger) RecentExpedites(limit int) ([]Expedite, error) {
	return l.expedites(`ended_at IS NOT NULL ORDER BY ended_at DESC, id DESC LIMIT ?`, limit)
}

// UpdateExpedite sets what was learned of an expedited job since: where it
// waits, its GitHub id, its workflow.
func (l *Ledger) UpdateExpedite(e Expedite) error {
	return l.exec(`UPDATE expedites SET class=?, gh_job_id=NULLIF(?,0), workflow=?, labels=?, event=?, queued_at=COALESCE(queued_at, ?) WHERE id=?`,
		e.Class, e.GHJobID, e.Workflow, e.Labels, e.Event, ts(e.QueuedAt), e.ID)
}

// EndExpedite closes an expedited job: how it ended and, when it started,
// when and on which runner.
func (l *Ledger) EndExpedite(id int64, ended string, at, startedAt time.Time, runner string) error {
	return l.exec(`UPDATE expedites SET ended=?, ended_at=?, started_at=?, runner=NULLIF(?,'') WHERE id=? AND ended_at IS NULL`,
		ended, ts(at), ts(startedAt), runner, id)
}

func (l *Ledger) expedites(where string, args ...any) ([]Expedite, error) {
	rows, err := l.db.Query(`SELECT `+expediteCols+` FROM expedites WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Expedite
	for rows.Next() {
		var e Expedite
		var queued, at, ended, started sql.NullString
		if err := rows.Scan(&e.ID, &e.Repo, &e.RunID, &e.Job, &e.GHJobID, &e.Class, &e.Workflow, &e.Labels, &e.Event,
			&queued, &e.By, &at, &ended, &e.Ended, &started, &e.Runner); err != nil {
			return nil, err
		}
		e.QueuedAt, e.At, e.EndedAt, e.StartedAt = parseTS(queued), parseTS(at), parseTS(ended), parseTS(started)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DemandOf is every row of one job — a repository's run and a job name —
// the newest first. GitHub gives a job a new id each time it assigns it
// again; the run and name stay.
func (l *Ledger) DemandOf(repo string, runID int64, job string) ([]Demand, error) {
	return l.demand(`repo=? AND run_id=? AND job=? ORDER BY first_seen DESC, last_seen DESC`, repo, runID, job)
}

// DemandRow is one row of demand by its id (ok false = none).
func (l *Ledger) DemandRow(jobID string) (Demand, bool, error) {
	ds, err := l.demand(`job_id=?`, jobID)
	if err != nil || len(ds) == 0 {
		return Demand{}, false, err
	}
	return ds[0], true, nil
}

// ByRunner says whether a row of demand was closed by a runner taking the job
// (served_by is then the runner's name).
func (d Demand) ByRunner() bool {
	s := d.ServedBy
	return s != "" && !strings.HasPrefix(s, "unassigned-") && !strings.HasPrefix(s, "run-") && s != "ttl" && s != "down"
}

// Rule is a priority rule added while `pumasi up` runs. Rule is its
// {match, level} as JSON; the ledger does not read it.
type Rule struct {
	ID      int64
	Rule    string
	By      string
	At      time.Time
	Expires time.Time // zero = until removed
}

// RuleID is how a runtime rule is named to people: r-<n>.
func RuleID(id int64) string { return "r-" + strconv.FormatInt(id, 10) }

// ParseRuleID reads r-<n> (or <n>).
func ParseRuleID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "r-"), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a runtime rule (r-<n>)", s)
	}
	return n, nil
}

func (l *Ledger) AddRule(rule, by string, at, expires time.Time) (Rule, error) {
	res, err := l.db.Exec(`INSERT INTO priority_rules(rule, by, at, expires_at) VALUES(?,?,?,?)`, rule, by, ts(at), ts(expires))
	if err != nil {
		return Rule{}, err
	}
	id, err := res.LastInsertId()
	return Rule{ID: id, Rule: rule, By: by, At: at, Expires: expires}, err
}

// ReplaceRule changes a rule in force. ok is false when there is none by that id.
func (l *Ledger) ReplaceRule(id int64, rule, by string, at, expires time.Time) (bool, error) {
	res, err := l.db.Exec(`UPDATE priority_rules SET rule=?, by=?, at=?, expires_at=? WHERE id=? AND removed_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		rule, by, ts(at), ts(expires), id, ts(at))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RemoveRule takes a rule out of force. ok is false when there is none in force by that id.
func (l *Ledger) RemoveRule(id int64, by string, at time.Time) (bool, error) {
	res, err := l.db.Exec(`UPDATE priority_rules SET removed_at=?, removed_by=? WHERE id=? AND removed_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		ts(at), by, id, ts(at))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Rules are the runtime rules in force at a moment, the first added first.
func (l *Ledger) Rules(now time.Time) ([]Rule, error) {
	rows, err := l.db.Query(`SELECT id, rule, by, at, expires_at FROM priority_rules
	  WHERE removed_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var at, exp sql.NullString
		if err := rows.Scan(&r.ID, &r.Rule, &r.By, &at, &exp); err != nil {
			return nil, err
		}
		r.At, r.Expires = parseTS(at), parseTS(exp)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Setting is a value an operator set (ok false = never set).
func (l *Ledger) Setting(key string) (value string, at time.Time, ok bool) {
	var v, a sql.NullString
	if err := l.db.QueryRow(`SELECT value, at FROM settings WHERE key=?`, key).Scan(&v, &a); err != nil {
		return "", time.Time{}, false
	}
	return v.String, parseTS(a), true
}

func (l *Ledger) SetSetting(key, value string, at time.Time) error {
	return l.exec(`INSERT INTO settings(key, value, at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, at=excluded.at`, key, value, ts(at))
}
