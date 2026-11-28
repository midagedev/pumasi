package controller

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
)

// The demo's priority controls: expedite a waiting job, high-only mode, rules
// for a while. They answer as the controller's do (opsapi.go): the same
// paths, bodies, answers and refusals, and the same types. They act through
// the real placement like everything else in the demo. What people do here
// is kept in memory as a log with the simulated second it happened, so the
// world can be worked out again with it in place; the seed does not replay it.
// Anyone who reaches the demo may write: it changes only its simulation.

type demoOps struct {
	expedites []demoExpedite
	modes     []demoModeEv
	rules     []demoRuleEv
	nextRule  int
}

type demoExpedite struct {
	job, by      string
	since, until float64 // until = when it was taken back (+Inf = not)
	clearedBy    string
}

type demoModeEv struct {
	name, by    string
	from, until float64 // until: when high-only ends by itself (+Inf = not)
}

type demoRuleEv struct {
	rule        config.Rule // ID, Runtime and By; At and Expires are worked out for a view
	from, until float64     // until: when it expires or was removed (+Inf = neither)
	expires     float64     // +Inf = never
}

func (o demoOps) clone() demoOps {
	return demoOps{expedites: slices.Clone(o.expedites), modes: slices.Clone(o.modes), rules: slices.Clone(o.rules), nextRule: o.nextRule}
}

// activeAt is the expedites in force at t, in the order they were made.
func (o demoOps) activeAt(t float64) []demoExpedite {
	var out []demoExpedite
	for _, e := range o.expedites {
		if e.since <= t && t < e.until {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b demoExpedite) int { return cmp.Compare(a.since, b.since) })
	return out
}

// expediteAt is a job's place among the expedited jobs at t (1 = the first
// expedited; 0 = not expedited), as decide.Demand.Expedite takes it.
func (o demoOps) expediteAt(job string, t float64) int {
	for i, e := range o.activeAt(t) {
		if e.job == job {
			return i + 1
		}
	}
	return 0
}

func (o demoOps) expeditedAt(job string, t float64) bool { return o.expediteAt(job, t) > 0 }

func (o demoOps) modeAt(t float64) (name string, until float64) {
	name, until = ModeNormal, math.Inf(1)
	for _, m := range o.modes {
		if m.from <= t {
			name, until = m.name, m.until
		}
	}
	if name == ModeHighOnly && t >= until {
		return ModeNormal, math.Inf(1)
	}
	return name, until
}

func (o demoOps) modeBy(t float64) string {
	by := ""
	for _, m := range o.modes {
		if m.from <= t {
			by = m.by
		}
	}
	return by
}

func (o demoOps) rulesAt(t float64) []demoRuleEv {
	var out []demoRuleEv
	for _, r := range o.rules {
		if r.from <= t && t < r.until {
			out = append(out, r)
		}
	}
	return out
}

// runtimeRules are the rules set here in force at t, in the order they were
// added as the controller's ledger gives them; config.Rules puts them before
// the file's.
func (o demoOps) runtimeRules(t float64) []config.Rule {
	var out []config.Rule
	for _, r := range o.rulesAt(t) {
		out = append(out, r.rule)
	}
	return out
}

// standing is a waiting job's level, the rule behind it, and its place among
// the expedited jobs at t: as the controller's lineup gives them.
func (d *Demo) standing(o demoOps, j demoJob, t float64) (level int, ranked string, expedite int) {
	level, ranked = config.LevelIn(d.cfg.Rules(o.runtimeRules(t)), d.waitingJob(j))
	if n := o.expediteAt(j.id(), t); n > 0 {
		return level, expeditedWords(n), n
	}
	return level, ranked, 0
}

// lineJob is a waiting job's standing and its place in the whole line.
type lineJob struct {
	decide.Demand
	place int
}

// lineOf is each waiting job's level and place in the whole line at t.
func (d *Demo) lineOf(o demoOps, w *demoWorld, t float64) map[string]lineJob {
	var ds []decide.Demand
	for _, j := range w.demand {
		dd := decide.Demand{ID: j.id(), Class: j.class, FirstSeen: demoEpoch.Add(time.Duration(j.firstSeen * float64(time.Second))), Name: j.spec.k().job}
		dd.Level, dd.Ranked, dd.Expedite = d.standing(o, j, t)
		ds = append(ds, dd)
	}
	out := map[string]lineJob{}
	for i, dd := range decide.Line(ds, nil, d.cfg.PriorityHeadStart) {
		out[dd.ID] = lineJob{dd, i + 1}
	}
	return out
}

