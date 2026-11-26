package ledger

import (
	"fmt"
	"github.com/midagedev/pumasi/internal/size"
	"math"
	"os"
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

	must(l.InsertRunner("r1", "linux-s", "linux/docker", "h1", t0.Add(11*time.Minute), size.Estimate{}))
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
		if err := l.InsertRunner(n, "linux-s", "linux/docker", "server1", t0, size.Estimate{}); err != nil {
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

func TestOpenOrRebuildMovesDamagedFileAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	l, moved, err := OpenOrRebuild(path, time.Now())
	if err != nil || moved != "" {
		t.Fatalf("fresh ledger: moved %q err %v", moved, err)
	}
	_ = l.InsertRunner("r1", "linux-s", "linux/docker", "h", time.Now(), size.Estimate{})
	l.Close()
	if err := os.WriteFile(path, []byte("this is not a database, not even close"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, moved, err = OpenOrRebuild(path, time.Now())
	if err != nil || moved == "" {
		t.Fatalf("damaged ledger: moved %q err %v", moved, err)
	}
	defer l.Close()
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("damaged copy not kept: %v", err)
	}
	if rs, _ := l.LiveRunners(); len(rs) != 0 {
		t.Fatalf("rebuilt ledger not empty: %+v", rs)
	}
	now := time.Now()
	if err := l.InsertAdopted("linux-l-h-1", "linux-l", "h", 7, now, now, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Runner("linux-l-h-1"); r.State != "busy" || r.RunnerID != 7 {
		t.Fatalf("adopted runner %+v", r)
	}
}

func TestRerunQueueHonoursTheLimit(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Now()
	if ok, _ := l.RequestRerun("o/r", 1, "runner died", now, 1); !ok {
		t.Fatal("first request not queued")
	}
	if rs, _ := l.PendingReruns(); len(rs) != 1 || rs[0].RunID != 1 || rs[0].Repo != "o/r" {
		t.Fatalf("pending %+v", rs)
	}
	_ = l.RerunSettled(1, true)
	if ok, _ := l.RequestRerun("o/r", 1, "runner died again", now, 1); ok {
		t.Fatal("second rerun queued although the limit is 1")
	}
	if ok, _ := l.RequestRerun("o/r", 1, "runner died again", now, 2); !ok {
		t.Fatal("second rerun refused although the limit is 2")
	}
}

// A job is the same job on every branch; its measured runs come back newest
// first, and runs from before working memory was measured do not count.
func TestJobRuns(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t0 := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	run := func(i int, job, ref string, work int64, oom int, removed bool) {
		name := fmt.Sprintf("r%d", i)
		at := t0.Add(time.Duration(i) * time.Hour)
		must(l.InsertRunner(name, "linux-s", "linux/docker", "h1", at, size.Estimate{CPUs: 2, MemBytes: 4 << 30}))
		_, err := l.JobStarted(name, Job{ID: name, Class: "linux-s", Repo: "o/r", Workflow: "o/r/.github/workflows/ci.yml@" + ref, Name: job}, at)
		must(err)
		must(l.SetUsage(name, Usage{CPUSec: 300, MemPeak: 15 << 30, MemWorkPeak: work, OOMKills: oom, LimitMem: 6 << 30}))
		must(l.JobCompleted(name, Job{ID: name}, "succeeded", at.Add(100*time.Second)))
		if removed {
			must(l.MarkRemoved(name, at.Add(101*time.Second)))
		}
	}
	run(1, "build", "refs/heads/main", 1<<30, 0, true)
	run(2, "build", "refs/pull/7/merge", 2<<30, 0, true)
	run(3, "build", "refs/heads/main", 0, 0, true)      // from before working memory was measured
	run(4, "test", "refs/heads/main", 9<<30, 0, true)   // another job
	run(5, "build", "refs/heads/main", 3<<30, 1, true)  // cut short at its 6G limit before a sample saw more than 3G
	run(6, "build", "refs/heads/main", 8<<30, 0, false) // still on its host: its last sample is not in yet

	k := KeyOf("o/r", "o/r/.github/workflows/ci.yml@refs/heads/other", "build")
	k.Platform = "linux/docker"
	runs, err := l.JobRuns(k, 10)
	must(err)
	want := []size.Sample{{CPUs: 3, MemBytes: 6 << 30, OOM: true}, {CPUs: 3, MemBytes: 2 << 30}, {CPUs: 3, MemBytes: 1 << 30}}
	for i := range runs {
		runs[i].CPUs = math.Round(runs[i].CPUs*1000) / 1000 // julianday is a float of days
	}
	if fmt.Sprint(runs) != fmt.Sprint(want) {
		t.Fatalf("runs %+v, want %+v", runs, want)
	}
	if runs, _ := l.JobRuns(k, 2); len(runs) != 2 || runs[0].MemBytes != 6<<30 {
		t.Errorf("the newest two: %+v", runs)
	}
	keys, err := l.MeasuredJobs(10)
	must(err)
	if len(keys) != 2 || keys[0] != k || keys[1].Job != "test" {
		t.Errorf("measured jobs %+v", keys)
	}
	r, err := l.Runner("r1")
	must(err)
	if r.EstCPUs != 2 || r.EstMem != 4<<30 || r.MemWorkPeak != 1<<30 || r.MemPeak != 15<<30 {
		t.Errorf("runner %+v", r)
	}
}
