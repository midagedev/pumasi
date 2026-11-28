package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

// e2e has come 3s after unit ended in the runs before: as unit runs out its
// time now, the snapshot expects an e2e, and says so.
func TestSnapshotExpectsTheJobThatFollows(t *testing.T) {
	r, _ := opsRig(t)
	on := true
	r.cfg.Ahead = &on
	r.cfg.EvaluateEvery = 10 * time.Second
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	wf := "o/r/.github/workflows/ci.yml@refs/pull/3/merge"
	unit := func(name string, run int64, start time.Time, done bool) {
		j := ledger.Job{ID: name, Class: "small", Repo: "o/r", Workflow: wf, Name: "unit", RunID: run}
		must(r.ledger.UpsertDemand(j, start.Add(-time.Second)))
		must(r.ledger.InsertRunner(name, "small", r.classes[0].cfg.Platform(), "h1", start.Add(-time.Second), size.Estimate{CPUs: 1, MemBytes: 1 << 30}))
		_, err := r.ledger.JobStarted(name, j, start)
		must(err)
		if done {
			must(r.ledger.SetUsage(name, ledger.Usage{CPUSec: 50, MemPeak: 1 << 30, MemWorkPeak: 1 << 30}))
			must(r.ledger.JobCompleted(name, j, "succeeded", start.Add(100*time.Second)))
			must(r.ledger.MarkRemoved(name, start.Add(101*time.Second)))
		}
	}
	e2e := func(id string, run int64, at time.Time) {
		must(r.ledger.UpsertDemand(ledger.Job{ID: id, Class: "other", Repo: "o/r", Workflow: wf, Name: "e2e", RunID: run}, at))
	}
	now := *r.now
	for i, ago := range []time.Duration{3 * time.Hour, 2 * time.Hour} {
		start := now.Add(-ago)
		unit("u"+string(rune('a'+i)), int64(i+1), start, true)
		e2e("e"+string(rune('a'+i)), int64(i+1), start.Add(103*time.Second))
	}
	if s := r.decideSnapshot(now, nil); s.Classes[1].Soon != 0 {
		t.Fatalf("nothing runs, yet %+v", s.Classes[1])
	}
	r.needsAt = time.Time{}
	unit("uc", 3, now.Add(-95*time.Second), false)
	s := r.decideSnapshot(now, nil)
	other := s.Classes[1]
	if other.Name != "other" || other.Soon != 1 || !strings.HasPrefix(other.SoonFor, "e2e expected in ~8s, when unit ends") {
		t.Fatalf("other: %+v", other)
	}
	if s.Classes[0].Soon != 0 {
		t.Fatalf("small: %+v", s.Classes[0])
	}
	// Its e2e came: no more expected.
	e2e("ec", 3, now)
	if s := r.decideSnapshot(now, nil); s.Classes[1].Soon != 0 {
		t.Fatalf("after it came: %+v", s.Classes[1])
	}
}
