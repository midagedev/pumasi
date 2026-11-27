package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ghauth"
	"github.com/midagedev/pumasi/internal/ledger"
)

// fakeREST is GitHub's REST API for jobs and runs.
type fakeREST struct {
	mu   sync.Mutex
	jobs map[int64]ghJob
	runs map[int64]string // run id -> workflow path
}

func (f *fakeREST) set(j ghJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.ID] = j
}

func (f *fakeREST) serve(t *testing.T, c *Controller) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var id, run int64
		switch {
		case r.Header.Get("Authorization") != "Bearer tok":
			http.Error(w, "no", http.StatusUnauthorized)
		case scan(r.URL.Path, "/repos/o/r/actions/jobs/%d", &id):
			if j, ok := f.jobs[id]; ok {
				_ = json.NewEncoder(w).Encode(j)
				return
			}
			http.NotFound(w, r)
		case scan(r.URL.Path, "/repos/o/r/actions/runs/%d/jobs", &run):
			var page struct {
				Jobs []ghJob `json:"jobs"`
			}
			for _, j := range f.jobs {
				if j.RunID == run {
					page.Jobs = append(page.Jobs, j)
				}
			}
			_ = json.NewEncoder(w).Encode(page)
		case scan(r.URL.Path, "/repos/o/r/actions/runs/%d", &run):
			_ = json.NewEncoder(w).Encode(map[string]string{"path": f.runs[run], "head_branch": "main", "event": "push", "status": "queued"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = old })
	c.auth = ghauth.Auth{Token: "tok"}
}

func scan(path, format string, v *int64) bool {
	n, err := fmt.Sscanf(path, format, v)
	return n == 1 && err == nil && fmt.Sprintf(format, *v) == path
}

// opsRig is the rig with two classes that each fit the host once (3 of its
// 4 cpus), and an overflow class whose jobs come from GitHub's queue.
func opsRig(t *testing.T) (*rig, *fakeREST) {
	r := newRig(t)
	r.cfg.URL = "https://github.com/o/r"
	r.ag.Sample(context.Background())
	os := r.ag.Snapshot().OS // the rig's agent says the OS the tests run on
	r.cfg.Classes = []config.Class{
		{Name: "small", OS: os, Driver: "docker", CPUs: 3, Memory: 1 << 30},
		{Name: "other", OS: os, Driver: "docker", CPUs: 3, Memory: 1 << 30},
		{Name: "over", OS: os, Driver: "docker", CPUs: 3, Memory: 1 << 30, Overflow: true, Labels: []string{"self-hosted", "ovf"}},
	}
	r.classes = nil
	for _, cc := range r.cfg.Classes {
		r.classes = append(r.classes, &class{cfg: cc, setID: 1, retaken: make(chan struct{}, 1)})
	}
	r.cfg.PriorityHeadStart = 30 * time.Minute
	f := &fakeREST{jobs: map[int64]ghJob{}, runs: map[int64]string{9: ".github/workflows/ci.yml"}}
	f.serve(t, r.Controller)
	return r, f
}

func (r *rig) waits(id, class, name string, run int64, ago time.Duration) {
	r.t.Helper()
	j := ledger.Job{ID: id, Class: class, Repo: "o/r", Workflow: "o/r/.github/workflows/ci.yml@refs/heads/main", Name: name, RunID: run}
	if err := r.ledger.UpsertDemand(j, r.now.Add(-ago)); err != nil {
		r.t.Fatal(err)
	}
}

// call sends one request to the dashboard's API from this machine.
func (r *rig) call(method, path, body string, head ...string) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:7420"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(head); i += 2 {
		req.Header.Set(head[i], head[i+1])
	}
	w := httptest.NewRecorder()
	r.webHandler().ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// An expedited job goes first, with the jobs ahead of it in its class, and
