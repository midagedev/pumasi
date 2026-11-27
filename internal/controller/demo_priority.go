package controller

import (
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
)

// The demo's priority controls: expedite a waiting job, high-only mode, rules
// for a while. Only the demo serves them, so the dashboard can show the
// whole flow without a controller; they act through the real placement like
// everything else in the demo. What people do here is kept in memory as a
// log with the simulated second it happened, so the world can be worked out
// again with it in place; the seed does not replay it.

// levelExpedite is an expedited job's level: ahead of any rule's.
const levelExpedite = 9

type demoOps struct {
	expedites []demoExpedite
	modes     []demoModeEv
	rules     []demoRuleEv
	nextRule  int
}

type demoExpedite struct {
	job          string
	since, until float64 // until = when it was taken back (+Inf = not)
}

type demoModeEv struct {
	name        string
	from, until float64 // until: when high-only ends by itself (+Inf = not)
}

type demoRuleEv struct {
	id          string
	rule        config.PriorityRule
	from, until float64 // until: when it expires or was removed (+Inf = neither)
	expires     float64 // +Inf = never
}

func (o demoOps) clone() demoOps {
	return demoOps{expedites: slices.Clone(o.expedites), modes: slices.Clone(o.modes), rules: slices.Clone(o.rules), nextRule: o.nextRule}
}

func (o demoOps) expeditedAt(job string, t float64) bool {
	for _, e := range o.expedites {
		if e.job == job && e.since <= t && t < e.until {
			return true
		}
	}
	return false
}

func (o demoOps) modeAt(t float64) (name string, until float64) {
	name, until = "normal", math.Inf(1)
	for _, m := range o.modes {
		if m.from <= t {
			name, until = m.name, m.until
		}
	}
	if name == "high-only" && t >= until {
		return "normal", math.Inf(1)
	}
	return name, until
}

