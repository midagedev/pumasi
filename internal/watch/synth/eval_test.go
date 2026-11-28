package synth

import (
	"strings"
	"testing"
)

// The regression search keeps what it was measured to do: the full numbers
// come from `go run ./internal/watch/cmd/watcheval` (100 histories a
// scenario); this guard runs fewer and holds looser bars.
func TestRegressionSearchHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluation")
	}
	ec := EvalConfig{Histories: 20}
	noise, per := Eval(ec)
	var b strings.Builder
	WriteReport(&b, ec, noise, per)
	t.Log("\n" + b.String())
	if noise.FalsePerWeek() > 0.5 {
		t.Errorf("false alarms %.2f a week with nothing injected", noise.FalsePerWeek())
	}
	for _, r := range per {
		if strings.HasPrefix(r.Scenario, "creep") {
			continue // no single commit to find: the slope alert's case
		}
		if r.Recall() < 0.8 || r.Attribution() < 0.7 || r.FalsePerWeek() > 0.5 {
			t.Errorf("%s: recall %.2f, commit %.2f, false %.2f a week", r.Scenario, r.Recall(), r.Attribution(), r.FalsePerWeek())
		}
	}
}

// why-slow keeps what it was measured to do (`watcheval -why` for the full
// numbers): the pull request behind a change is answered, an unrelated one
// is rarely called slower and never shown as a change's cause.
func TestWhySlowHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluation")
	}
	ec := EvalConfig{Histories: 10}
	noise, per := EvalWhy(ec)
	var b strings.Builder
	WriteWhyReport(&b, ec, noise, per)
	t.Log("\n" + b.String())
	if frac(noise.Slower, noise.PRs) > 0.03 || frac(noise.Maybe, noise.PRs) > 0.2 || noise.Own > 0 {
		t.Errorf("nothing injected: slower %d, maybe %d, own %d of %d", noise.Slower, noise.Maybe, noise.Own, noise.PRs)
	}
	for _, r := range per {
		if frac(r.Answered, r.Histories) < 0.6 || frac(r.NeighbourSure, r.Neighbours) > 0.02 {
			t.Errorf("%s: answered %d of %d, neighbours named %d of %d", r.Scenario, r.Answered, r.Histories, r.NeighbourSure, r.Neighbours)
		}
	}
}

// The forecast's replay of the last week sees about the share of jobs that
// wait that a queue of the same history does (`watcheval -forecast 0.08`
// for the dates).
func TestForecastReplayHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluation")
	}
	for _, c := range EvalForecast(ForecastEval{Histories: 8}) {
		if d := c.WaitOdds - c.WaitShare; d < -0.15 || d > 0.15 {
			t.Errorf("waiting: the replay %.2f, the queue %.2f", c.WaitOdds, c.WaitShare)
		}
		if c.GrowthSeen < 0 || c.GrowthSeen > 0.2 {
			t.Errorf("growth %+.3f read from a team growing 8%% a week", c.GrowthSeen)
		}
	}
}
