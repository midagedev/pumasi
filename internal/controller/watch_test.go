package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

// A crossed limit is an incident with a notice; when it is not crossed any
// more, the incident resolves with one.
func TestWatchLimitOpensAndResolves(t *testing.T) {
	c, now, sent := testBook(t)
	c.cfg.Watch = config.Watch{Limits: []config.Limit{{Match: config.RuleMatch{Job: "build"}, MaxJobTime: config.Span(5 * time.Minute)}}}
	put := func(sec float64, n int) {
		for range n {
			*now = now.Add(10 * time.Minute)
			if err := c.ledger.PutRun(ledger.JobRun{Runner: "r" + now.Format("150405"), Class: "c", Host: "h", Repo: "o/r", Workflow: ".github/workflows/ci.yml",
				Job: "build", RunID: now.Unix(), Result: "succeeded", StartedAt: now.Add(-time.Duration(sec) * time.Second), FinishedAt: *now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(200, 5)
	c.checkAlarms(*now)
	put(400, 3)
	c.checkAlarms(*now)
	c.checkAlarms(now.Add(time.Minute)) // still: no second notice
	if open, _ := c.ledger.OpenIncidents(); len(open) != 1 || open[0].Kind != "ci-too-slow" {
		t.Fatalf("open %+v", open)
	}
	put(200, 3)
	*now = now.Add(10 * time.Minute) // more than flapWindow after it opened
	c.checkAlarms(*now)
	if open, _ := c.ledger.OpenIncidents(); len(open) != 0 {
		t.Fatalf("still open %+v", open)
	}
	got := sent()
	if len(got) != 2 || !strings.Contains(got[0], `"state":"problem"`) || !strings.Contains(got[0], "ci.yml / build takes 6m40s (median of its last 5 runs), over its 5m00s limit") ||
		!strings.Contains(got[1], `"state":"resolved"`) || !strings.Contains(got[1], "ci.yml / build: ci too slow resolved after") {
		t.Fatalf("notices:\n%s", strings.Join(got, "\n"))
	}
	// A limit taken out of the config: what it opened resolves.
	put(400, 3)
	c.checkAlarms(*now)
	c.cfg.Watch = config.Watch{}
	c.checkAlarms(now.Add(time.Minute))
	if open, _ := c.ledger.OpenIncidents(); len(open) != 0 {
		t.Fatalf("limit gone, still open %+v", open)
	}
}

// A regression is announced once, with its commit and its step.
func TestRegressionAnnouncedOnce(t *testing.T) {
	c, now, sent := testBook(t)
	c.cfg.URL = "https://github.com/example/app"
	c.cfg.Watch.Regressions = nil // the default, 20%
	h := synth.Demo(*now)
	if err := c.ledger.PutRuns(h.Runs...); err != nil {
		t.Fatal(err)
	}
	c.announceRegressions(*now)
	c.announceRegressions(now.Add(20 * time.Minute))
	got := sent()
	// The demo's build-image took more memory six days ago; its slower test step
	// was eight days ago, too long ago to announce, and its faster typecheck
	// is good news, not a regression.
	var in synth.Injected
	for _, i := range h.Injected {
		if i.Job == "build-image" {
			in = i
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "pr-checks.yml / build-image: memory +3") || !strings.Contains(got[0], "commit "+in.SHA[:7]+" on main") ||
		!strings.Contains(got[0], "https://github.com/example/app/commit/"+in.SHA) || !strings.Contains(got[0], `"kind":"ci-regression"`) {
		t.Fatalf("notices (want build-image's, once):\n%s", strings.Join(got, "\n"))
	}
	if open, _ := c.ledger.OpenIncidents(); len(open) != 0 {
		t.Fatalf("a regression is a fact, not an open incident: %+v", open)
	}
}
