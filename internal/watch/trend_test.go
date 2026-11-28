package watch_test

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

// The trend of every job and day is what SQL on the ledger file says:
// nearest-rank percentiles of the runs that succeeded, cpu-hours of all.
func TestTrendMatchesLedgerSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	h := synth.Generate(synth.Config{Seed: 3, Start: start, Days: 10})
	for i, r := range h.Runs {
		if i%17 == 0 {
			r.Result = "failed" // some runs fail: counted, not measured
		}
		if err := l.PutRun(r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := l.FinishedRuns(start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := watch.Trend(runs, watch.TrendQuery{From: start, To: start.AddDate(0, 0, 10), Loc: time.UTC})

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	type key struct{ job, day string }
	type want struct {
		runs, ok        int
		p50, p90, mp90  float64
		cpuP50, cpuHour float64
	}
	wants := map[key]*want{}
	rows, err := db.Query(`
	  WITH d AS (SELECT job, date(finished_at) AS day, (julianday(finished_at)-julianday(started_at))*86400 AS sec, mem_work_peak AS mem, cpu_sec AS cpu
	             FROM runners WHERE result='succeeded'),
	  r AS (SELECT job, day, sec, mem, cpu,
	          ROW_NUMBER() OVER (PARTITION BY job, day ORDER BY sec) AS rs,
	          ROW_NUMBER() OVER (PARTITION BY job, day ORDER BY mem) AS rm,
	          ROW_NUMBER() OVER (PARTITION BY job, day ORDER BY cpu) AS rc,
	          COUNT(*) OVER (PARTITION BY job, day) AS n FROM d)
	  SELECT job, day, n,
	    MAX(CASE WHEN rs = (50*n+99)/100 THEN sec END), MAX(CASE WHEN rs = (90*n+99)/100 THEN sec END),
	    MAX(CASE WHEN rm = (90*n+99)/100 THEN mem END), MAX(CASE WHEN rc = (50*n+99)/100 THEN cpu END)
	  FROM r GROUP BY job, day`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k key
		w := &want{}
		if err := rows.Scan(&k.job, &k.day, &w.ok, &w.p50, &w.p90, &w.mp90, &w.cpuP50); err != nil {
			t.Fatal(err)
		}
		wants[k] = w
	}
	rows.Close()
	rows, err = db.Query(`SELECT job, date(finished_at), COUNT(*), SUM(cpu_sec)/3600 FROM runners GROUP BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k key
		var n int
		var cpuh float64
		if err := rows.Scan(&k.job, &k.day, &n, &cpuh); err != nil {
			t.Fatal(err)
		}
		if wants[k] == nil {
			wants[k] = &want{}
		}
		wants[k].runs, wants[k].cpuHour = n, cpuh
	}
	rows.Close()

	checked := 0
	near := func(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
	for _, s := range got {
		if s.Job == "" {
			continue
		}
		for _, b := range s.Buckets {
			w := wants[key{s.Job, b.Start.Format("2006-01-02")}]
			if w == nil {
				if b.Runs != 0 {
					t.Fatalf("%s %s: %d runs, SQL has none", s.Job, b.Start, b.Runs)
				}
				continue
			}
			if b.Runs != w.runs || b.Succeeded != w.ok || !near(b.Sec.P50, w.p50, 1e-3) || !near(b.Sec.P90, w.p90, 1e-3) ||
				!near(b.MemBytes.P90, w.mp90, 0) || !near(b.CPUSec.P50, w.cpuP50, 1e-9) || !near(b.CPUHours, w.cpuHour, 1e-3) {
				t.Fatalf("%s %s:\n trend %+v\n   sql %+v", s.Job, b.Start.Format("2006-01-02"), b, *w)
			}
			checked++
		}
	}
	if checked < 60 {
		t.Fatalf("only %d job-days checked", checked)
	}
}

func TestWorkflowRuns(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	j := func(name string, start, end int, cpu float64, mem int64, wait float64, result string) ledger.JobRun {
		return ledger.JobRun{Repo: "o/r", Workflow: "ci.yml", Job: name, RunID: 1, Attempt: 1, Result: result,
			StartedAt: t0.Add(time.Duration(start) * time.Second), FinishedAt: t0.Add(time.Duration(end) * time.Second),
			RunSec: float64(end - start), CPUSec: cpu, MemWorkPeak: mem, WaitSec: wait}
	}
	w := watch.WorkflowRuns([]ledger.JobRun{j("a", 0, 100, 50, 1<<30, 5, "succeeded"), j("b", 10, 300, 200, 3<<30, 40, "succeeded"), j("c", 20, 60, 5, 1<<29, 0, "failed")})
	if len(w) != 1 || w[0].RunSec != 300 || w[0].CPUSec != 255 || w[0].MemWorkPeak != 3<<30 || w[0].WaitSec != 40 || w[0].Result != "failed" {
		t.Fatalf("%+v", w)
	}
}
