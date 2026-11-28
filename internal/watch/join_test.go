package watch

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Memory that fell twice, both steps named by one run, is one change in two
// steps, not memory twice on one line; a step the other way stays apart.
func TestStepsOfOneNumberAreOne(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	first := RunRef{Runner: "r1", At: at}
	c := func(m string, before, after float64, z float64) Change {
		return Change{Workflow: "ci.yml", Job: "e2e", Metric: m, Before: before, After: after, Ratio: after/before - 1, Z: z, First: first, Holds: true, RunsBefore: 10, RunsAfter: 10}
	}
	gb := float64(1 << 30)
	cs := joinSteps([]Change{c("time", 60, 20, -5), c("memory", 2*gb, gb, -4), c("memory", gb, 0.8*gb, -2), c("cpu", 12, 6, -3)})
	if len(cs) != 3 || cs[1].Metric != "memory" || cs[1].Parts != 2 || math.Abs(cs[1].Ratio+0.6) > 1e-9 || cs[1].Before != 2*gb || cs[1].After != 0.8*gb || cs[1].Z != -4 {
		t.Fatalf("%+v", cs)
	}
	line := ChangeLine(GroupChanges(cs)[0])
	if strings.Count(line, "memory") != 1 || !strings.Contains(line, "memory -60% (2.0G -> 0.8G, in 2 steps)") {
		t.Fatal(line)
	}
	if up := joinSteps([]Change{c("memory", 2*gb, gb, -4), c("memory", gb, 1.5*gb, 3)}); len(up) != 2 {
		t.Fatalf("down then up: %+v", up)
	}
}
