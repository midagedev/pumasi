package ledger

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/size"
)

// The columns the ledger had before run facts, as a release before them left
// the file.
var columnsBeforeFacts = []string{"runners ADD COLUMN log_tail TEXT", "runners ADD COLUMN oom_kills INTEGER",
	"runners ADD COLUMN mem_work_peak INTEGER", "runners ADD COLUMN est_cpus REAL", "runners ADD COLUMN est_mem INTEGER", "runners ADD COLUMN est_runs INTEGER", "runners ADD COLUMN est_blind INTEGER",
	"runners ADD COLUMN limit_mem INTEGER", "runners ADD COLUMN platform TEXT", "demand ADD COLUMN event TEXT"}

func dumpTables(t *testing.T, db *sql.DB, tables ...string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, tb := range tables {
		rows, err := db.Query(`SELECT * FROM ` + tb + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			m := map[string]any{}
			for i, c := range cols {
				m[c] = vals[i]
			}
			out[tb] = append(out[tb], m)
		}
		rows.Close()
	}
	return out
}

// A ledger written before run facts opens with every row and every value it
// had; the new columns are empty, and its runs read back.
func TestRunFactsMigrationKeepsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + opsSchema); err != nil {
		t.Fatal(err)
	}
	for _, c := range columnsBeforeFacts {
		if _, err := db.Exec("ALTER TABLE " + c); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := range 50 {
		at := func(d time.Duration) string { return t0.Add(time.Duration(i)*time.Hour + d).Format(TimeFormat) }
		name := fmt.Sprintf("r%d", i)
		if _, err := db.Exec(`INSERT INTO demand(job_id, class, repo, workflow, job, run_id, labels, queue_time, first_seen, last_seen, assigned_count, served_by, closed_at, event)
		  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "j"+name, "c", "o/r", "o/r/.github/workflows/ci.yml@refs/heads/main", "test", 100+i, "self-hosted",
			at(0), at(0), at(time.Minute), 1, name, at(time.Minute), "push"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO runners(name, class, host, state, reason, runner_id, created_at, posted_at, listening_at, provision_ms, assigned_job_id, repo, workflow, job, run_id,
		    started_at, finished_at, result, cpu_sec, mem_peak, io_bytes, psi_cpu_sec, psi_mem_sec, psi_io_sec, exit_code, removed_at, unregistered,
		    log_tail, oom_kills, mem_work_peak, est_cpus, est_mem, est_runs, est_blind, limit_mem, platform)
		  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?,?,?)`,
			name, "c", "h1", "done", "finished", 1000+i, at(time.Minute), at(time.Minute), at(70*time.Second), 9000, "j"+name, "o/r",
			"o/r/.github/workflows/ci.yml@refs/heads/main", "test", 100+i, at(80*time.Second), at(5*time.Minute), "succeeded", 120.5+float64(i), 3<<30, 1<<20,
			1.5, 0.25, 0.125, 0, at(6*time.Minute), 1, "last line", 0, 2<<30, 2.0, 4<<30, 5, 0, 8<<30, "linux/docker"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO incidents(key, kind, subject, opened_at, detail) VALUES('k','host-unreachable','h1',?, 'h1 unreachable')`, t0.Format(TimeFormat)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO decisions(at, class, host, action, runner, reason) VALUES(?,?,?,?,?,?)`, t0.Format(TimeFormat), "c", "h1", "start", "r0", "fits"); err != nil {
		t.Fatal(err)
	}
	tables := []string{"runners", "demand", "incidents", "decisions", "outbox", "reruns", "expedites", "priority_rules", "settings"}
	before := dumpTables(t, db, tables...)
	db.Close()

	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	after := dumpTables(t, l.db, tables...)
	for _, tb := range tables {
		if len(before[tb]) != len(after[tb]) {
			t.Fatalf("%s: %d rows before, %d after", tb, len(before[tb]), len(after[tb]))
		}
		for i, row := range before[tb] {
			for col, v := range row {
				if !reflect.DeepEqual(after[tb][i][col], v) {
					t.Fatalf("%s row %d %s: %v before, %v after", tb, i, col, v, after[tb][i][col])
				}
			}
			for col, v := range after[tb][i] {
				if _, old := row[col]; !old && v != nil && !(col == "unregistered") {
					t.Fatalf("%s row %d: new column %s is %v, want NULL", tb, i, col, v)
				}
			}
		}
	}
	runs, err := l.FinishedRuns(t0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 50 || runs[0].Workflow != ".github/workflows/ci.yml" || runs[0].Event != "push" || runs[0].RunSec != 220 || runs[0].WaitSec != 80 || runs[0].Steps != nil {
		t.Fatalf("runs %d, first %+v", len(runs), runs[0])
	}
	// Opening again changes nothing.
	l.Close()
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if again := dumpTables(t, l2.db, tables...); !reflect.DeepEqual(again, after) {
		t.Fatal("a second open changed the ledger")
	}
}

func TestRefFacts(t *testing.T) {
	for ref, want := range map[string]struct {
		branch string
		pr     int
	}{
		"o/r/.github/workflows/ci.yml@refs/heads/main":      {"main", 0},
		"o/r/.github/workflows/ci.yml@refs/heads/feat/x":    {"feat/x", 0},
		"o/r/.github/workflows/ci.yml@refs/pull/412/merge":  {"", 412},
		"o/r/.github/workflows/ci.yml@refs/tags/v1":         {"", 0},
		"o/r/.github/workflows/ci.yml":                      {"", 0},
		"o/r/.github/workflows/ci.yml@refs/pull/oops/merge": {"", 0},
	} {
		b, pr := RefFacts(ref)
		if b != want.branch || pr != want.pr {
			t.Errorf("%s: %q %d, want %q %d", ref, b, pr, want.branch, want.pr)
		}
	}
}

func TestRunFactsLifecycle(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	j := Job{ID: "g1", Class: "c", Repo: "o/r", Workflow: "o/r/.github/workflows/ci.yml@refs/pull/7/merge", Name: "test", RunID: 5, Event: "pull_request"}
	must(l.UpsertDemand(j, t0))
	must(l.InsertRunner("r1", "c", "linux/docker", "h1", t0.Add(time.Second), size.Estimate{}))
	_, err = l.JobStarted("r1", j, t0.Add(20*time.Second))
	must(err)
	must(l.AddContention([]string{"r1"}, 10, "host cpu 97% busy"))
	must(l.AddContention([]string{"r1"}, 10, "host cpu 99% busy"))
	must(l.JobCompleted("r1", j, "succeeded", t0.Add(120*time.Second)))

	if w, _ := l.WantFacts(t0.Add(125*time.Second), 15*time.Second, 24*time.Hour, 10); len(w) != 0 {
		t.Fatalf("asked before GitHub had time: %+v", w)
	}
	w, err := l.WantFacts(t0.Add(140*time.Second), 15*time.Second, 24*time.Hour, 10)
	must(err)
	if len(w) != 1 || w[0] != (FactsWanted{"r1", "o/r", 5}) {
		t.Fatalf("want %+v", w)
	}
	must(l.AskedFacts("r1"))
	if w, _ := l.WantFacts(t0.Add(140*time.Second), 15*time.Second, 24*time.Hour, 10); len(w) != 0 {
		t.Fatalf("asked twice: %+v", w)
	}
	must(l.SetFacts("r1", Facts{SHA: "abc123", Branch: "feat/x", GHJobID: 991, Attempt: 1, Steps: []Step{
		{Number: 1, Name: "Set up job", StartedAt: t0.Add(20 * time.Second), CompletedAt: t0.Add(22 * time.Second), Conclusion: "success"},
		{Number: 2, Name: "Run tests", StartedAt: t0.Add(22 * time.Second), CompletedAt: t0.Add(118 * time.Second), Conclusion: "success"},
	}}))
	runs, err := l.FinishedRuns(t0, time.Time{})
	must(err)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	r := runs[0]
	if r.SHA != "abc123" || r.Branch != "feat/x" || r.PR != 7 || r.Event != "pull_request" || r.GHJobID != 991 || r.Attempt != 1 ||
		r.RunSec != 100 || r.WaitSec != 20 || r.ContendedSec != 20 || r.ContendedWhy != "host cpu 99% busy" || !r.Contended() ||
		len(r.Steps) != 2 || r.Steps[1].Name != "Run tests" || r.Steps[1].Sec != 96 {
		t.Fatalf("run %+v", r)
	}
	n, err := l.PruneSteps(t0.Add(time.Hour))
	must(err)
	if n != 2 {
		t.Fatalf("pruned %d", n)
	}
	runs, _ = l.FinishedRuns(t0, time.Time{})
	if len(runs) != 1 || runs[0].Steps != nil || runs[0].SHA != "abc123" {
		t.Fatalf("after prune %+v", runs)
	}
}

// PutRun writes what FinishedRuns reads back.
func TestPutRunRoundTrip(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t0 := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	in := JobRun{Runner: "x1", Class: "c", Host: "h", Platform: "linux/docker", Repo: "o/r", Workflow: ".github/workflows/ci.yml", Job: "build",
		RunID: 9, GHJobID: 77, Attempt: 2, Event: "push", Branch: "main", SHA: "deadbeef", Result: "succeeded",
		QueuedAt: t0, StartedAt: t0.Add(5 * time.Second), FinishedAt: t0.Add(65 * time.Second), WaitSec: 5, RunSec: 60, CPUSec: 90, MemPeak: 5 << 30, MemWorkPeak: 3 << 30,
		ContendedSec: 4, ContendedWhy: "memory pressure 12%",
		Steps: []Step{{Number: 1, Name: "Build", StartedAt: t0.Add(5 * time.Second), CompletedAt: t0.Add(60 * time.Second), Sec: 55, Conclusion: "success"}}}
	if err := l.PutRun(in); err != nil {
		t.Fatal(err)
	}
	out, err := l.FinishedRuns(t0, time.Time{})
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	got := out[0]
	for i := range got.Steps {
		got.Steps[i].StartedAt, got.Steps[i].CompletedAt = got.Steps[i].StartedAt.UTC(), got.Steps[i].CompletedAt.UTC()
	}
	got.QueuedAt, got.StartedAt, got.FinishedAt = got.QueuedAt.UTC(), got.StartedAt.UTC(), got.FinishedAt.UTC()
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("\n got %+v\nwant %+v", got, in)
	}
}
