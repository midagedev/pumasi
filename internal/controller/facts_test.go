package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ghauth"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

// finishJob runs a job on a new runner from start to end in the ledger.
func finishJob(t *testing.T, l *ledger.Ledger, runner string, run int64, name, ref string, from, to time.Time) {
	t.Helper()
	j := ledger.Job{ID: "id-" + runner, Class: "c", Repo: "o/r", Workflow: ref, Name: name, RunID: run, Event: "push"}
	if err := l.UpsertDemand(j, from.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := l.InsertRunner(runner, "c", "linux/docker", "h1", from.Add(-20*time.Second), size.Estimate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.JobStarted(runner, j, from); err != nil {
		t.Fatal(err)
	}
	if err := l.JobCompleted(runner, j, "succeeded", to); err != nil {
		t.Fatal(err)
	}
}

// GitHub is asked at most once per finished job: the jobs of one run share
// the call, a failed call is not repeated, and what came back is kept.
func TestRunFactsOneCallPerJobAtMost(t *testing.T) {
	c, now, _ := testBook(t)
	t0 := *now
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/o/r/actions/runs/1/jobs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
				{"id": 11, "run_attempt": 1, "name": "build", "runner_name": "r-build", "head_sha": "aaa111", "head_branch": "main", "steps": []map[string]any{
					{"number": 1, "name": "Set up job", "conclusion": "success", "started_at": t0.Add(-100 * time.Second), "completed_at": t0.Add(-98 * time.Second)},
					{"number": 2, "name": "Compile", "conclusion": "success", "started_at": t0.Add(-98 * time.Second), "completed_at": t0.Add(-10 * time.Second)},
					{"number": 3, "name": "Upload", "conclusion": "skipped", "started_at": "0001-01-01T00:00:00Z", "completed_at": "0001-01-01T00:00:00Z"},
				}},
				{"id": 12, "run_attempt": 1, "name": "test", "runner_name": "r-test", "head_sha": "aaa111", "head_branch": "main"},
				{"id": 13, "run_attempt": 1, "name": "lint", "runner_name": "someone-elses-runner", "head_sha": "aaa111"},
			}})
		case "/repos/o/r/actions/runs/2/jobs":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()
	c.auth = ghauth.Auth{Token: "tok"}

	ref := "o/r/.github/workflows/ci.yml@refs/heads/main"
	finishJob(t, c.ledger, "r-build", 1, "build", ref, t0.Add(-100*time.Second), t0.Add(-5*time.Second))
	finishJob(t, c.ledger, "r-test", 1, "test", ref, t0.Add(-90*time.Second), t0.Add(-4*time.Second))
	finishJob(t, c.ledger, "r-gone", 1, "docs", ref, t0.Add(-90*time.Second), t0.Add(-4*time.Second)) // not in GitHub's list
	finishJob(t, c.ledger, "r-fail", 2, "build", ref, t0.Add(-90*time.Second), t0.Add(-3*time.Second))

	c.askFacts(context.Background()) // too soon: GitHub has not filled in the last step
	if len(calls) != 0 {
		t.Fatalf("asked too soon: %v", calls)
	}
	*now = t0.Add(time.Minute)
	for range 3 {
		c.askFacts(context.Background())
		*now = now.Add(time.Minute)
	}
	jobs, total := 4, 0
	for _, n := range calls {
		total += n
	}
	if calls["/repos/o/r/actions/runs/1/jobs"] != 1 || calls["/repos/o/r/actions/runs/2/jobs"] != 1 || total > jobs {
		t.Fatalf("calls %v for %d jobs", calls, jobs)
	}
	runs, err := c.ledger.FinishedRuns(t0.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ledger.JobRun{}
	for _, r := range runs {
		by[r.Runner] = r
	}
	b := by["r-build"]
	if b.SHA != "aaa111" || b.Branch != "main" || b.GHJobID != 11 || b.Attempt != 1 || b.Event != "push" || len(b.Steps) != 3 ||
		b.Steps[1].Sec != 88 || b.Steps[2].Sec != 0 || b.Steps[2].Conclusion != "skipped" {
		t.Fatalf("build %+v", b)
	}
	if by["r-test"].SHA != "aaa111" || by["r-gone"].SHA != "" || by["r-fail"].SHA != "" {
		t.Fatalf("test %+v gone %+v fail %+v", by["r-test"], by["r-gone"], by["r-fail"])
	}
	why := map[string]string{}
	rows, _ := c.ledger.FactsStatus()
	for _, r := range rows {
		why[r[0]] = r[1]
	}
	if why["r-build"] != "ok" || why["r-gone"] != "not among the run's first 100 jobs" || !strings.Contains(why["r-fail"], "500") {
		t.Fatalf("facts %v", why)
	}
}

// A job is contended while others saturate its host, not while it fills the
// host alone; time between rounds that is not known is not counted.
func TestContention(t *testing.T) {
	c, now, _ := testBook(t)
	c.cfg.EvaluateEvery, c.cfg.PSICPUThreshold, c.cfg.PSIMemThreshold = 10*time.Second, 40, 10
	l := c.ledger
	for _, n := range []string{"alone", "a", "b"} {
		if err := l.InsertRunner(n, "c", "linux/docker", "h", *now, size.Estimate{}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.JobStarted(n, ledger.Job{ID: n, Class: "c", Repo: "o/r", Name: n, RunID: 1}, *now); err != nil {
			t.Fatal(err)
		}
	}
	h1 := &host{cfg: config.Host{Name: "h1"}}
	h2 := &host{cfg: config.Host{Name: "h2"}}
	c.hosts = []*host{h1, h2}
	round := func(d time.Duration, s1, s2 *api.Snapshot) {
		*now = now.Add(d)
		h1.snap, h2.snap = s1, s2
		live, _ := l.LiveRunners()
		c.recordContention(*now, live)
	}
	// h1: one job using all 8 cores. h2: two jobs of 4 cores each on 8, and some memory pressure.
	alone := &api.Snapshot{CPUs: 8, NonRunnerCPU: 0.2, PSI: map[string]float64{"cpu": 60}, Runners: []api.Runner{{Name: "alone", CPUNow: 7.8}}}
	shared := &api.Snapshot{CPUs: 8, NonRunnerCPU: 0.3, PSI: map[string]float64{"cpu": 30, "memory": 2}, Runners: []api.Runner{{Name: "a", CPUNow: 4}, {Name: "b", CPUNow: 3.9}}}
	idle := &api.Snapshot{CPUs: 8, NonRunnerCPU: 0.3, Runners: []api.Runner{{Name: "a", CPUNow: 2}, {Name: "b", CPUNow: 1}}}
	round(0, alone, shared)              // first round: nothing known before it
	round(10*time.Second, alone, shared) // 10s contended for a and b
	round(10*time.Second, alone, idle)   // not saturated
	round(5*time.Minute, alone, shared)  // a gap: not counted
	round(10*time.Second, alone, shared) // 10s more
	runs := map[string]float64{}
	rows, err := c.ledger.ContendedSeconds()
	if err != nil {
		t.Fatal(err)
	}
	for n, s := range rows {
		runs[n] = s
	}
	if runs["alone"] != 0 || runs["a"] != 20 || runs["b"] != 20 {
		t.Fatalf("contended seconds %v", runs)
	}
}
