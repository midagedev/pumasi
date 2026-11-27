package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

// What operators change while `up` runs (the API in opsapi.go, the CLI over
// it): a job put before every other one, the mode, rules before the file's.
// All of it lives in the ledger, so a restart keeps it and what was done can
// be read back. None of it touches a running job.

// Modes. high-only places only expedited and high jobs, for a while.
const (
	ModeNormal   = "normal"
	ModeHighOnly = "high-only"
	modeKey      = "mode"
	// A mode that is not normal always ends by itself: forgotten, it would
	// hold every other job back.
	modeDefaultFor = time.Hour
	modeMaxFor     = 24 * time.Hour
)

// Mode is the operating switch of the line.
type Mode struct {
	Name  string     `json:"name"`
	Until *time.Time `json:"until"` // nil = until changed (normal)
	By    string     `json:"by"`
	At    *time.Time `json:"at,omitempty"`
}

// CurrentMode is the mode in the ledger; one whose time is up is normal.
func CurrentMode(l *ledger.Ledger, now time.Time) Mode {
	m, ok := storedMode(l)
	if !ok || m.Name != ModeNormal && m.Until != nil && !now.Before(*m.Until) {
		return Mode{Name: ModeNormal}
	}
	return m
}

func storedMode(l *ledger.Ledger) (Mode, bool) {
	v, _, ok := l.Setting(modeKey)
	var m Mode
	if !ok || json.Unmarshal([]byte(v), &m) != nil || m.Name == "" {
		return Mode{Name: ModeNormal}, false
	}
	return m, true
}

// modeNow is the mode for this round. A mode whose time is up is ended here,
// once, and said so.
func (c *Controller) modeNow(now time.Time) Mode {
	m, ok := storedMode(c.ledger)
	if ok && m.Name != ModeNormal && m.Until != nil && !now.Before(*m.Until) {
		over := Mode{Name: ModeNormal, By: "its time was up", At: &now}
		if err := c.saveMode(over, now); err != nil {
			c.log.Error("ledger mode", "err", err)
		}
		c.log.Info(fmt.Sprintf("mode %s is over (set by %s for %s): every job gets room again", m.Name, m.By, m.Until.Sub(*m.At).Round(time.Second)))
		c.decision(now, "", "", "mode", "", "normal: "+m.Name+" was over")
		return over
	}
	return m
}

func (c *Controller) saveMode(m Mode, now time.Time) error {
	b, _ := json.Marshal(m)
	return c.ledger.SetSetting(modeKey, string(b), now)
}

// setMode switches the mode. for is how long a mode other than normal lasts.
func (c *Controller) setMode(name string, forDur time.Duration, by string, now time.Time) (Mode, error) {
	m := Mode{Name: name, By: by, At: &now}
	switch name {
	case ModeNormal:
	case ModeHighOnly:
		if forDur == 0 {
			forDur = modeDefaultFor
		}
		if forDur < time.Minute || forDur > modeMaxFor {
			return Mode{}, opsErr(http.StatusBadRequest, "for: between 1m and %s", modeMaxFor)
		}
		until := now.Add(forDur)
		m.Until = &until
	default:
		return Mode{}, opsErr(http.StatusBadRequest, "mode %q: normal or %s", name, ModeHighOnly)
	}
	if err := c.saveMode(m, now); err != nil {
		return Mode{}, err
	}
	words := name
	if m.Until != nil {
		words += " until " + m.Until.Local().Format("15:04:05")
	}
	c.log.Info(fmt.Sprintf("mode %s (by %s)", words, by))
	c.decision(now, "", "", "mode", "", words+" by "+by)
	c.kickRound()
	return m, nil
}

// modeReason is what a job held back by the mode waits for ("" = nothing holds it back).
func modeReason(m Mode) string {
	if m.Name != ModeHighOnly || m.Until == nil {
		return ""
	}
	return fmt.Sprintf("mode high-only until %s (by %s): only expedited and high jobs get new runners", m.Until.Local().Format("15:04"), m.By)
}

// runtimeRules are the rules added while `up` runs that are in force now.
func (c *Controller) runtimeRules(now time.Time) []config.Rule {
	rs, err := RuntimeRules(c.ledger, now)
	if err != nil {
		c.log.Error("ledger rules", "err", err)
	}
	return rs
}

