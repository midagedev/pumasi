package ledger

import (
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
	// Assigned three times (GitHub reassigns every 5 minutes), still one open demand.
	for i := range 3 {
		must(l.UpsertDemand(j, t0.Add(time.Duration(i)*5*time.Minute)))
	}
	ds, err := l.OpenDemand()
	must(err)
	if len(ds) != 1 || !ds[0].FirstSeen.Equal(t0) || !ds[0].LastSeen.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("demand %+v", ds)
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
