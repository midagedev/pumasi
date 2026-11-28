package watch_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]watch.Target{
		"https://github.com/o/r/actions/runs/123":              {RunID: 123},
		"https://github.com/o/r/actions/runs/123/job/456?pr=7": {RunID: 123},
		"https://github.com/o/r/pull/45":                       {PR: 45},
		"https://github.com/o/r/pull/45/files":                 {PR: 45},
		"#45":                                                  {PR: 45},
		"pr/45":                                                {PR: 45},
		"9876":                                                 {RunID: 9876},
		"4F2A1B3":                                              {SHA: "4f2a1b3"},
	} {
		got, err := watch.ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "main", "#x", "abc"} {
		if _, err := watch.ParseTarget(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Asked about the pull request that made each of the demo's changes, or its
// merge commit, why-slow shows that change as the target's own; asked about
// a run that is not there, it says so.
func TestWhySlowNamesTheDemosChanges(t *testing.T) {
	end := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	h := synth.Demo(end)
	for _, in := range h.Injected {
		if in.Creep > 0 {
			continue // no single pull request to name
		}
		metric := map[string]string{"duration": "time", "mem": "memory", "cpu": "cpu"}[in.Metric]
		for _, tgt := range []watch.Target{{PR: in.PR}, {SHA: in.SHA[:7]}} {
			rep, err := watch.WhySlow(h.Runs, tgt)
			if err != nil {
				t.Fatalf("%s %+v: %v", in.Label, tgt, err)
			}
			ok := false
			for i, c := range rep.Own {
				if c.Job == in.Job && c.Metric == metric && c.First.SHA == in.SHA && rep.OwnSure[i] {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s %+v: own %+v", in.Label, tgt, rep.Own)
			}
			for _, c := range rep.Changes {
				if c.First.SHA == in.SHA {
					t.Errorf("%s %+v: its own change also listed as main's", in.Label, tgt)
				}
			}
		}
	}
	if _, err := watch.WhySlow(h.Runs, watch.Target{RunID: 999_999_999}); err == nil || !strings.Contains(err.Error(), "no finished job") {
		t.Fatalf("missing run: %v", err)
	}
}

func mkRuns(n int, host string, at time.Time, sec float64, mutate ...func(i int, r *ledger.JobRun)) []ledger.JobRun {
	var rs []ledger.JobRun
	for i := range n {
		end := at.Add(time.Duration(i) * 20 * time.Minute)
		s := sec * (1 + 0.04*math.Sin(float64(i)*1.7))
		r := ledger.JobRun{Runner: host + end.Format("0102150405") + "-" + string(rune('a'+i%26)), Host: host, Repo: "o/r", Workflow: ".github/workflows/ci.yml",
			Job: "test", RunID: end.Unix(), Event: "push", Branch: "main", Result: "succeeded",
			StartedAt: end.Add(-time.Duration(s) * time.Second), FinishedAt: end, RunSec: s, CPUSec: s, MemWorkPeak: 1 << 30}
		for _, m := range mutate {
			m(i, &r)
		}
		rs = append(rs, r)
	}
	return rs
}

func pr(n int) func(int, *ledger.JobRun) {
	return func(i int, r *ledger.JobRun) {
		r.Event, r.Branch, r.PR, r.RunID = "pull_request", "feature", n, int64(9000+i)
	}
}

// One slow run is a maybe; two, each well over the usual, are slower; a
// run slow because its host was slow for everyone is usual.
func TestWhySlowVerdicts(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	base := mkRuns(30, "h1", t0, 300)
	verdict := func(target []ledger.JobRun, extra ...ledger.JobRun) watch.JobWhy {
		t.Helper()
		all := append(append(append([]ledger.JobRun{}, base...), extra...), target...)
		rep, err := watch.WhySlow(all, watch.Target{PR: target[0].PR})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range rep.Jobs {
			if j.Job == "test" {
				return j
			}
		}
		t.Fatalf("no test job: %+v", rep.Jobs)
		return watch.JobWhy{}
	}
	later := t0.Add(12 * time.Hour)
	if j := verdict(mkRuns(1, "h1", later, 400, pr(1))); j.Time.Verdict != "maybe slower" || j.Time.N != 1 {
		t.Errorf("one run +33%%: %+v", j.Time)
	}
	if j := verdict(mkRuns(2, "h1", later, 400, pr(2))); j.Time.Verdict != "slower" || j.Time.Ratio < 0.25 {
		t.Errorf("two runs +33%%: %+v", j.Time)
	}
	mixed := append(mkRuns(1, "h1", later, 400, pr(3)), mkRuns(1, "h1", later.Add(time.Hour), 300, pr(3), func(_ int, r *ledger.JobRun) { r.RunID = 9100 })...)
	if j := verdict(mixed); j.Time.Verdict == "slower" {
		t.Errorf("one slow run and one usual: %+v", j.Time)
	}
	// Its host was 1.4x slow for three other jobs around then.
	var others []ledger.JobRun
	for _, job := range []string{"lint", "build", "e2e"} {
		job := job
		named := func(i int, r *ledger.JobRun) { r.Job, r.Runner = job, r.Runner+job }
		others = append(others, mkRuns(20, "h1", t0, 200, named)...)
		others = append(others, mkRuns(1, "h1", later.Add(time.Minute), 280, named, func(_ int, r *ledger.JobRun) { r.RunID = 7000 })...)
	}
	j := verdict(mkRuns(2, "h1", later, 420, pr(4)), others...)
	if j.Time.Verdict != "usual" || j.SlowHost < 1.3 || j.SlowHost > 1.5 {
		t.Errorf("slow host: %+v slow %.2f", j.Time, j.SlowHost)
	}
	// A run on a host saturated by others is left out when another is not.
	busy := append(mkRuns(1, "h1", later, 600, pr(5), func(_ int, r *ledger.JobRun) { r.ContendedSec = 400 }),
		mkRuns(1, "h1", later.Add(time.Hour), 305, pr(5), func(_ int, r *ledger.JobRun) { r.RunID = 9200 })...)
	if j := verdict(busy); j.Time.Verdict != "usual" || j.Time.N != 1 || j.Contended {
		t.Errorf("contended run counted: %+v %v", j.Time, j.Contended)
	}
}