// RuntimeRules are the rules added while `up` runs that are in force at now,
// as the ledger keeps them.
func RuntimeRules(l *ledger.Ledger, now time.Time) ([]config.Rule, error) {
	rs, err := l.Rules(now)
	if err != nil {
		return nil, err
	}
	var out []config.Rule
	for _, r := range rs {
		var pr config.PriorityRule
		if err := json.Unmarshal([]byte(r.Rule), &pr); err != nil || pr.Validate() != nil {
			continue
		}
		out = append(out, config.Rule{PriorityRule: pr, ID: ledger.RuleID(r.ID), Runtime: true, By: r.By, At: r.At, Expires: r.Expires})
	}
	return out, nil
}

// rulesNow is every rule in force: the runtime ones, then the file's.
func (c *Controller) rulesNow(now time.Time) []config.Rule {
	rt := c.runtimeRules(now)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Rules(rt)
}

// addRule puts a rule before the others; for > 0 ends it by itself.
func (c *Controller) addRule(pr config.PriorityRule, forDur time.Duration, by string, now time.Time) (config.Rule, error) {
	if err := checkRule(pr, forDur); err != nil {
		return config.Rule{}, err
	}
	b, _ := json.Marshal(pr)
	var exp time.Time
	if forDur > 0 {
		exp = now.Add(forDur)
	}
	r, err := c.ledger.AddRule(string(b), by, now, exp)
	if err != nil {
		return config.Rule{}, err
	}
	out := config.Rule{PriorityRule: pr, ID: ledger.RuleID(r.ID), Runtime: true, By: by, At: now, Expires: exp}
	c.log.Info(fmt.Sprintf("priority: runtime rule %s %s (%s) by %s%s", out.ID, pr.Level, pr, by, cond(exp.IsZero(), "", " until "+exp.Local().Format("15:04"))))
	c.decision(now, "", "", "priority", "", fmt.Sprintf("runtime rule %s added by %s: %s (%s)", out.ID, by, pr.Level, pr))
	c.kickRound()
	return out, nil
}

// replaceRule changes a runtime rule in force.
func (c *Controller) replaceRule(id string, pr config.PriorityRule, forDur time.Duration, by string, now time.Time) (config.Rule, error) {
	n, err := c.runtimeRuleID(id)
	if err != nil {
		return config.Rule{}, err
	}
	if err := checkRule(pr, forDur); err != nil {
		return config.Rule{}, err
	}
	b, _ := json.Marshal(pr)
	var exp time.Time
	if forDur > 0 {
		exp = now.Add(forDur)
	}
	ok, err := c.ledger.ReplaceRule(n, string(b), by, now, exp)
	if err != nil {
		return config.Rule{}, err
	}
	if !ok {
		return config.Rule{}, opsErr(http.StatusNotFound, "no runtime rule %s in force", id)
	}
	out := config.Rule{PriorityRule: pr, ID: ledger.RuleID(n), Runtime: true, By: by, At: now, Expires: exp}
	c.decision(now, "", "", "priority", "", fmt.Sprintf("runtime rule %s changed by %s: %s (%s)", out.ID, by, pr.Level, pr))
	c.kickRound()
	return out, nil
}

func (c *Controller) removeRule(id, by string, now time.Time) error {
	n, err := c.runtimeRuleID(id)
	if err != nil {
		return err
	}
	ok, err := c.ledger.RemoveRule(n, by, now)
	if err != nil {
		return err
	}
	if !ok {
		return opsErr(http.StatusNotFound, "no runtime rule %s in force", id)
	}
	c.log.Info(fmt.Sprintf("priority: runtime rule %s removed by %s", id, by))
	c.decision(now, "", "", "priority", "", fmt.Sprintf("runtime rule %s removed by %s", ledger.RuleID(n), by))
	c.kickRound()
	return nil
}

func (c *Controller) runtimeRuleID(id string) (int64, error) {
	if strings.HasPrefix(id, "file-") {
		return 0, opsErr(http.StatusConflict, "rule %s is in %s: the file's rules are changed in the file (a running up reads it again)", id, c.cfg.Path)
	}
	n, err := ledger.ParseRuleID(id)
	if err != nil {
		return 0, opsErr(http.StatusNotFound, "%v", err)
	}
	return n, nil
}

