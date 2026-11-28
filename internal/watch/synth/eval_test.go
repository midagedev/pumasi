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