// level is the word /api/state and /api/priority give a job's level.
func (lj lineJob) level() string {
	return levelWord(decide.Lined{Level: lj.Level, Expedite: lj.Expedite})
}

func (j demoJob) githubID() int64 { return 61_000_000_000 + int64(j.spec.id)*7 }

func (j demoJob) url() string {
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", demoRepo, j.spec.runID, j.githubID())
}

// invalidate forgets what was worked out for t and after: an op made at t changes it.
func (d *Demo) invalidate(t float64) {
	i := slices.IndexFunc(d.checks, func(c *demoWorld) bool { return c.t >= t })
	if i >= 0 {
		d.checks = d.checks[:i]
	}
	if d.w != nil && d.w.t >= t {
		d.w = nil
	}
}

func (v demoView) mode() Mode {
	name, until := v.ops.modeAt(v.t)
	m := Mode{Name: name, By: v.ops.modeBy(v.t)}
	if !math.IsInf(until, 1) {
		u := v.ts(until)
		m.Until = &u
	}
	return m
}

func (v demoView) rule(r demoRuleEv) config.Rule {
	out := r.rule
	out.At = v.ts(r.from)
	if !math.IsInf(r.expires, 1) {
		out.Expires = v.ts(r.expires)
	}
	return out
}

// expedited is an expedited job as the API shows it.
func (v demoView) expedited(j demoJob, e demoExpedite) Expedited {
	kd := j.spec.k()
	return Expedited{Job: j.id(), GitHubJobID: j.githubID(), Name: kd.job, Repo: demoRepo, RunID: j.spec.runID, Class: j.class,
		Since: v.ts(e.since), By: e.by, URL: j.url()}
}

var keptRE = regexp.MustCompile(`(?:\(|, )([^ ,()]+) kept for a \w+ job of ([^ ,()]+)`)

// priority is the demo's GET /api/priority at t; /api/explain and
// /api/state read it too.
func (v demoView) priority() PriorityView {
	d := v.d
	pv := PriorityView{Mode: v.mode(), HeadStart: shortDur(d.cfg.PriorityHeadStart), File: cmpOr(d.cfg.Path, "pumasi.yaml"), Controller: true,
		Rules: []RuleView{}, Expedited: []Expedited{}, RecentlyExpedited: []Expedited{}, Line: []LineJob{}}
	var runtime []config.Rule
	for _, r := range v.ops.rulesAt(v.t) {
		runtime = append(runtime, v.rule(r))
	}
	for _, r := range d.cfg.Rules(runtime) {
		pv.Rules = append(pv.Rules, ruleView(r))
	}

	reason, keptFor := map[string]string{}, map[string]string{}
	for _, df := range v.w.defers {
		reason[df.Class] = df.Reason
		for _, m := range keptRE.FindAllStringSubmatch(df.Reason, -1) {
			if _, ok := keptFor[m[2]]; !ok {
				keptFor[m[2]] = m[1]
			}
		}
	}
	coming := map[string]string{}
	for _, r := range v.w.runners {
		if !r.busy() {
			coming[r.class] = "a runner is coming up for it on " + r.host
		}
	}
	line := d.lineOf(v.ops, v.w, v.t)
	byPlace := slices.Clone(v.w.demand)
	slices.SortStableFunc(byPlace, func(a, b demoJob) int { return line[a.id()].place - line[b.id()].place })
	for _, j := range byPlace {
		lj, kd := line[j.id()], j.spec.k()
		waited := secs(v.t - j.firstSeen)
		pv.Line = append(pv.Line, LineJob{Job: j.id(), GitHubJobID: j.githubID(), Name: kd.job, Repo: demoRepo, RunID: j.spec.runID, Workflow: kd.workflow,
			Class: j.class, Level: lj.level(), Position: lj.place, Waited: (time.Duration(waited) * time.Second).String(), WaitedSeconds: waited,
			Why: cmpOr(lj.Ranked, "by its age"), WaitingFor: cmpOr(keptFor[j.class], cmpOr(reason[j.class], coming[j.class]))})
	}

	for _, e := range v.ops.activeAt(v.t) {
		i := slices.IndexFunc(v.w.demand, func(j demoJob) bool { return j.id() == e.job })
		if i < 0 {
			continue // it has a runner now: below
		}
		j := v.w.demand[i]
		x := v.expedited(j, e)
		lj := line[j.id()]
		x.Position = lj.place
		for _, o := range v.w.demand {
			if lo := line[o.id()]; o.class == j.class && lo.Expedite == lj.Expedite && lo.place < lj.place {
				x.Ahead++
			}
		}
		pv.Expedited = append(pv.Expedited, x)
	}
	// Recently: the last that were taken back or got a runner, the newest first.
	for _, e := range slices.Backward(v.ops.expedites) {
		if e.since > v.t || len(pv.RecentlyExpedited) == 10 {
			continue
		}
		if slices.ContainsFunc(v.w.demand, func(j demoJob) bool { return j.id() == e.job }) && v.t < e.until {
			continue // still waits, expedited
		}
		var x Expedited
		for _, r := range v.w.runners {
			if r.job.id() == e.job && r.busy() {
				x = v.expedited(r.job, e)
				st := v.ts(r.started)
				after := secs(r.started - e.since)
				x.StartedAt, x.AfterSeconds, x.Runner, x.Ended = &st, &after, r.name, "started on "+r.name
			}
		}
		if x.Job == "" {
			x = Expedited{Job: e.job, Repo: demoRepo, Since: v.ts(e.since), By: e.by, Ended: "its job is over"}
		}
		if v.t >= e.until {
			x.Ended = "cleared by " + e.clearedBy
		}
		pv.RecentlyExpedited = append(pv.RecentlyExpedited, x)
	}
	return pv
}