func checkRule(pr config.PriorityRule, forDur time.Duration) error {
	if err := pr.Validate(); err != nil {
		return opsErr(http.StatusBadRequest, "%v", err)
	}
	if forDur < 0 {
		return opsErr(http.StatusBadRequest, "for must not be negative")
	}
	return nil
}

// Expedited is one job put first in line, as the API shows it.
type Expedited struct {
	Job         string     `json:"job"` // pumasi's id of the job while its class holds it ("" = GitHub still holds it)
	GitHubJobID int64      `json:"githubJobId,omitempty"`
	Name        string     `json:"name"`
	Repo        string     `json:"repo"`
	RunID       int64      `json:"runId"`
	Class       string     `json:"class"`
	Since       time.Time  `json:"since"`
	By          string     `json:"by"`
	Ahead       int        `json:"ahead"`             // jobs before it in its class that go first
	Pending     bool       `json:"pending,omitempty"` // its class has not been given it yet
	Position    int        `json:"position"`          // its place in the line (1 = next); 0 = a runner is up for it
	URL         string     `json:"url"`
	Ended       string     `json:"ended,omitempty"` // how it ended (recent ones only)
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	Runner      string     `json:"runner,omitempty"`
	// After: from expedited to started, in seconds (recent ones that started).
	AfterSeconds *int64 `json:"afterSeconds,omitempty"`
}

// ExpediteResult answers POST /api/expedite.
type ExpediteResult struct {
	OK bool `json:"ok"`
	Expedited
	Already bool   `json:"already,omitempty"`
	Note    string `json:"note"`
}

// opsError is a refusal with the HTTP status it is answered with.
type opsError struct {
	code int
	msg  string
}

func (e *opsError) Error() string { return e.msg }

func opsErr(code int, format string, args ...any) error {
	return &opsError{code, fmt.Sprintf(format, args...)}
}

// jobRef is a job as an operator named it, once it is known which job that is.
type jobRef struct {
	Repo    string
	RunID   int64
	Name    string
	GHJobID int64
	// What GitHub said of it (Asked), and of its run.
	Asked              bool
	Status, Conclusion string
	Runner             string
	Labels             []string
	QueuedAt, Started  time.Time
	Workflow, Event    string
}

var (
	jobURLRE = regexp.MustCompile(`^https?://[^/]+/([^/\s]+/[^/\s]+)/actions/runs/(\d+)/job/(\d+)`)
	runURLRE = regexp.MustCompile(`^https?://[^/]+/([^/\s]+/[^/\s]+)/actions/runs/(\d+)(?:/attempts/\d+)?/?(?:[?#].*)?$`)
)