func (o demoOps) modeBy(t float64) string {
	by := "demo"
	for _, m := range o.modes {
		if m.from <= t {
			by = "dashboard"
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

// levelOf is a waiting job's level at t: expedited first, then the rules set
// from the dashboard (oldest first), then the config's.
func (d *Demo) levelOf(o demoOps, j demoJob, t float64) (int, string) {
	if o.expeditedAt(j.id(), t) {
		return levelExpedite, "expedited from the dashboard"
	}
	wj := d.waitingJob(j)
	for _, r := range o.rulesAt(t) {
		one := &config.Config{Priority: []config.PriorityRule{r.rule}}
		if l, why := one.LevelOf(wj); why != "" {
			return l, strings.Replace(why, "by rule 1", "by dashboard rule "+r.id, 1)
		}
	}
	return d.cfg.LevelOf(wj)
}

// levelAt is levelOf with the ops as they are; the caller holds d.mu.
func (d *Demo) levelAt(j demoJob, t float64) (int, string) { return d.levelOf(d.ops, j, t) }

// lineOf is each waiting job's level and place in the whole line at t.
func (d *Demo) lineOf(o demoOps, w *demoWorld, t float64) map[string]lineJob {
	var ds []decide.Demand
	for _, j := range w.demand {
		dd := decide.Demand{ID: j.id(), Class: j.class, FirstSeen: demoEpoch.Add(time.Duration(j.firstSeen * float64(time.Second))), Name: j.spec.kind}
		dd.Level, dd.Ranked = d.levelOf(o, j, t)
		ds = append(ds, dd)
	}
	out := map[string]lineJob{}
	for i, dd := range decide.Line(ds, nil, d.cfg.PriorityHeadStart) {
		out[dd.ID] = lineJob{dd, i + 1}
	}
	return out
}

func levelWord(l int) string {
	if l >= levelExpedite {
		return "expedite"
	}
	return levelName(l)
}

func (j demoJob) githubID() int64 { return 61_000_000_000 + int64(j.spec.id)*7 }

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

// The JSON of GET /api/priority.
type demoRuleJSON struct {
	ID      string            `json:"id"`
	Source  string            `json:"source"` // file | runtime
	Match   map[string]string `json:"match"`
	Level   string            `json:"level"`
	Expires *time.Time        `json:"expires"`
}

type demoLineJSON struct {
	Job         string `json:"job"`
	GithubJobID int64  `json:"githubJobId"`
	Name        string `json:"name"`
	Workflow    string `json:"workflow"`
	Class       string `json:"class"`
	Level       string `json:"level"` // expedite | high | normal | low
	Position    int    `json:"position"`
	Waited      string `json:"waited"`
	Why         string `json:"why"`
	WaitingFor  string `json:"waitingFor"`
}

type demoPriorityJSON struct {
	Mode      PriorityMode   `json:"mode"`
	HeadStart string         `json:"headStart"`
	Rules     []demoRuleJSON `json:"rules"`
	Expedited []ExpeditedJob `json:"expedited"`
	Line      []demoLineJSON `json:"line"`
}

func matchMap(r config.PriorityRule) map[string]string {
	m := map[string]string{}
	for k, v := range map[string]string{"class": r.Match.Class, "repo": r.Match.Repo, "workflow": r.Match.Workflow, "job": r.Match.Job,
		"ref": r.Match.Ref, "branch": r.Match.Branch, "event": r.Match.Event} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

func shortDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (v demoView) mode() PriorityMode {
	name, until := v.ops.modeAt(v.t)
	m := PriorityMode{Name: name, By: v.ops.modeBy(v.t)}
	if !math.IsInf(until, 1) {
		u := v.ts(until)
		m.Until = &u
	}
	return m
}

// priority is the demo's priority controls at t, for /api/explain.
func (v demoView) priority() PriorityView {
	pv := PriorityView{Mode: v.mode(), Expedited: []ExpeditedJob{}}
	line := v.d.lineOf(v.ops, v.w, v.t)
	for _, e := range v.ops.expedites {
		if !(e.since <= v.t && v.t < e.until) {
			continue
		}
		i := slices.IndexFunc(v.w.demand, func(j demoJob) bool { return j.id() == e.job })
		if i < 0 {
			continue // it has a runner now
		}
		j, place := v.w.demand[i], line[e.job].place
		pv.Expedited = append(pv.Expedited, ExpeditedJob{Job: e.job, GithubJobID: j.githubID(), Name: j.spec.kind, Class: j.class,
			Since: v.ts(e.since), By: "dashboard", Ahead: max(0, place-1), Position: place})
	}
	return pv
}

var keptRE = regexp.MustCompile(`(?:\(|, )([^ ,()]+) kept for a \w+ job of ([^ ,()]+)`)

func (v demoView) priorityJSON() demoPriorityJSON {
	pv := v.priority()
	out := demoPriorityJSON{Mode: pv.Mode, HeadStart: shortDur(v.d.cfg.PriorityHeadStart), Rules: []demoRuleJSON{}, Expedited: pv.Expedited, Line: []demoLineJSON{}}
	for i, r := range v.d.cfg.Priority {
		out.Rules = append(out.Rules, demoRuleJSON{ID: fmt.Sprintf("file-%d", i+1), Source: "file", Match: matchMap(r), Level: r.Level})
	}
	for _, r := range v.ops.rulesAt(v.t) {
		rj := demoRuleJSON{ID: r.id, Source: "runtime", Match: matchMap(r.rule), Level: r.rule.Level}
		if !math.IsInf(r.expires, 1) {
			e := v.ts(r.expires)
			rj.Expires = &e
		}
		out.Rules = append(out.Rules, rj)
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
		if r.started == 0 {
			coming[r.class] = "a runner is coming up for it on " + r.host
		}
	}
	line := v.d.lineOf(v.ops, v.w, v.t)
	byPlace := slices.Clone(v.w.demand)
	slices.SortStableFunc(byPlace, func(a, b demoJob) int { return line[a.id()].place - line[b.id()].place })
	for _, j := range byPlace {
		lj := line[j.id()]
		why := lj.Ranked
		if why == "" {
			why = "by its age"
		}
		waitingFor := keptFor[j.class]
		if waitingFor == "" {
			waitingFor = cmpOrStr(reason[j.class], coming[j.class])
		}
		out.Line = append(out.Line, demoLineJSON{Job: j.id(), GithubJobID: j.githubID(), Name: j.spec.kind, Workflow: j.spec.k().workflow, Class: j.class,
			Level: cmpOrStr(levelWord(lj.Level), "normal"), Position: lj.place, Waited: shortDur(time.Duration(v.t-j.firstSeen) * time.Second), Why: why, WaitingFor: waitingFor})
	}
	return out
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// readJSON decodes a write's body; a write must say it is JSON, which a
// page on another site cannot send without asking first.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		jsonError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

var jobInURL = regexp.MustCompile(`/runs/(\d+)(?:/job/(\d+))?`)

// findWaiting finds a waiting job by its id, GitHub's job id, or a run or job URL.
func (d *Demo) findWaiting(w *demoWorld, ref string) (demoJob, bool) {
	ref = strings.TrimSpace(ref)
	if m := jobInURL.FindStringSubmatch(ref); m != nil {
		if m[2] != "" {
			ref = m[2]
		} else {
			run, _ := strconv.ParseInt(m[1], 10, 64)
			for _, j := range w.demand {
				if j.spec.runID == run {
					return j, true
				}
			}
			return demoJob{}, false
		}
	}
	gh, _ := strconv.ParseInt(ref, 10, 64)
	for _, j := range w.demand {
		if j.id() == ref || (gh > 0 && j.githubID() == gh) {
			return j, true
		}
	}
	return demoJob{}, false
}

// jobRef takes the "job" of a request body: a string, or GitHub's job id as a number.
type jobRef struct {
	Job json.RawMessage `json:"job"`
}

func (j jobRef) String() string {
	var s string
	if json.Unmarshal(j.Job, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(j.Job))
}

func (d *Demo) priorityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/priority", func(w http.ResponseWriter, r *http.Request) {
		t, now := d.when(r)
		writeJSON(w, d.view(t, now).priorityJSON())
	})
	mux.HandleFunc("POST /api/expedite", func(w http.ResponseWriter, r *http.Request) {
		var body jobRef
		if !readJSON(w, r, &body) {
			return
		}
		t, _ := d.when(r)
		d.mu.Lock()
		world := d.at(t)
		j, ok := d.findWaiting(world, body.String())
		if !ok {
			d.mu.Unlock()
			jsonError(w, http.StatusNotFound, "no such waiting job")
			return
		}
		if !d.ops.expeditedAt(j.id(), t) {
			d.ops.expedites = append(d.ops.expedites, demoExpedite{job: j.id(), since: t, until: math.Inf(1)})
		}
		place := d.lineOf(d.ops, world, t)[j.id()].place
		before := 0
		for _, o := range world.demand {
			if o.class == j.class && o.firstSeen < j.firstSeen {
				before++
			}
		}
		d.invalidate(t)
		d.mu.Unlock()
		note := "it gets the next room that fits it; nothing running is stopped for it"
		if before > 0 {
			note = fmt.Sprintf("%d older %s of %s go first with it: GitHub gives a class's jobs to its runners oldest first; nothing running is stopped for it",
				before, plural(before, "job", "jobs"), j.class)
		}
		writeJSON(w, map[string]any{"ok": true, "job": j.id(), "githubJobId": j.githubID(), "class": j.class, "position": place, "ahead": max(0, place-1), "note": note})
	})
	mux.HandleFunc("DELETE /api/expedite", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("job")
		t, _ := d.when(r)
		d.mu.Lock()
		defer d.mu.Unlock()
		world := d.at(t)
		id := ref
		if j, ok := d.findWaiting(world, ref); ok {
			id = j.id()
		}
		for i := range d.ops.expedites {
			if e := &d.ops.expedites[i]; e.job == id && e.since <= t && t < e.until {
				e.until = t
				d.invalidate(t)
				writeJSON(w, map[string]any{"ok": true, "job": id})
				return
			}
		}
		jsonError(w, http.StatusNotFound, "no such expedited job")
	})
	mux.HandleFunc("POST /api/mode", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			For  string `json:"for"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Name != "normal" && body.Name != "high-only" {
			jsonError(w, http.StatusBadRequest, `name must be "normal" or "high-only"`)
			return
		}
		until := math.Inf(1)
		t, now := d.when(r)
		if body.For != "" && body.Name == "high-only" {
			dur, err := time.ParseDuration(body.For)
			if err != nil || dur <= 0 {
				jsonError(w, http.StatusBadRequest, "for must be a duration like 30m")
				return
			}
			until = t + dur.Seconds()
		}
		d.mu.Lock()
		d.ops.modes = append(d.ops.modes, demoModeEv{name: body.Name, from: t, until: until})
		d.invalidate(t)
		d.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "mode": d.view(t, now).mode()})
	})
	mux.HandleFunc("POST /api/priority/rules", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Match map[string]string `json:"match"`
			Level string            `json:"level"`
			For   string            `json:"for"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		var rule config.PriorityRule
		rule.Level = body.Level
		for k, v := range body.Match {
			if _, err := path.Match(v, ""); err != nil {
				jsonError(w, http.StatusBadRequest, fmt.Sprintf("bad pattern %q", v))
				return
			}
			switch k {
			case "class":
				rule.Match.Class = v
			case "repo":
				rule.Match.Repo = v
			case "workflow":
				rule.Match.Workflow = v
			case "job":
				rule.Match.Job = v
			case "ref":
				rule.Match.Ref = v
			case "branch":
				rule.Match.Branch = v
			case "event":
				rule.Match.Event = v
			default:
				jsonError(w, http.StatusBadRequest, "match keys: class, repo, workflow, job, ref, branch, event")
				return
			}
		}
		if rule.Level != "high" && rule.Level != "normal" && rule.Level != "low" {
			jsonError(w, http.StatusBadRequest, "level must be high, normal or low")
			return
		}
		if rule.String() == "" {
			jsonError(w, http.StatusBadRequest, "match needs at least one of class, repo, workflow, job, ref, branch, event")
			return
		}
		t, now := d.when(r)
		expires := math.Inf(1)
		if body.For != "" {
			dur, err := time.ParseDuration(body.For)
			if err != nil || dur <= 0 {
				jsonError(w, http.StatusBadRequest, "for must be a duration like 2h")
				return
			}
			expires = t + dur.Seconds()
		}
		d.mu.Lock()
		d.ops.nextRule++
		ev := demoRuleEv{id: fmt.Sprintf("r-%d", d.ops.nextRule), rule: rule, from: t, until: expires, expires: expires}
		d.ops.rules = append(d.ops.rules, ev)
		d.invalidate(t)
		d.mu.Unlock()
		rj := demoRuleJSON{ID: ev.id, Source: "runtime", Match: matchMap(rule), Level: rule.Level}
		if !math.IsInf(expires, 1) {
			e := now.Add(time.Duration((expires - t) * float64(time.Second))).UTC()
			rj.Expires = &e
		}
		writeJSON(w, rj)
	})
	mux.HandleFunc("DELETE /api/priority/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.HasPrefix(id, "file-") {
			jsonError(w, http.StatusForbidden, "rule from pumasi.yaml: edit the file")
			return
		}
		t, _ := d.when(r)
		d.mu.Lock()
		defer d.mu.Unlock()
		for i := range d.ops.rules {
			if ru := &d.ops.rules[i]; ru.id == id && ru.from <= t && t < ru.until {
				ru.until = t
				d.invalidate(t)
				writeJSON(w, map[string]any{"ok": true, "id": id})
				return
			}
		}
		jsonError(w, http.StatusNotFound, "no such rule")
	})
}