var jobInURL = regexp.MustCompile(`/runs/(\d+)(?:/attempts/\d+)?(?:/job/(\d+))?`)

// findWaiting finds a waiting job as the controller's resolve does: by
// pumasi's id, GitHub's job id, the job's URL, or a run's URL with the job's
// name (which a run with one job waiting needs not).
func (d *Demo) findWaiting(w *demoWorld, ref, name string) (demoJob, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return demoJob{}, opsErr(http.StatusBadRequest, "job: pumasi's job id, GitHub's job id, the job's URL, or a run's URL with name")
	}
	if m := jobInURL.FindStringSubmatch(ref); m != nil && strings.HasPrefix(ref, "http") {
		if m[2] == "" {
			run, _ := strconv.ParseInt(m[1], 10, 64)
			var in []demoJob
			for _, j := range w.demand {
				if j.spec.runID == run && (name == "" || j.spec.k().job == name) {
					in = append(in, j)
				}
			}
			switch {
			case len(in) == 0:
				return demoJob{}, opsErr(http.StatusNotFound, "no job of run %d waits here: give the job's URL (…/actions/runs/%d/job/<id>) or name it", run, run)
			case len(in) > 1 && name == "":
				var names []string
				for _, j := range in {
					names = append(names, j.spec.k().job)
				}
				return demoJob{}, opsErr(http.StatusBadRequest, "run %d has %d jobs waiting, name one: %s", run, len(in), strings.Join(names, "; "))
			}
			return in[0], nil
		}
		ref = m[2]
	}
	gh, _ := strconv.ParseInt(ref, 10, 64)
	for _, j := range w.demand {
		if j.id() == ref || (gh > 0 && j.githubID() == gh) {
			return j, nil
		}
	}
	return demoJob{}, opsErr(http.StatusNotFound, "no job %q waits: give pumasi's job id (waitingJobs[].id in /api/state), GitHub's job id, or the job's URL", ref)
}