// name reads what an operator gave: pumasi's job id, GitHub's job id, a job's
// URL, or a run's URL with the job's name. It asks GitHub only for what the
// ledger does not tell.
func (c *Controller) resolve(ctx context.Context, ref, name string) (jobRef, error) {
	ref = strings.TrimSpace(ref)
	var j jobRef
	switch m := jobURLRE.FindStringSubmatch(ref); {
	case ref == "":
		return j, opsErr(http.StatusBadRequest, "job: pumasi's job id, GitHub's job id, the job's URL, or a run's URL with name")
	case m != nil:
		j.Repo = m[1]
		j.RunID, _ = strconv.ParseInt(m[2], 10, 64)
		j.GHJobID, _ = strconv.ParseInt(m[3], 10, 64)
	case runURLRE.MatchString(ref):
		m := runURLRE.FindStringSubmatch(ref)
		j.Repo = m[1]
		j.RunID, _ = strconv.ParseInt(m[2], 10, 64)
		j.Name = name
		if j.Name == "" {
			open, err := c.ledger.OpenDemand()
			if err != nil {
				return j, err
			}
			var names []string
			for _, d := range open {
				if d.Repo == j.Repo && d.RunID == j.RunID && !slices.Contains(names, d.Job) {
					names = append(names, d.Job)
				}
			}
			switch len(names) {
			case 0:
				return j, opsErr(http.StatusNotFound, "no job of run %d waits here: give the job's URL (…/actions/runs/%d/job/<id>) or name it", j.RunID, j.RunID)
			case 1:
				j.Name = names[0]
			default:
				return j, opsErr(http.StatusBadRequest, "run %d has %d jobs waiting, name one: %s", j.RunID, len(names), strings.Join(names, "; "))
			}
		}
	case isDigits(ref):
		j.GHJobID, _ = strconv.ParseInt(ref, 10, 64)
	default:
		d, ok, err := c.ledger.DemandRow(ref)
		if err != nil {
			return j, err
		}
		if !ok {
			return j, opsErr(http.StatusNotFound, "no job %q: give pumasi's job id (waitingJobs[].id in /api/state), GitHub's job id, or the job's URL", ref)
		}
		j.Repo, j.RunID, j.Name = d.Repo, d.RunID, d.Job
	}
	if j.GHJobID != 0 {
		repos := []string{j.Repo}
		if j.Repo == "" {
			repos = c.reposToAsk()
			if len(repos) == 0 {
				return j, opsErr(http.StatusBadRequest, "GitHub's job id alone names no repository here: give the job's URL")
			}
		}
		var lastErr error
		for _, repo := range repos {
			gj, err := c.githubJob(ctx, repo, j.GHJobID)
			if err != nil {
				lastErr = err
				continue
			}
			j.Repo = repo
			j.fromGitHub(gj)
			lastErr = nil
			break
		}
		if lastErr != nil {
			var ge ghError
			switch {
			case j.Name == "" && errors.As(lastErr, &ge) && ge.code == http.StatusNotFound:
				return j, opsErr(http.StatusNotFound, "GitHub has no job %d in %s (or these credentials cannot read its Actions)", j.GHJobID, strings.Join(repos, ", "))
			case j.Name == "":
				return j, opsErr(http.StatusBadGateway, "GitHub does not say which job %d is: %v", j.GHJobID, lastErr)
			}
			c.log.Warn("expedite: GitHub does not say what job it is", "job", j.GHJobID, "err", lastErr)
		}
	} else if j.Repo != "" && j.RunID != 0 && j.Name != "" {
		// Its GitHub id gives it a link, and lets pumasi ask after it while
		// its class does not hold it. Not needed to expedite it.
		if gj, err := c.runJob(ctx, j.Repo, j.RunID, j.Name); err == nil {
			j.fromGitHub(gj)
		}
	}
	return j, nil
}

func (j *jobRef) fromGitHub(gj ghJob) {
	j.Asked = true
	j.GHJobID, j.RunID, j.Name = gj.ID, gj.RunID, gj.Name
	j.Status, j.Conclusion, j.Runner, j.Labels = gj.Status, gj.Conclusion, gj.RunnerName, gj.Labels
	j.QueuedAt, j.Started = gj.CreatedAt, gj.StartedAt
}

