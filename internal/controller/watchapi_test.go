package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
