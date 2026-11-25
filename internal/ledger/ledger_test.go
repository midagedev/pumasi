package ledger

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestLifecycle(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	j := Job{ID: "job-1", Class: "linux-s", Repo: "o/r", RunID: 7, Name: "burn"}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// GitHub cancels an assignment nobody took after 5 minutes and assigns the job
	// again under a new id, three times; one open demand remains, as old as the first.
	for i := range 3 {
		at := t0.Add(time.Duration(i) * 5 * time.Minute)
		if i > 0 {
			must(l.CloseDemand(j.ID, "unassigned-canceled", at))
			j.ID = fmt.Sprintf("job-%d", i+1)
		}
		must(l.UpsertDemand(j, at))
	}
	must(l.UpsertDemand(j, t0.Add(11*time.Minute))) // a redelivered message changes nothing but last_seen
	ds, err := l.OpenDemand()
	must(err)
	if len(ds) != 1 || ds[0].JobID != "job-3" || !ds[0].FirstSeen.Equal(t0) || !ds[0].LastSeen.Equal(t0.Add(11*time.Minute)) {
		t.Fatalf("demand %+v", ds)
	}
	var n int
	must(l.db.QueryRow(`SELECT assigned_count FROM demand WHERE job_id='job-3'`).Scan(&n))
	if n != 3 {
		t.Fatalf("assigned_count %d", n)
	}

	must(l.InsertRunner("r1", "linux-s", "h1", t0.Add(11*time.Minute)))
	must(l.SetRegistration("r1", 42, t0.Add(11*time.Minute+time.Second)))
	must(l.MarkListening("r1", t0.Add(11*time.Minute+9*time.Second), 9000))
	r, err := l.Runner("r1")
	must(err)
	if r.State != "idle" || r.RunnerID != 42 {
		t.Fatalf("runner %+v", r)
	}

	ours, err := l.JobStarted("r1", j, t0.Add(12*time.Minute))
	must(err)
	if !ours {
		t.Fatal("r1 should be ours")
	}
	if ds, _ := l.OpenDemand(); len(ds) != 0 {
		t.Fatalf("demand still open %+v", ds)
	}
	code := 0
	must(l.SetUsage("r1", Usage{CPUSec: 10, MemPeak: 100, ExitCode: &code}))
	must(l.SetUsage("r1", Usage{CPUSec: 4, MemPeak: 50})) // a later, smaller sample never lowers the record
	must(l.JobCompleted("r1", j, "succeeded", t0.Add(14*time.Minute)))
	live, err := l.LiveRunners()
	must(err)
	if len(live) != 1 || live[0].State != "done" {
		t.Fatalf("live %+v", live)
	}
	must(l.MarkRemoved("r1", t0.Add(14*time.Minute)))
	if live, _ := l.LiveRunners(); len(live) != 0 {
		t.Fatalf("live after remove %+v", live)
	}
	pend, err := l.PendingUnregister()
	must(err)
	if len(pend) != 1 {
		t.Fatalf("pending %+v", pend)
	}
	must(l.MarkUnregistered("r1"))
	if pend, _ := l.PendingUnregister(); len(pend) != 0 {
		t.Fatalf("pending after unregister %+v", pend)
	}
	var cpu float64
	var peak int64
	must(l.db.QueryRow(`SELECT cpu_sec, mem_peak FROM runners WHERE name='r1'`).Scan(&cpu, &peak))
	if cpu != 10 || peak != 100 {
		t.Fatalf("usage %v %v", cpu, peak)
	}
}

// A controller that was away misses JobCompleted: the exit code closes the row,
// and a late message still wins. A written-off runner found working comes back.
func TestCloseFromExitAndRevive(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	result := func(name string) (state, res string) {
		l.db.QueryRow(`SELECT state, COALESCE(result,'') FROM runners WHERE name=?`, name).Scan(&state, &res)
		return
	}
	for _, n := range []string{"a", "b"} {
		if err := l.InsertRunner(n, "linux-s", "server1", t0); err != nil {
			t.Fatal(err)
		}
	}
	l.MarkBusy("a", t0)
	l.CloseFromExit("a", 0, t0.Add(time.Hour))
	if s, r := result("a"); s != "done" || r != "exit-0" {
		t.Fatalf("closed from exit: %s %s", s, r)
	}
	l.JobCompleted("a", Job{ID: "j"}, "succeeded", t0.Add(time.Hour))
	if _, r := result("a"); r != "succeeded" {
		t.Fatalf("late message should win: %s", r)
	}

	l.SetState("b", "lost", "host unreachable")
	if ok, _ := l.Revive("b"); !ok {
		t.Fatal("not revived")
	}
	live, _ := l.LiveRunners()
	if s, _ := result("b"); s != "busy" || len(live) != 2 {
		t.Fatalf("revived: %s live %+v", s, live)
	}
	if ok, _ := l.Revive("a"); ok {
		t.Fatal("a finished runner must not be revived")
	}
}