// waits says whether GitHub's status is one of a job that has not started.
func (j jobRef) waits() bool {
	switch j.Status {
	case "queued", "waiting", "pending", "requested":
		return true
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// reposToAsk are where a bare GitHub job id may be: the config's repository,
// or for an organization the repositories of the jobs waiting here.
func (c *Controller) reposToAsk() []string {
	if u, err := url.Parse(c.cfg.URL); err == nil {
		if parts := strings.Split(strings.Trim(u.Path, "/"), "/"); len(parts) == 2 {
			return []string{parts[0] + "/" + parts[1]}
		}
	}
	open, _ := c.ledger.OpenDemand()
	var out []string
	for _, d := range open {
		if d.Repo != "" && !slices.Contains(out, d.Repo) {
			out = append(out, d.Repo)
		}
	}
	return out
}

func sameJob(e ledger.Expedite, repo string, runID int64, name string) bool {
	return e.Repo == repo && e.RunID == runID && e.Job == name
}

// expedite puts a job before every other one. The job is matched by its
// repository, run and name: GitHub gives it a new id each time it assigns it
// again.
func (c *Controller) expedite(ctx context.Context, ref, name, by string, now time.Time) (ExpediteResult, error) {
	j, err := c.resolve(ctx, ref, name)
	if err != nil {
		return ExpediteResult{}, err
	}
	active, err := c.ledger.ActiveExpedites()
	if err != nil {
		return ExpediteResult{}, err
	}
	for _, e := range active {
		if sameJob(e, j.Repo, j.RunID, j.Name) {
			res := c.expeditedNow(e, active, now)
			return ExpediteResult{OK: true, Expedited: res, Already: true,
				Note: fmt.Sprintf("already expedited by %s at %s. %s", e.By, e.At.Local().Format("15:04:05"), aheadNote(res))}, nil
		}
	}
	rows, err := c.ledger.DemandOf(j.Repo, j.RunID, j.Name)
	if err != nil {
		return ExpediteResult{}, err
	}
	e := ledger.Expedite{Repo: j.Repo, RunID: j.RunID, Job: j.Name, GHJobID: j.GHJobID, By: by, At: now, QueuedAt: j.QueuedAt,
		Labels: strings.Join(j.Labels, ",")}
	var held *ledger.Demand
	for i := range rows {
		if rows[i].ClosedAt.IsZero() {
			held = &rows[i]
			break
		}
	}
	switch {
	case held != nil:
		e.Class, e.Workflow, e.Event, e.QueuedAt = held.Class, held.Workflow, held.Event, held.FirstSeen
	case !j.Asked:
		return ExpediteResult{}, opsErr(http.StatusNotFound, "%s of run %d does not wait here, and GitHub could not be asked about it (does the token read Actions?)", j.Name, j.RunID)
	case j.Status == "in_progress":
		return ExpediteResult{}, opsErr(http.StatusConflict, "%s of run %d is not waiting: it runs on %s", j.Name, j.RunID, cmpOr(j.Runner, "a runner"))
	case !j.waits():
		return ExpediteResult{}, opsErr(http.StatusConflict, "%s of run %d is not waiting: %s %s", j.Name, j.RunID, j.Status, j.Conclusion)
	default:
		// GitHub holds it: a class that takes only what fits has not taken it.
		cl := c.classFor(j.Labels)
		if cl == nil {
			return ExpediteResult{}, opsErr(http.StatusNotFound, "%s of run %d asks for runs-on [%s]: no class of this controller takes that", j.Name, j.RunID, strings.Join(j.Labels, ", "))
		}
		e.Class = cl.cfg.Name
		if w, ev, err := c.runWorkflow(ctx, j.Repo, j.RunID); err == nil {
			e.Workflow, e.Event = w, ev
		}
	}
	if e, err = c.ledger.AddExpedite(e); err != nil {
		return ExpediteResult{}, err
	}
	active = append(active, e)
	res := c.expeditedNow(e, active, now)
	where := "waits in class " + e.Class
	if held == nil {
		where = "GitHub holds it for class " + e.Class
	}
	c.log.Info(fmt.Sprintf("expedite %s of run %d by %s: %s; %d of its class %s first", e.Job, e.RunID, by, where, res.Ahead, plural(res.Ahead, "goes", "go")))
	c.decision(now, e.Class, "", "expedite", "", fmt.Sprintf("%s of %s run %d expedited by %s (%s)", e.Job, e.Repo, e.RunID, by, where))
	c.kickRound()
	return ExpediteResult{OK: true, Expedited: res, Note: aheadNote(res)}, nil
}

// aheadNote says what an operator cannot change: GitHub gives a class its
// oldest job first.
func aheadNote(x Expedited) string {
	var notes []string
	if x.Ahead > 0 {
		notes = append(notes, fmt.Sprintf("%d older %s of class %s %s first, with it: GitHub gives a class its oldest job first", x.Ahead, plural(x.Ahead, "job", "jobs"), x.Class, plural(x.Ahead, "goes", "go")))
	}
	if x.Pending {
		notes = append(notes, fmt.Sprintf("GitHub still holds it: class %s takes it when there is room for it, and jobs of its labels that GitHub queued before it come first", x.Class))
	}
	if len(notes) == 0 {
		return "first in line: it gets the next room that fits it"
	}
	return strings.Join(notes, "; ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// clearExpedite takes a job out of the front of the line.
func (c *Controller) clearExpedite(ref, by string, now time.Time) (ledger.Expedite, error) {
	active, err := c.ledger.ActiveExpedites()
	if err != nil {
		return ledger.Expedite{}, err
	}
	match := func(f func(ledger.Expedite) bool) (ledger.Expedite, bool) {
		for _, e := range active {
			if f(e) {
				return e, true
			}
		}
		return ledger.Expedite{}, false
	}
	ref = strings.TrimSpace(ref)
	var e ledger.Expedite
	var ok bool
	switch m := jobURLRE.FindStringSubmatch(ref); {
	case m != nil:
		id, _ := strconv.ParseInt(m[3], 10, 64)
		e, ok = match(func(e ledger.Expedite) bool { return e.GHJobID == id })
	case isDigits(ref):
		id, _ := strconv.ParseInt(ref, 10, 64)
		e, ok = match(func(e ledger.Expedite) bool { return e.GHJobID == id })
	default:
		if d, found, err := c.ledger.DemandRow(ref); err == nil && found {
			e, ok = match(func(e ledger.Expedite) bool { return sameJob(e, d.Repo, d.RunID, d.Job) })
		}
		if !ok && strings.HasPrefix(ref, "x-") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(ref, "x-"), 10, 64)
			e, ok = match(func(e ledger.Expedite) bool { return e.ID == id })
		}
	}
	if !ok {
		return e, opsErr(http.StatusNotFound, "%q is not an expedited job", ref)
	}
	if err := c.ledger.EndExpedite(e.ID, "cleared by "+by, now, time.Time{}, ""); err != nil {
		return e, err
	}
	c.log.Info(fmt.Sprintf("expedite of %s (run %d) cleared by %s", e.Job, e.RunID, by))
	c.decision(now, e.Class, "", "expedite", "", fmt.Sprintf("%s of %s run %d no longer expedited: cleared by %s", e.Job, e.Repo, e.RunID, by))
	c.kickRound()
	return e, nil
}

// expeditedNow is an active expedited job with its place in the line as it
// would be drawn now.
func (c *Controller) expeditedNow(e ledger.Expedite, active []ledger.Expedite, now time.Time) Expedited {
	x := expeditedOf(e)
	open, _ := c.ledger.OpenDemand()
	for _, d := range open {
		if sameJob(e, d.Repo, d.RunID, d.Job) {
			x.Job = d.JobID
		}
	}
	x.Pending = x.Job == ""
	c.mu.Lock()
	covering := c.status.Covering
	c.mu.Unlock()
	line := decide.Line(lineup(open, active, c.rulesNow(now)), covering, c.headStart())
	id := x.Job
	if id == "" {
		id = phantomID(e.ID)
	}
	for i, d := range line {
		if d.ID == id {
			x.Position = i + 1
			for _, o := range line[:i] {
				if o.Class == d.Class && o.Expedite == d.Expedite {
					x.Ahead++
				}
			}
		}
	}
	return x
}

func (c *Controller) headStart() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.PriorityHeadStart
}

func expeditedOf(e ledger.Expedite) Expedited {
	x := Expedited{GitHubJobID: e.GHJobID, Name: e.Job, Repo: e.Repo, RunID: e.RunID, Class: e.Class, Since: e.At, By: e.By, Ended: e.Ended, Runner: e.Runner}
	x.URL = fmt.Sprintf("https://github.com/%s/actions/runs/%d", e.Repo, e.RunID)
	if e.GHJobID != 0 {
		x.URL += fmt.Sprintf("/job/%d", e.GHJobID)
	}
	if !e.StartedAt.IsZero() {
		t := e.StartedAt
		x.StartedAt = &t
		after := int64(max(0, e.StartedAt.Sub(e.At).Seconds()))
		x.AfterSeconds = &after
	}
	return x
}

func phantomID(id int64) string { return "x-" + strconv.FormatInt(id, 10) }

func expeditedWords(n int) string {
	if n == 1 {
		return "expedited"
	}
	return fmt.Sprintf("expedited (#%d)", n)
}

// waitingJob is a job of the line and what its size is looked up by.
type waitingJob struct {
	d   decide.Demand
	key ledger.JobKey
}

// lineup is the jobs that wait, as the line takes them: each with its level
// by the rules, and the expedited ones with their place. An expedited job
// that GitHub still holds for its class is in it too, as pending.
func lineup(open []ledger.Demand, active []ledger.Expedite, rules []config.Rule) []decide.Demand {
	ws := waitingJobs(open, active, rules)
	out := make([]decide.Demand, len(ws))
	for i, w := range ws {
		out[i] = w.d
	}
	return out
}

func waitingJobs(open []ledger.Demand, active []ledger.Expedite, rules []config.Rule) []waitingJob {
	place := map[string]int{}
	for i, e := range active {
		k := e.Repo + "\x00" + strconv.FormatInt(e.RunID, 10) + "\x00" + e.Job
		if _, ok := place[k]; !ok {
			place[k] = i + 1
		}
	}
	held := map[int]bool{}
	var out []waitingJob
	for _, d := range open {
		dd := decide.Demand{ID: d.JobID, Class: d.Class, FirstSeen: d.FirstSeen, Name: d.Job}
		dd.Level, dd.Ranked = config.LevelIn(rules, config.WaitingJob{Class: d.Class, Repo: d.Repo, WorkflowRef: d.Workflow, Name: d.Job, Event: d.Event})
		if n := place[d.Repo+"\x00"+strconv.FormatInt(d.RunID, 10)+"\x00"+d.Job]; n > 0 && !held[n] {
			dd.Expedite, dd.Ranked = n, expeditedWords(n)
			held[n] = true
		}
		out = append(out, waitingJob{dd, ledger.KeyOf(d.Repo, d.Workflow, d.Job)})
	}
	for i, e := range active {
		n := i + 1
		if held[n] || e.Class == "" {
			continue
		}
		dd := decide.Demand{ID: phantomID(e.ID), Class: e.Class, FirstSeen: e.QueuedAt, Name: e.Job,
			Expedite: n, Pending: true, Ranked: expeditedWords(n)}
		if dd.FirstSeen.IsZero() {
			dd.FirstSeen = e.At
		}
		out = append(out, waitingJob{dd, ledger.KeyOf(e.Repo, e.Workflow, e.Job)})
	}
	return out
}

// classFor is the class GitHub gives a job of these labels: one whose labels
// hold them all. Of several, one that comes after none.
func (c *Controller) classFor(labels []string) *class {
	var found []*class
	for _, cl := range c.classes {
		has := cl.cfg.RunsOn()
		if len(labels) > 0 && !slices.ContainsFunc(labels, func(l string) bool {
			return !slices.ContainsFunc(has, func(h string) bool { return strings.EqualFold(h, l) })
		}) {
			found = append(found, cl)
		}
	}
	for _, cl := range found {
		if cl.cfg.After == "" {
			return cl
		}
	}
	if len(found) > 0 {
		return found[0]
	}
	return nil
}

// expediteCheck is how often GitHub is asked after an expedited job that no
// class of pumasi holds.
const expediteCheck = time.Minute

// settleExpedites ends the expedited jobs that started or are over, and keeps
// the rest pointed at where they wait. A job a class holds is followed in the
// ledger; one GitHub holds is asked after at GitHub once a minute.
func (c *Controller) settleExpedites(ctx context.Context, now time.Time) {
	active, err := c.ledger.ActiveExpedites()
	if err != nil {
		c.log.Error("ledger expedites", "err", err)
		return
	}
	if c.expChecked == nil {
		c.expChecked = map[int64]time.Time{}
	}
	for _, e := range active {
		rows, err := c.ledger.DemandOf(e.Repo, e.RunID, e.Job)
		if err != nil {
			continue
		}
		if len(rows) > 0 && rows[0].ClosedAt.IsZero() {
			if rows[0].Class != e.Class || e.Workflow == "" {
				e.Class, e.Workflow, e.Event = rows[0].Class, rows[0].Workflow, rows[0].Event
				_ = c.ledger.UpdateExpedite(e)
			}
			continue
		}
		if len(rows) > 0 {
			switch r := rows[0]; {
			case r.ByRunner():
				c.endExpedite(e, "started", now, r.ClosedAt, r.ServedBy)
				continue
			case strings.HasPrefix(r.ServedBy, "run-"):
				c.endExpedite(e, "its run is over ("+strings.TrimPrefix(r.ServedBy, "run-")+")", now, time.Time{}, "")
				continue
			case r.ServedBy == "ttl" || r.ServedBy == "down":
				c.endExpedite(e, "no longer waits here ("+r.ServedBy+")", now, time.Time{}, "")
				continue
			case now.Sub(r.ClosedAt) < 30*time.Second:
				continue // taken back: GitHub assigns it again in the same breath, or not
			}
		}
		if now.Sub(c.expChecked[e.ID]) < expediteCheck {
			continue
		}
		c.expChecked[e.ID] = now
		var gj ghJob
		if e.GHJobID != 0 {
			gj, err = c.githubJob(ctx, e.Repo, e.GHJobID)
		} else {
			gj, err = c.runJob(ctx, e.Repo, e.RunID, e.Job)
		}
		if err != nil {
			c.log.Warn(fmt.Sprintf("expedited %s of run %d: no class here holds it and GitHub does not say where it is: %v", e.Job, e.RunID, err))
			continue
		}
		var j jobRef
		j.fromGitHub(gj)
		switch {
		case j.Status == "in_progress":
			c.endExpedite(e, "started elsewhere", now, j.Started, j.Runner)
		case j.Status == "completed":
			c.endExpedite(e, "its job is over ("+j.Conclusion+")", now, time.Time{}, "")
		default:
			// Still at GitHub: the class its labels go to keeps room for it.
			changed := e.GHJobID != j.GHJobID
			e.GHJobID = j.GHJobID
			if cl := c.classFor(j.Labels); cl != nil && cl.cfg.Name != e.Class {
				e.Class, changed = cl.cfg.Name, true
			}
			if changed {
				_ = c.ledger.UpdateExpedite(e)
			}
		}
	}
}

// endExpedite closes an expedited job and says what expediting it came to.
func (c *Controller) endExpedite(e ledger.Expedite, how string, now, started time.Time, runner string) {
	if err := c.ledger.EndExpedite(e.ID, how, now, started, runner); err != nil {
		c.log.Error("ledger expedite", "err", err)
		return
	}
	delete(c.expChecked, e.ID)
	words := fmt.Sprintf("expedited %s of run %d: %s", e.Job, e.RunID, how)
	if !started.IsZero() {
		words += fmt.Sprintf(" on %s %s after it was expedited", cmpOr(runner, "a runner"), max(0, started.Sub(e.At)).Round(time.Second))
		if !e.QueuedAt.IsZero() {
			words += fmt.Sprintf(" (it waited %s in all)", max(0, started.Sub(e.QueuedAt)).Round(time.Second))
		}
	}
	c.log.Info(words)
	c.decision(now, e.Class, "", "expedite", runner, words)
}

func (c *Controller) kickRound() {
	if c.kick == nil {
		return
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// GitHub's REST API, for what the scale set messages do not carry: a job's
// number, its labels and whether it started somewhere else.

// githubAPI is GitHub's REST API (a var for tests).
var githubAPI = "https://api.github.com"

type ghJob struct {
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Labels     []string  `json:"labels"`
	RunnerName string    `json:"runner_name"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
}

func (c *Controller) githubGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+path, nil)
	if err != nil {
		return err
	}
	bearer, err := c.auth.Bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubREST.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghError{resp.StatusCode, fmt.Sprintf("GET %s: %s", path, resp.Status)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ghError is an answer of GitHub's REST API other than 200.
type ghError struct {
	code int
	msg  string
}

func (e ghError) Error() string { return e.msg }

func (c *Controller) githubJob(ctx context.Context, repo string, id int64) (ghJob, error) {
	var j ghJob
	err := c.githubGet(ctx, fmt.Sprintf("/repos/%s/actions/jobs/%d", repo, id), &j)
	return j, err
}

// runJob is the job of a run by its name (its latest attempt).
func (c *Controller) runJob(ctx context.Context, repo string, runID int64, name string) (ghJob, error) {
	var page struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := c.githubGet(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, runID), &page); err != nil {
		return ghJob{}, err
	}
	for _, j := range page.Jobs {
		if j.Name == name {
			return j, nil
		}
	}
	return ghJob{}, errors.New("run has no job " + strconv.Quote(name))
}

// runWorkflow is a run's workflow file as a job's message names it
// (owner/repo/.github/workflows/x.yml@refs/heads/main), and its event.
func (c *Controller) runWorkflow(ctx context.Context, repo string, runID int64) (workflow, event string, err error) {
	var run struct {
		Path       string `json:"path"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
	}
	if err := c.githubGet(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), &run); err != nil {
		return "", "", err
	}
	workflow = repo + "/" + run.Path
	if run.HeadBranch != "" {
		workflow += "@refs/heads/" + run.HeadBranch
	}
	return workflow, run.Event, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
