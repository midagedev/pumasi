package watch_test

import (
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

// A host that gets faster for every job pinned to it is one line, not a
// change of each job; a job's own regression elsewhere in the history is
// still its own.
func TestHostShiftIsOneLine(t *testing.T) {
	start := synth.EvalStart
	shiftAt := 15*24*time.Hour + 2*time.Hour
	h := synth.Generate(synth.Config{Seed: 5, Start: start, Days: 28, Pinned: true,
		HostShifts: []synth.HostShift{{Host: "linux-slow", After: shiftAt, Factor: 0.7}},
		Inject:     []synth.Planned{{Change: synth.Change{Job: "lint", Metric: "duration", Factor: 1.4}, After: 9 * 24 * time.Hour}}})
	at := start.Add(shiftAt)

	jobs, shifts := watch.FindAll(h.Runs, watch.Options{})
	var shift *watch.Shift
	for i, s := range shifts {
		if s.Kind == "host" && s.At.Sub(at).Abs() < 24*time.Hour {
			shift = &shifts[i]
		}
	}
	if shift == nil || shift.Jobs < 3 || shift.Slower || strings.Join(shift.Hosts, ",") != "linux-slow" {
		t.Fatalf("no host shift on linux-slow near %v: %+v", at, shifts)
	}
	if line := watch.ShiftLine(*shift); !strings.HasPrefix(line, "linux-slow: ") || !strings.Contains(line, "faster since") || !strings.Contains(line, "not the code") {
		t.Errorf("line: %s", line)
	}
	for _, c := range jobs {
		if c.Host == "linux-slow" && c.First.At.Sub(at).Abs() < 24*time.Hour {
			t.Errorf("a job's change at the host's: %s %s %+.0f%%", c.Job, c.Metric, 100*c.Ratio)
		}
	}
	found := false
	for _, c := range jobs {
		if c.Job == "lint" && c.Metric == "time" && c.Slower() && c.First.SHA == h.Injected[0].SHA {
			found = true
		}
	}
	if !found {
		t.Errorf("the lint regression at %s is not listed: %+v", h.Injected[0].SHA[:7], jobs)
	}

	// The search as it was lists the host's change as the jobs'.
	plain := 0
	for _, c := range watch.Find(h.Runs, watch.Options{Plain: true}) {
		if c.First.At.Sub(at).Abs() < 24*time.Hour {
			plain++
		}
	}
	if plain < 3 {
		t.Errorf("plain search found %d changes at the host's change; the test shows nothing", plain)
	}
}

// A job that went from a few machines to another kind is not a change of
// the job: what it ran on before is not what it runs on now.
func TestJobThatMovedIsNotItsChange(t *testing.T) {
	base := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	var runs []ledger.JobRun
	add := func(i int, host, class string, sec float64) {
		at := base.Add(time.Duration(i) * 90 * time.Minute)
		runs = append(runs, ledger.JobRun{Runner: "r" + string(rune('a'+i%26)) + at.Format("150405"), Repo: "o/r", Workflow: ".github/workflows/ci.yml",
			Job: "e2e", Host: host, Class: class, Result: "succeeded", RunID: int64(1000 + i), QueuedAt: at, StartedAt: at,
			FinishedAt: at.Add(time.Duration(sec) * time.Second), RunSec: sec, CPUSec: sec / 2, MemWorkPeak: 1 << 30})
	}
	// Before: 20s on four machines, three runs each; after: 45s on another.
	before := []string{"server1/large", "server1/small", "mac1/default", "server1/disk"}
	for i := range 12 {
		m := strings.SplitN(before[i%4], "/", 2)
		add(i, m[0], m[1], 20+float64(i%3))
	}
	for i := 12; i < 40; i++ {
		add(i, "server2", "medium", 45+float64(i%3))
	}
	jobs, shifts := watch.FindAll(runs, watch.Options{MinBefore: -1, MinAfter: -1})
	for _, c := range jobs {
		t.Errorf("listed as the job's change: %s %+.0f%%", c.Metric, 100*c.Ratio)
	}
	if len(shifts) == 0 || shifts[0].Kind != "moved" {
		t.Fatalf("not shown as moved: %+v", shifts)
	}
	if line := watch.ShiftLine(shifts[0]); !strings.Contains(line, "when it moved from") || !strings.Contains(line, "to server2/medium") {
		t.Errorf("line: %s", line)
	}
}

// A ledger hours old lists no job's change, and says so once.
func TestYoungLedgerSaysSo(t *testing.T) {
	start := synth.EvalStart
	h := synth.Generate(synth.Config{Seed: 9, Start: start, Days: 2,
		Inject: []synth.Planned{{Change: synth.Change{Job: "build", Metric: "duration", Factor: 1.6}, After: 12 * time.Hour}}})
	now := start.Add(30 * time.Hour)
	var runs []ledger.JobRun
	for _, r := range h.Runs {
		if !r.QueuedAt.Before(start) && r.FinishedAt.Before(now) {
			runs = append(runs, r)
		}
	}
	q := watch.TrendQuery{From: start, To: now, Loc: time.UTC}
	rep := watch.BuildTrend(runs, q, watch.Options{})
	if len(rep.Changes) != 0 || !strings.HasPrefix(rep.Notice, "only 30h of history") {
		t.Fatalf("changes %d, notice %q", len(rep.Changes), rep.Notice)
	}
	// Asked to list at once, the step is there.
	rep = watch.BuildTrend(runs, q, watch.Options{MinBefore: -1, MinAfter: -1})
	if rep.Notice != "" {
		t.Errorf("notice without the rule: %q", rep.Notice)
	}
	if len(rep.Changes) == 0 {
		t.Errorf("the +60%% build step is not found even at once")
	}
}

// A host slow for days and then back is one line, "slower from … to …,
// back since". A spell shorter than three days is dropped as transient by
// each job's search before, so it is not shown at all.
func TestHostSpellBackIsOneLine(t *testing.T) {
	start := synth.EvalStart
	from := 14*24*time.Hour + 2*time.Hour
	h := synth.Generate(synth.Config{Seed: 6, Start: start, Days: 28, Pinned: true,
		HostShifts: []synth.HostShift{{Host: "linux-fast", After: from, For: 5 * 24 * time.Hour, Factor: 1.6}}})
	_, shifts := watch.FindAll(h.Runs, watch.Options{})
	var lines []string
	for _, s := range shifts {
		lines = append(lines, watch.ShiftLine(s))
	}
	for _, s := range shifts {
		if s.Kind == "host" && s.Slower && s.Back && s.Until.Sub(s.At) > 4*24*time.Hour {
			if l := watch.ShiftLine(s); !strings.Contains(l, "slower from") || !strings.Contains(l, "back since") {
				t.Errorf("line: %s", l)
			}
			return
		}
	}
	t.Errorf("no spell line: %q", lines)
}