// the ledger keeps who expedited it and how long after it started.
func TestExpeditedJobIsFirstAndItsStartIsKept(t *testing.T) {
	r, _ := opsRig(t)
	r.waits("o1", "other", "lint", 5, 10*time.Minute)
	r.waits("s1", "small", "build", 6, 5*time.Minute)
	r.waits("s2", "small", "hotfix", 7, time.Minute)

	code, out := r.call("POST", "/api/expedite", `{"job":"s2"}`, "X-Pumasi-Client", "cli", "X-Pumasi-User", "alice")
	if code != 200 || out["ok"] != true || out["class"] != "small" || out["ahead"] != 1.0 || out["position"] != 2.0 {
		t.Fatalf("POST expedite: %d %v", code, out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "1 older job of class small goes first, with it") {
		t.Errorf("note %q", note)
	}
	active, _ := r.ledger.ActiveExpedites()
	if len(active) != 1 || active[0].By != "cli alice@127.0.0.1" || active[0].Job != "hotfix" || active[0].RunID != 7 {
		t.Fatalf("ledger %+v", active)
	}
	// Expediting it again changes nothing.
	if code, out := r.call("POST", "/api/expedite", `{"job":"s2"}`); code != 200 || out["already"] != true {
		t.Errorf("again: %d %v", code, out)
	}

	r.round(0)
	r.starts.Wait()
	line := r.liveStatus().Line
	if len(line) != 3 || line[0].ID != "s1" || line[0].Host != "h1" || line[1].ID != "s2" || line[2].ID != "o1" {
		t.Fatalf("line %+v", line)
	}
	if line[0].Ranked != "ahead of hotfix in its class, which is expedited" {
		t.Errorf("s1 stands there because %q", line[0].Ranked)
	}
	st, err := CollectState(r.cfg, r.ledger, r.liveStatus(), true, *r.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range st.WaitingJobs {
		switch {
		case j.ID == "s2" && (!j.Expedited || j.Level != "expedite" || j.Position != 2 || j.Ranked != "expedited"),
			// pulled along, not expedited by hand
			j.ID == "s1" && (j.Expedited || j.Level != "expedite" || j.Position != 1 || j.Ranked != line[0].Ranked),
			j.ID == "o1" && (j.Expedited || j.Level != "" || j.Position != 3):
			t.Errorf("waitingJobs: %+v", j)
		}
	}
	// Before a round has drawn the line (or while a runner is up for it), a
	// job stands at 0 and keeps its level.
	pv, err := CollectPriority(r.cfg, r.ledger, Status{}, true, *r.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range pv.Line {
		if want := map[string]string{"s1": "expedite", "s2": "expedite", "o1": "normal"}[j.Job]; j.Position != 0 || j.Level != want {
			t.Errorf("not in the last round's line: %+v, want level %s", j, want)
		}
	}

	// GitHub's JobStarted closes its demand on a runner: the expedite is over,
	// and the ledger keeps how long after it was expedited it started.
	*r.now = r.now.Add(3 * time.Minute)
	if _, err := r.ledger.JobStarted("small-h1-x", ledger.Job{ID: "s2", Class: "small", Repo: "o/r", Name: "hotfix", RunID: 7}, *r.now); err != nil {
		t.Fatal(err)
	}
	r.settleExpedites(context.Background(), *r.now)
	if a, _ := r.ledger.ActiveExpedites(); len(a) != 0 {
		t.Fatalf("still active: %+v", a)
	}
	rec, _ := r.ledger.RecentExpedites(5)
	if len(rec) != 1 || rec[0].Ended != "started" || rec[0].Runner != "small-h1-x" || rec[0].StartedAt.Sub(rec[0].At) != 3*time.Minute {
		t.Errorf("recent %+v", rec)
	}
}

// A job GitHub has given a class again under a new id is still the same
// expedited job: it is matched by repository, run and name.
func TestExpediteOutlivesAReassignment(t *testing.T) {
	r, _ := opsRig(t)
	r.waits("a1", "small", "build", 6, 6*time.Minute)
	if code, out := r.call("POST", "/api/expedite", `{"job":"https://github.com/o/r/actions/runs/6","name":"build"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_ = r.ledger.CloseDemand("a1", "unassigned-canceled", *r.now)
	r.waits("a2", "small", "build", 6, 0)
	r.settleExpedites(context.Background(), *r.now)
	s := r.decideSnapshot(*r.now, nil)
	if len(s.Demand) != 1 || s.Demand[0].ID != "a2" || s.Demand[0].Expedite != 1 || s.Demand[0].Pending {
		t.Fatalf("demand %+v", s.Demand)
	}
}

// An overflow class has not taken the job yet: GitHub holds it. pumasi learns
// its labels from GitHub, keeps room for it in the class they go to, and
// ends the expedite when GitHub says it started elsewhere.
func TestExpediteAJobGitHubStillHolds(t *testing.T) {
	r, f := opsRig(t)
	f.set(ghJob{ID: 77, RunID: 9, Name: "build", Status: "queued", Labels: []string{"self-hosted", "ovf"}, CreatedAt: r.now.Add(-2 * time.Minute)})
	code, out := r.call("POST", "/api/expedite", `{"job":"https://github.com/o/r/actions/runs/9/job/77"}`)
	if code != 200 || out["pending"] != true || out["class"] != "over" || out["githubJobId"] != 77.0 {
		t.Fatalf("%d %v", code, out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "GitHub still holds it") {
		t.Errorf("note %q", note)
	}
	a, _ := r.ledger.ActiveExpedites()
	if len(a) != 1 || a[0].Workflow != "o/r/.github/workflows/ci.yml@refs/heads/main" {
		t.Fatalf("expedite %+v", a)
	}
	s := r.decideSnapshot(*r.now, nil)
	if len(s.Demand) != 1 || !s.Demand[0].Pending || s.Demand[0].Class != "over" || s.Demand[0].Expedite != 1 {
		t.Fatalf("snapshot demand %+v", s.Demand)
	}
	// Still queued: nothing changes. Then it runs on a fixed runner.
	r.settleExpedites(context.Background(), *r.now)
	if a, _ := r.ledger.ActiveExpedites(); len(a) != 1 {
		t.Fatalf("ended while queued: %+v", a)
	}
	f.set(ghJob{ID: 77, RunID: 9, Name: "build", Status: "in_progress", Labels: []string{"self-hosted", "ovf"}, RunnerName: "fixed-1", StartedAt: r.now.Add(30 * time.Second)})
	*r.now = r.now.Add(30 * time.Second)
	r.settleExpedites(context.Background(), *r.now)
	if a, _ := r.ledger.ActiveExpedites(); len(a) != 1 {
		t.Fatalf("GitHub asked again within a minute: %+v", a)
	}
	*r.now = r.now.Add(time.Minute)
	r.settleExpedites(context.Background(), *r.now)
	rec, _ := r.ledger.RecentExpedites(1)
	if len(rec) != 1 || rec[0].Ended != "started elsewhere" || rec[0].Runner != "fixed-1" {
		t.Fatalf("recent %+v", rec)
	}
}

// A job that does not wait, or that nothing here takes, is refused with why.
func TestExpediteRefusals(t *testing.T) {
	r, f := opsRig(t)
	f.set(ghJob{ID: 5, RunID: 9, Name: "running", Status: "in_progress", RunnerName: "r9"})
	f.set(ghJob{ID: 6, RunID: 9, Name: "elsewhere", Status: "queued", Labels: []string{"self-hosted", "gpu"}})
	for _, tt := range []struct {
		body string
		code int
		says string
	}{
		{`{"job":"https://github.com/o/r/actions/runs/9/job/5"}`, 409, "it runs on r9"},
		{`{"job":6}`, 404, "no class of this controller takes that"},
		{`{"job":"nope"}`, 404, "no job \"nope\""},
		{`{"job":""}`, 400, "job:"},
		{`{"job":"https://github.com/o/r/actions/runs/1234"}`, 404, "no job of run 1234 waits here"},
		{`{"job":"https://github.com/o/r/actions/runs/1/job/2"}`, 404, "GitHub has no job 2 in o/r"},
	} {
		code, out := r.call("POST", "/api/expedite", tt.body)
		if msg, _ := out["error"].(string); code != tt.code || !strings.Contains(msg, tt.says) {
			t.Errorf("%s: %d %v, want %d %q", tt.body, code, out, tt.code, tt.says)
		}
	}
	if code, _ := r.call("DELETE", "/api/expedite?job=nope", ""); code != 404 {
		t.Errorf("clear an unknown job: %d", code)
	}
}

// Writes come from this machine, at an address; from elsewhere only with the
// token, and with the dashboard on every address the token is asked of all.
// A write is JSON: a form from another site cannot make one.
func TestWritesComeFromThisMachineOrWithTheToken(t *testing.T) {
	const token = "0123456789abcdef0123"
	for _, tt := range []struct {
		name, dashboard, token, remote, host, auth, ctype string
		code                                              int
	}{
		{"this machine", "127.0.0.1:7420", "", "127.0.0.1:5", "127.0.0.1:7420", "", "application/json", 200},
		{"this machine, ::1 and localhost", "[::1]:7420", "", "[::1]:5", "localhost:7420", "", "application/json", 200},
		{"another machine, no token in the config", "0.0.0.0:7420", "", "10.0.0.3:5", "ci:7420", "", "application/json", 403},
		{"another machine with the token", "0.0.0.0:7420", token, "10.0.0.3:5", "ci:7420", "Bearer " + token, "application/json", 200},
		{"another machine with a wrong token", "0.0.0.0:7420", token, "10.0.0.3:5", "ci:7420", "Bearer nope", "application/json", 401},
		{"dashboard on every address with a token: this machine shows it too", "0.0.0.0:7420", token, "127.0.0.1:5", "127.0.0.1:7420", "", "application/json", 401},
		{"dashboard on loopback with a token: this machine need not", "127.0.0.1:7420", token, "127.0.0.1:5", "127.0.0.1:7420", "", "application/json", 200},
		{"a name pointed at this machine (DNS rebinding)", "127.0.0.1:7420", "", "127.0.0.1:5", "evil.example:7420", "", "application/json", 403},
		{"a form", "127.0.0.1:7420", "", "127.0.0.1:5", "127.0.0.1:7420", "", "application/x-www-form-urlencoded", 415},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := opsRig(t)
			r.cfg.Dashboard, r.cfg.DashboardToken = tt.dashboard, tt.token
			req := httptest.NewRequest("POST", "http://"+tt.host+"/api/mode", strings.NewReader(`{"name":"normal"}`))
			req.RemoteAddr, req.Host = tt.remote, tt.host
			req.Header.Set("Content-Type", tt.ctype)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()
			r.webHandler().ServeHTTP(w, req)
			if w.Code != tt.code {
				t.Errorf("%d %s, want %d", w.Code, w.Body, tt.code)
			}
		})
	}
	// Reading stays open as before.
	r, _ := opsRig(t)
	req := httptest.NewRequest("GET", "http://ci:7420/api/priority", nil)
	req.RemoteAddr = "10.0.0.3:5"
	w := httptest.NewRecorder()
	r.webHandler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("GET from elsewhere: %d", w.Code)
	}
}

// high-only lasts as long as it was asked to, then every job gets room again
// without anyone's hand; normal ends it sooner.
func TestModeEndsByItself(t *testing.T) {
	r, _ := opsRig(t)
	code, out := r.call("POST", "/api/mode", `{"name":"high-only","for":"30m"}`)
	if code != 200 || out["name"] != "high-only" || out["until"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if m := CurrentMode(r.ledger, *r.now); m.Name != ModeHighOnly {
		t.Fatalf("mode %+v", m)
	}
	r.waits("n1", "small", "build", 6, time.Minute)
	if s := r.decideSnapshot(*r.now, nil); !strings.HasPrefix(s.HighOnly, "mode high-only until") {
		t.Errorf("snapshot HighOnly %q", s.HighOnly)
	}
	*r.now = r.now.Add(31 * time.Minute)
	if m := CurrentMode(r.ledger, *r.now); m.Name != ModeNormal {
		t.Errorf("after its time, read without a controller: %+v", m)
	}
	if s := r.decideSnapshot(*r.now, nil); s.HighOnly != "" {
		t.Errorf("after its time: %q", s.HighOnly)
	}
	if m, _ := storedMode(r.ledger); m.Name != ModeNormal || m.By != "its time was up" {
		t.Errorf("stored %+v", m)
	}
	for _, body := range []string{`{"name":"high-only","for":"30s"}`, `{"name":"high-only","for":"48h"}`, `{"name":"pause"}`} {
		if code, _ := r.call("POST", "/api/mode", body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, out := r.call("POST", "/api/mode", `{"name":"high-only"}`); code != 200 || out["until"] == nil {
		t.Errorf("no for: %d %v, want a default end", code, out)
	}
}

// A rule added at runtime goes before the file's; removed or out of time,
// the file's counts again. The file's rules are not changed through the API.
func TestRuntimeRulesGoBeforeTheFile(t *testing.T) {
	r, _ := opsRig(t)
	r.cfg.Priority = []config.PriorityRule{{Match: config.RuleMatch{Class: "small"}, Level: "low"}}
	job := config.WaitingJob{Class: "small", Repo: "o/r", WorkflowRef: "o/r/.github/workflows/deploy.yml@refs/heads/main", Name: "ship"}
	level := func() (int, string) { return config.LevelIn(r.rulesNow(*r.now), job) }
	if l, why := level(); l != -1 || why != "low by rule 1 (class: small)" {
		t.Fatalf("file only: %d %q", l, why)
	}
	code, out := r.call("POST", "/api/priority/rules", `{"match":{"workflow":"deploy*.yml"},"level":"high","for":"2h"}`)
	if code != 200 || out["id"] != "r-1" || out["source"] != "runtime" || out["yaml"] != `- {match: {workflow: "deploy*.yml"}, level: high}` {
		t.Fatalf("%d %v", code, out)
	}
	if l, why := level(); l != 1 || why != "high by runtime rule r-1 (workflow: deploy*.yml)" {
		t.Fatalf("with the runtime rule: %d %q", l, why)
	}
	if code, out := r.call("PUT", "/api/priority/rules/r-1", `{"match":{"workflow":"deploy*.yml"},"level":"normal"}`); code != 200 || out["level"] != "normal" {
		t.Errorf("PUT: %d %v", code, out)
	}
	if l, _ := level(); l != 0 {
		t.Errorf("after PUT: level %d", l)
	}
	if code, _ := r.call("DELETE", "/api/priority/rules/file-1", ""); code != 409 {
		t.Errorf("DELETE a file rule: %d, want 409", code)
	}
	if code, _ := r.call("DELETE", "/api/priority/rules/r-1", ""); code != 200 {
		t.Errorf("DELETE: %d", code)
	}
	if l, _ := level(); l != -1 {
		t.Errorf("removed: level %d", l)
	}
	if code, _ := r.call("POST", "/api/priority/rules", `{"match":{"workflow":"deploy*.yml"},"level":"high","for":"1h"}`); code != 200 {
		t.Fatal(code)
	}
	*r.now = r.now.Add(61 * time.Minute)
	if l, _ := level(); l != -1 {
		t.Errorf("out of time: level %d", l)
	}
	if code, out := r.call("POST", "/api/priority/rules", `{"match":{},"level":"high"}`); code != 400 {
		t.Errorf("a rule that matches everything: %d %v", code, out)
	}

	// GET /api/priority: the contract the dashboard reads.
	code, out = r.call("GET", "/api/priority", "")
	mode, _ := out["mode"].(map[string]any)
	rules, _ := out["rules"].([]any)
	if code != 200 || mode["name"] != "normal" || mode["until"] != nil || out["headStart"] != "30m" || len(rules) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if fr := rules[0].(map[string]any); fr["id"] != "file-1" || fr["source"] != "file" || fr["expires"] != nil {
		t.Errorf("file rule %v", fr)
	}
}

// An overflow class keeps the expedited job it holds although it has no place
// for it yet: room is being kept for it here, so it is not given back.
func TestOverflowClassKeepsItsExpeditedJob(t *testing.T) {
	c := overflowController(config.Class{Name: "over", Overflow: true})
	s := decide.Snapshot{Demand: []decide.Demand{{ID: "a", Class: "over"}, {ID: "b", Class: "over"}}}
	plain := decide.Plan{Room: map[string]int{"over": 0}, Defer: []decide.Defer{{Class: "over", Count: 2}}}
	c.setTakes(s, plain)
	if got := c.classes[0].take.Load(); got != 0 {
		t.Fatalf("take %d without an expedite, want 0", got)
	}
	plain.Line = []decide.Lined{{ID: "a", Class: "over", Expedite: 1, Reason: "no host"}, {ID: "b", Class: "over", Reason: "behind"}}
	c.setTakes(s, plain)
	if got := c.classes[0].take.Load(); got != 1 {
		t.Errorf("take %d, want 1: the expedited job is kept, the other given back", got)
	}
}
