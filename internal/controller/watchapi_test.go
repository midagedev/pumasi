package controller

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// The demo's trend names the commits its history was made with.
func TestDemoTrendNamesTheInjectedCommits(t *testing.T) {
	d := NewDemo(1)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	var rep watch.TrendReport
	if code := getJSON(t, srv.URL+"/api/trend?since=30d", &rep); code != 200 {
		t.Fatalf("status %d", code)
	}
	truth := map[string]synth.Injected{}
	for _, in := range d.history().Injected {
		truth[in.Job] = in
	}
	found := map[string]bool{}
	for _, c := range rep.Changes {
		in, ok := truth[c.Job]
		if !ok {
			t.Errorf("a change nobody made: %s %s %+.2f since %s", c.Job, c.Metric, c.Ratio, c.First.SHA)
			continue
		}
		if in.Creep > 0 {
			if !c.Gradual {
				t.Errorf("%s crept, but a commit is named: %s", c.Job, c.First.SHA)
			}
			continue
		}
		if c.First.SHA != in.SHA {
			t.Errorf("%s %s: since %s, made by %s", c.Job, c.Metric, c.First.SHA, in.SHA)
		}
		found[c.Job] = true
	}
	for job, in := range truth {
		if in.Creep == 0 && !found[job] {
			t.Errorf("%s (%s) not found", job, in.Label)
		}
	}
	// The creep is listed as such, and each job's CHANGE says what the
	// lists say of it.
	if len(rep.Creeping) != 1 || rep.Creeping[0].Job != "e2e (chromium)" || rep.Creeping[0].Ratio < 0.12 {
		t.Errorf("creeping %+v", rep.Creeping)
	}
	for _, s := range rep.Series {
		want := 1.0
		for _, c := range rep.Changes {
			if c.Metric == "time" && c.Job == s.Job && s.Job != "" {
				want *= 1 + c.Ratio
			}
		}
		for _, c := range rep.Creeping {
			if c.Job == s.Job {
				want *= 1 + c.Ratio
			}
		}
		if want != 1 && (s.ChangeFrom != "listed" || math.Abs(s.Change-(want-1)) > 1e-9) {
			t.Errorf("%s: change %+.3f (%s), the lists say %+.3f", s.Name(), s.Change, s.ChangeFrom, want-1)
		}
	}
	if len(rep.Series) < 8 || rep.Grain != "day" {
		t.Fatalf("series %d grain %s", len(rep.Series), rep.Grain)
	}
	if code := getJSON(t, srv.URL+"/api/trend?since=forever", &rep); code != http.StatusBadRequest {
		t.Fatalf("bad since: %d", code)
	}
	var one watch.TrendReport
	getJSON(t, srv.URL+"/api/trend?job=unit-tests&week=1", &one)
	if len(one.Series) != 2 || one.Series[1].Job != "unit-tests" || one.Grain != "week" {
		t.Fatalf("filtered: %+v", one.Series)
	}
}

// The controller's trend reads its ledger.
func TestTrendFromTheLedger(t *testing.T) {
	c, now, _ := testBook(t)
	h := synth.Generate(synth.Config{Seed: 5, Start: now.AddDate(0, 0, -20), Days: 21})
	var runs = h.Runs[:0]
	for _, r := range h.Runs {
		if r.FinishedAt.Before(*now) {
			runs = append(runs, r)
		}
	}
	if err := c.ledger.PutRuns(runs...); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(webMux(func(*http.Request) dashSource { return ctlSource{c} }))
	defer srv.Close()
	var rep watch.TrendReport
	if code := getJSON(t, srv.URL+"/api/trend?since=7d&job=lint", &rep); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(rep.Series) != 2 || rep.Series[1].All.Runs == 0 || rep.From.After(now.Add(-7*24*time.Hour)) {
		t.Fatalf("%+v", rep)
	}
}

