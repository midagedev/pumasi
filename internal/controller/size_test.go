package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

const gbs = int64(1) << 30

// finished records one finished, measured run of a job the way the controller does.
func (r *rig) finished(name string, k ledger.JobKey, cpus float64, work int64, oom int) {
	r.t.Helper()
	at := *r.now
	j := ledger.Job{ID: name, Class: "small", Repo: k.Repo, Workflow: k.Workflow + "@refs/heads/main", Name: k.Job}
	_, err1 := r.ledger.JobStarted(name, j, at)
	err2 := r.ledger.SetUsage(name, ledger.Usage{CPUSec: cpus * 100, MemPeak: 2 * work, MemWorkPeak: work, OOMKills: oom})
	err3 := r.ledger.JobCompleted(name, j, "succeeded", at.Add(100*time.Second))
	err4 := r.ledger.MarkRemoved(name, at.Add(101*time.Second))
	for _, err := range []error{err1, err2, err3, err4} {
		if err != nil {
			r.t.Fatal(err)
		}
	}
	*r.now = at.Add(time.Hour)
}

// The estimate follows the job: a job that grows run by run is placed bigger
// each time, by nobody's hand.
func TestEstimateFollowsTheLedger(t *testing.T) {
	r := newRig(t)
	cl := r.cfg.Classes[0] // 1 cpu, 1G
	k := ledger.KeyOf("o/r", "o/r/.github/workflows/ci.yml@refs/heads/main", "build")
	if e := r.estimate(cl, k); e.Runs != 0 || e.CPUs != 1 || e.MemBytes != gbs || !e.Blind {
		t.Fatalf("a job never seen: %+v, want the class's first estimate, marked blind", e)
	}
	prev := int64(0)
	for i, work := range []int64{2 * gbs, 3 * gbs, 5 * gbs} {
		name := "small-h1-" + string(rune('a'+i))
		if err := r.ledger.InsertRunner(name, "small", "h1", *r.now, r.estimate(cl, k)); err != nil {
			t.Fatal(err)
		}
		r.finished(name, k, 0.5, work, 0)
		e := r.estimate(cl, k)
		if e.Runs != i+1 || e.MemBytes < work || e.MemBytes <= prev {
			t.Fatalf("after a run that held %d: %+v", work, e)
		}
		prev = e.MemBytes
	}
	// A job cut short at a limit is given more than it was seen with.
	if err := r.ledger.InsertRunner("small-h1-oom", "small", "h1", *r.now, r.estimate(cl, k)); err != nil {
		t.Fatal(err)
	}
	r.finished("small-h1-oom", k, 0.5, 6*gbs, 1)
	if e := r.estimate(cl, k); e.MemBytes != 9*gbs {
		t.Errorf("after a run cut short at 6G: %s", e.Source())
	}
	// Another job of the same workflow has no run of its own: it is sized like
	// the largest that is known of the workflow, and is not blind.
	if e := r.estimate(cl, ledger.KeyOf("o/r", k.Workflow, "lint")); e.Runs != 0 || !e.Borrowed || e.Blind || e.MemBytes != 9*gbs {
		t.Errorf("lint: %+v", e)
	}
	// A job of another workflow knows nothing.
	other := ledger.KeyOf("o/r", "o/r/.github/workflows/new.yml@refs/heads/main", "build")
	if e := r.estimate(cl, other); !e.Blind || e.MemBytes != gbs {
		t.Errorf("another workflow: %+v", e)
	}
	// Until a job of it has run long enough to show what it holds now.
	r.seenNow = map[ledger.JobKey]size.Sample{{Repo: other.Repo, Workflow: other.Workflow}: {CPUs: 1.5, MemBytes: 5 * gbs}}
	if e := r.estimate(cl, other); e.Blind || !e.Borrowed || e.MemBytes != 6*gbs || e.CPUs != 1.8 {
		t.Errorf("another workflow with a job running: %+v (%s)", e, e.Source())
	}
}