func (d *Demo) priorityRoutes(mux *http.ServeMux) {
	write := func(h func(r *http.Request, by string) (any, error)) http.HandlerFunc { return serveWrite(nil, h) }
	mux.HandleFunc("GET /api/priority", func(w http.ResponseWriter, r *http.Request) {
		t, now := d.when(r)
		writeJSON(w, d.view(t, now).priority())
	})
	mux.HandleFunc("POST /api/expedite", write(func(r *http.Request, by string) (any, error) {
		var body struct {
			Job  json.RawMessage `json:"job"`
			Name string          `json:"name"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		t, now := d.when(r)
		d.mu.Lock()
		j, err := d.findWaiting(d.at(t), rawRef(body.Job), body.Name)
		if err != nil {
			d.mu.Unlock()
			return nil, err
		}
		already := d.ops.expeditedAt(j.id(), t)
		if !already {
			d.ops.expedites = append(d.ops.expedites, demoExpedite{job: j.id(), by: by, since: t, until: math.Inf(1)})
			d.invalidate(t)
		}
		d.mu.Unlock()
		pv := d.view(t, now).priority()
		i := slices.IndexFunc(pv.Expedited, func(x Expedited) bool { return x.Job == j.id() })
		if i < 0 {
			return nil, opsErr(http.StatusConflict, "%s got a runner as it was expedited", j.id())
		}
		x := pv.Expedited[i]
		return ExpediteResult{OK: true, Expedited: x, Already: already, Note: aheadNote(x)}, nil
	}))
	mux.HandleFunc("DELETE /api/expedite", write(func(r *http.Request, by string) (any, error) {
		ref := r.URL.Query().Get("job")
		t, now := d.when(r)
		d.mu.Lock()
		defer d.mu.Unlock()
		world := d.at(t)
		id := strings.TrimSpace(ref)
		j, err := d.findWaiting(world, ref, "")
		if err == nil {
			id = j.id()
		}
		for i := range d.ops.expedites {
			if e := &d.ops.expedites[i]; e.job == id && e.since <= t && t < e.until {
				e.until, e.clearedBy = t, by
				d.invalidate(t)
				v := demoView{d: d, w: world, t: t, now: now}
				x := Expedited{Job: id, Repo: demoRepo, Since: v.ts(e.since), By: e.by}
				if err == nil {
					x = v.expedited(j, *e)
				}
				x.Ended = "cleared by " + by
				return map[string]any{"ok": true, "cleared": x}, nil
			}
		}
		return nil, opsErr(http.StatusNotFound, "%q is not an expedited job", ref)
	}))
	mux.HandleFunc("POST /api/mode", write(func(r *http.Request, by string) (any, error) {
		var body struct {
			Name string `json:"name"`
			For  string `json:"for"`
		}
		if err := decodeBody(r, &body); err != nil {
			return nil, err
		}
		dur, err := parseFor(body.For)
		if err != nil {
			return nil, err
		}
		lasts, err := modeFor(body.Name, dur)
		if err != nil {
			return nil, err
		}
		t, now := d.when(r)
		until := math.Inf(1)
		if lasts > 0 {
			until = t + lasts.Seconds()
		}
		d.mu.Lock()
		d.ops.modes = append(d.ops.modes, demoModeEv{name: body.Name, by: by, from: t, until: until})
		d.invalidate(t)
		d.mu.Unlock()
		m := d.view(t, now).mode()
		m.At = &now
		return m, nil
	}))
	addRule := func(r *http.Request, by, replace string) (any, error) {
		pr, dur, err := ruleBody(r)
		if err != nil {
			return nil, err
		}
		if err := checkRule(pr, dur); err != nil {
			return nil, err
		}
		t, now := d.when(r)
		d.mu.Lock()
		defer d.mu.Unlock()
		// The rules are tried in the order they were added, as the ledger
		// keeps them; a changed rule keeps its place.
		at, id := len(d.ops.rules), replace
		if replace != "" {
			if strings.HasPrefix(replace, "file-") {
				return nil, fileRuleRefused(replace, cmpOr(d.cfg.Path, "pumasi.yaml"))
			}
			i := slices.IndexFunc(d.ops.rules, func(ru demoRuleEv) bool { return ru.rule.ID == replace && ru.from <= t && t < ru.until })
			if i < 0 {
				return nil, opsErr(http.StatusNotFound, "no runtime rule %s in force", replace)
			}
			d.ops.rules[i].until, at = t, i+1
		} else {
			d.ops.nextRule++
			id = fmt.Sprintf("r-%d", d.ops.nextRule)
		}
		expires := math.Inf(1)
		if dur > 0 {
			expires = t + dur.Seconds()
		}
		ev := demoRuleEv{rule: config.Rule{PriorityRule: pr, ID: id, Runtime: true, By: by}, from: t, until: expires, expires: expires}
		d.ops.rules = slices.Insert(d.ops.rules, at, ev)
		d.invalidate(t)
		return ruleView(demoView{d: d, t: t, now: now}.rule(ev)), nil
	}
	mux.HandleFunc("POST /api/priority/rules", write(func(r *http.Request, by string) (any, error) {
		return addRule(r, by, "")
	}))
	mux.HandleFunc("PUT /api/priority/rules/{id}", write(func(r *http.Request, by string) (any, error) {
		return addRule(r, by, r.PathValue("id"))
	}))
	mux.HandleFunc("DELETE /api/priority/rules/{id}", write(func(r *http.Request, by string) (any, error) {
		id := r.PathValue("id")
		if strings.HasPrefix(id, "file-") {
			return nil, fileRuleRefused(id, cmpOr(d.cfg.Path, "pumasi.yaml"))
		}
		t, _ := d.when(r)
		d.mu.Lock()
		defer d.mu.Unlock()
		for i := range d.ops.rules {
			if ru := &d.ops.rules[i]; ru.rule.ID == id && ru.from <= t && t < ru.until {
				ru.until = t
				d.invalidate(t)
				return map[string]any{"ok": true, "removed": id}, nil
			}
		}
		return nil, opsErr(http.StatusNotFound, "no runtime rule %s in force", id)
	}))
}