// The demo answers why-slow about the pull request behind its slower test
// step with that change and its commit.
func TestDemoWhySlow(t *testing.T) {
	d := NewDemo(1)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	var in synth.Injected
	for _, i := range d.history().Injected {
		if i.Step != "" {
			in = i
		}
	}
	var rep watch.WhyReport
	if code := getJSON(t, srv.URL+"/api/why-slow?pr="+strconv.Itoa(in.PR), &rep); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(rep.Own) == 0 || rep.Own[0].Job != in.Job || rep.Own[0].First.SHA != in.SHA || !rep.OwnSure[0] {
		t.Fatalf("own %+v, want %s at %s", rep.Own, in.Job, in.SHA)
	}
	var steps []string
	for _, c := range rep.Own {
		for _, s := range c.Steps {
			steps = append(steps, s.Name)
		}
	}
	if len(steps) == 0 || steps[0] != in.Step {
		t.Fatalf("steps %v, want %q first", steps, in.Step)
	}
	for q, want := range map[string]int{"": 400, "pr=x": 400, "run=999999999": 404, "commit=" + in.SHA[:8]: 200} {
		if code := getJSON(t, srv.URL+"/api/why-slow?"+q, &rep); code != want {
			t.Errorf("%q: %d, want %d", q, code, want)
		}
	}
}

// The demo's forecast reads its growing history against its hosts' cores.
func TestDemoForecast(t *testing.T) {
	d := NewDemo(1)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	var rep watch.ForecastReport
	if code := getJSON(t, srv.URL+"/api/forecast", &rep); code != 200 {
		t.Fatalf("status %d", code)
	}
	// Each host's budget, else its cores: 32 + 8 + 10 + 16, the cores the
	// demo's history queued on.
	if DemoCores() != 66 || DemoCores() != synth.DemoFleetCores {
		t.Fatalf("demo cores %.0f, the history's %d", DemoCores(), synth.DemoFleetCores)
	}
	if !rep.Physical || !strings.Contains(rep.Words, "of the 66 physical cores") {
		t.Fatalf("the demo's cores are its hosts' physical ones: %s", rep.Words)
	}
	if rep.Cores != DemoCores() || rep.Fitted < 3 || rep.Growth <= 0 || rep.Load <= 0 || rep.FullIn <= 0 || rep.Words == "" {
		t.Fatalf("%+v", rep)
	}
	if code := getJSON(t, srv.URL+"/api/forecast?weeks=2", &rep); code != http.StatusBadRequest {
		t.Fatalf("weeks=2: %d", code)
	}
	rep = watch.ForecastReport{}
	if getJSON(t, srv.URL+"/api/forecast?cores=1000", &rep); rep.CoresFrom != "asked" || rep.Physical || rep.Load > 0.05 {
		t.Fatalf("cores=1000: %+v", rep)
	}
}

// The demo's CI history ends with its jobs as the demo dashboard runs them:
// for each job of the same name, its time, cores and memory are the middle
// of the dashboard's ranges, in the same workflow and class.
func TestDemoHistoryRunsTheDashboardsJobs(t *testing.T) {
	mid := func(v [2]float64) float64 { return (v[0] + v[1]) / 2 }
	n := 0
	for _, j := range synth.DemoJobs {
		k, ok := demoKinds[j.Name]
		if !ok {
			t.Errorf("%s: the dashboard has no job of that name", j.Name)
			continue
		}
		n++
		if j.Sec != mid(k.dur) || math.Abs(j.Cores-mid(k.cpu)) > 1e-9 || math.Abs(j.MemGB-mid(k.mem)) > 1e-9 || path.Base(j.Workflow) != k.workflow || j.Class != k.class {
			t.Errorf("%s: history %.0fs %.2f cores %.2fG %s %s, dashboard %.0fs %.2f cores %.2fG %s %s", j.Name, j.Sec, j.Cores, j.MemGB, path.Base(j.Workflow), j.Class,
				mid(k.dur), mid(k.cpu), mid(k.mem), k.workflow, k.class)
		}
	}
	if n < 5 {
		t.Fatalf("%d jobs in common", n)
	}
}