// A waiting job is placed with what it learned, and its runner is started
// without a limit at that size: the size is only what placement counts on.
func TestStartIsSizedByTheJobAndNotLimitedToIt(t *testing.T) {
	r := newRig(t)
	k := ledger.KeyOf("o/r", "o/r/.github/workflows/ci.yml", "build")
	if err := r.ledger.InsertRunner("small-h1-old", "small", "h1", *r.now, size.Estimate{CPUs: 1, MemBytes: gbs}); err != nil {
		t.Fatal(err)
	}
	r.finished("small-h1-old", k, 0.5, 2*gbs, 0)
	if err := r.ledger.UpsertDemand(ledger.Job{ID: "j2", Class: "small", Repo: k.Repo, Workflow: k.Workflow + "@refs/heads/x", Name: k.Job}, *r.now); err != nil {
		t.Fatal(err)
	}
	r.ag.Sample(context.Background())
	r.classes[0].cfg.OS = r.ag.Snapshot().OS // the rig's agent says the OS the tests run on
	r.Evaluate(context.Background())
	r.starts.Wait()
	live, err := r.ledger.LiveRunners()
	if err != nil || len(live) != 1 {
		t.Fatalf("live runners %+v (%v); deferred: %s", live, err, r.ledger.LastDefer("small"))
	}
	if lr := live[0]; lr.EstMem != 2496<<20 || lr.EstRuns != 1 {
		t.Errorf("placed with %+v, want 2.4G learned from 1 run", lr)
	}
	r.drv.mu.Lock()
	defer r.drv.mu.Unlock()
	spec, ok := r.drv.specs[live[0].Name]
	if !ok {
		t.Fatal("the driver was not asked to start it")
	}
	// The limit is what protects the host (the rig's lends 8G), not the job's size.
	if spec.MemBytes != 2496<<20 || spec.LimitCPUs != 0 || spec.LimitMem <= spec.MemBytes {
		t.Errorf("spec size %d, limits %g/%d", spec.MemBytes, spec.LimitCPUs, spec.LimitMem)
	}
	ds, _, _ := r.ledger.RunnerHistory(live[0].Name)
	if len(ds) == 0 || !strings.Contains(ds[0].Reason, "for build (learned from 1 run") {
		t.Errorf("decision %+v: it does not say what the size comes from", ds)
	}
}

func TestLimits(t *testing.T) {
	plain := config.Class{Name: "a", CPUs: 2, Memory: 4 << 30}
	held := plain
	held.Limit.CPUs, held.Limit.Memory = 4, 8<<30
	tests := []struct {
		name            string
		cl              config.Class
		budget, machine int64
		cpus            float64
		mem             int64
	}{
		{"no limit said: four fifths of the machine, no cpu limit", plain, 0, 40 * gbs, 0, 32 * gbs},
		{"a host that lends less is held to that", plain, 6 * gbs, 40 * gbs, 0, 6 * gbs},
		{"the class's own limit is the limit", held, 6 * gbs, 40 * gbs, 4, 8 * gbs},
	}
	for _, tt := range tests {
		if cpus, mem := limits(tt.cl, tt.budget, tt.machine); cpus != tt.cpus || mem != tt.mem {
			t.Errorf("%s: %g/%d", tt.name, cpus, mem)
		}
	}
}

// The priority rules are read again when the config file changes, without a
// restart; a file that does not load leaves the rules in force.
func TestPriorityRulesAreReadAgain(t *testing.T) {
	r := newRig(t)
	p := filepath.Join(t.TempDir(), "pumasi.yaml")
	write := func(body string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(p, []byte("url: u\nclasses: [{name: small}]\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(age) // file times can be coarse: make each write a different one
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("", -3*time.Minute)
	r.cfg.Path = p
	r.reloadPriority() // the file `up` started with
	job := config.WaitingJob{Class: "small", WorkflowRef: "o/r/.github/workflows/deploy-prod.yml@refs/heads/main"}
	if level, _ := r.cfg.LevelOf(job); level != 0 {
		t.Fatalf("level %d before any rule", level)
	}
	write("priority:\n  - {match: {workflow: \"deploy-*.yml\"}, level: high}\npriorityHeadStart: 1h\n", -2*time.Minute)
	r.reloadPriority()
	if level, why := r.cfg.LevelOf(job); level != 1 || r.cfg.PriorityHeadStart != time.Hour {
		t.Fatalf("after the edit: level %d (%s), head start %s", level, why, r.cfg.PriorityHeadStart)
	}
	write("priority:\n  - {match: {workflow: \"deploy-*.yml\"}, level: urgent}\n", -time.Minute)
	r.reloadPriority()
	if level, _ := r.cfg.LevelOf(job); level != 1 {
		t.Errorf("a file that does not load took the rules away")
	}
	write("", 0)
	r.reloadPriority()
	if level, _ := r.cfg.LevelOf(job); level != 0 || r.cfg.PriorityHeadStart != config.DefaultHeadStart {
		t.Errorf("rules removed from the file are still in force")
	}
}
