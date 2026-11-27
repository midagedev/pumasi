package controller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
	"path/filepath"
)

func overflowController(classes ...config.Class) *Controller {
	c := &Controller{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, cc := range classes {
		c.classes = append(c.classes, &class{cfg: cc, retaken: make(chan struct{}, 1)})
	}
	return c
}

// The take is what the class holds plus what it has room for; a class that
// comes after another offers room only while that one has none.
func TestTakeIsHeldPlusRoom(t *testing.T) {
	c := overflowController(config.Class{Name: "linux", Overflow: true}, config.Class{Name: "mac", Overflow: true, After: "linux"}, config.Class{Name: "all"})
	s := decide.Snapshot{
		Demand: []decide.Demand{{ID: "a", Class: "linux"}, {ID: "b", Class: "all"}},
		Runners: []decide.Runner{
			{Name: "r1", Class: "linux", State: "busy", JobKnown: true},
			{Name: "r2", Class: "linux", State: "idle"},                 // covers the waiting job: not counted twice
			{Name: "r3", Class: "linux", State: "done", JobKnown: true}, // its job is over
			{Name: "r4", Class: "mac", State: "busy", JobKnown: true},
		},
	}
	takes := func() [3]int32 {
		return [3]int32{c.classes[0].take.Load(), c.classes[1].take.Load(), c.classes[2].take.Load()}
	}
	c.setTakes(s, decide.Plan{Room: map[string]int{"linux": 3, "mac": 2}})
	if got := takes(); got != [3]int32{2 + 3, 1 + 0, 0} {
		t.Fatalf("takes %v, want linux 5 (holds 2, room 3), mac 1 (linux has room), all untouched", got)
	}
	select {
	case <-c.classes[0].retaken:
	default:
		t.Error("a changed take is not told to the poll that is out")
	}
	c.setTakes(s, decide.Plan{Room: map[string]int{"linux": 3, "mac": 2}})
	select {
	case <-c.classes[0].retaken:
		t.Error("an unchanged take gives up the poll")
	default:
	}
	// linux cannot place the job it holds: it asks only for the one that runs.
	c.setTakes(s, decide.Plan{Room: map[string]int{"linux": 0, "mac": 0}, Defer: []decide.Defer{{Class: "linux", Count: 1}}})
	if got := takes(); got != [3]int32{1, 1, 0} {
		t.Fatalf("takes %v, want linux 1 (the running job; the one without a place is given back)", got)
	}
	// linux is full: mac opens.
	c.setTakes(s, decide.Plan{Room: map[string]int{"linux": 0, "mac": 2}})
	if got := takes(); got != [3]int32{2, 1 + 2, 0} {
		t.Fatalf("takes %v, want linux 2, mac 3", got)
	}
}

type fakePoll struct {
	mu   sync.Mutex
	caps []int
	out  chan struct{} // closed once the first poll is out
}

func (f *fakePoll) GetMessage(ctx context.Context, _ int, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	f.mu.Lock()
	f.caps = append(f.caps, maxCapacity)
	n := len(f.caps)
	f.mu.Unlock()
	if n == 1 {
		close(f.out)
		<-ctx.Done() // a long poll with nothing to say
		return nil, ctx.Err()
	}
	return &scaleset.RunnerScaleSetMessage{MessageID: 7}, nil
}
func (f *fakePoll) DeleteMessage(context.Context, int) error { return nil }
func (f *fakePoll) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}
func (f *fakePoll) Session() scaleset.RunnerScaleSetSession { return scaleset.RunnerScaleSetSession{} }

// A poll that is out when the take changes is given up and sent again with
// the new number; ending the controller still ends the poll with an error.
func TestPollIsSentAgainWhenTheTakeChanges(t *testing.T) {
	cl := &class{retaken: make(chan struct{}, 1)}
	cl.take.Store(2)
	f := &fakePoll{out: make(chan struct{})}
	p := &measuredPoll{Client: f, cl: cl}
	go func() {
		<-f.out
		cl.take.Store(5)
		cl.retaken <- struct{}{}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := p.GetMessage(ctx, 0, 1<<30)
	if err != nil || msg == nil || msg.MessageID != 7 {
		t.Fatalf("message %+v, err %v", msg, err)
	}
	if len(f.caps) != 2 || f.caps[0] != 2 || f.caps[1] != 5 {
		t.Fatalf("polls carried %v, want [2 5]", f.caps)
	}

	f2 := &fakePoll{out: make(chan struct{})}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { <-f2.out; cancel2() }()
	if _, err := (&measuredPoll{Client: f2, cl: cl}).GetMessage(ctx2, 0, 0); err == nil {
		t.Fatal("a poll ended by the controller returned no error")
	}
}

// The class's next job is as large as the largest job GitHub has given it,
// by what is known of that job anywhere: a class that ran only small jobs
// still counts the large one another class measured, and the one of an
// unmeasured job's workflow.
func TestNextSizeCountsJobsTheClassWasGivenButNeverRan(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := overflowController()
	c.ledger = l
	t0 := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	wf := func(f string) string { return "o/r/.github/workflows/" + f + "@refs/heads/main" }
	cl := config.Class{Name: "over", Overflow: true}
	ran := func(name, class, workflow, job string, work int64) {
		must(l.InsertRunner(name, class, cl.Platform(), "h1", t0, size.Estimate{CPUs: 2, MemBytes: 4 << 30}))
		_, err := l.JobStarted(name, ledger.Job{ID: name, Class: class, Repo: "o/r", Workflow: workflow, Name: job}, t0)
		must(err)
		must(l.SetUsage(name, ledger.Usage{CPUSec: 100, MemPeak: work, MemWorkPeak: work}))
		must(l.JobCompleted(name, ledger.Job{ID: name}, "succeeded", t0.Add(100*time.Second)))
		must(l.MarkRemoved(name, t0.Add(101*time.Second)))
	}
	given := func(id, workflow, job string) {
		must(l.UpsertDemand(ledger.Job{ID: id, Class: "over", Repo: "o/r", Workflow: workflow, Name: job}, t0))
	}
	if n := c.nextSize(cl); n.MemBytes != 0 {
		t.Fatalf("nothing given yet: %+v", n)
	}
	ran("r1", "over", wf("ci.yml"), "lint", 1<<30)
	given("d1", wf("ci.yml"), "lint")
	small := c.nextSize(cl).MemBytes
	if small == 0 || small > 2<<30 {
		t.Fatalf("only the small job known: %d", small)
	}
	// Another class measured "gate" at 20G; this class is given it and cannot place it.
	ran("r2", "other", wf("ci.yml"), "gate", 20<<30)
	given("d2", wf("ci.yml"), "gate")
	if n := c.nextSize(cl).MemBytes; n < 20<<30 {
		t.Errorf("a job measured under another class: next is %d, want at least 20G", n)
	}
	// A job nobody ran, of a workflow whose only measured job is 30G.
	ran("r3", "other", wf("big.yml"), "build", 30<<30)
	given("d3", wf("big.yml"), "never-ran")
	if n := c.nextSize(cl).MemBytes; n < 30<<30 {
		t.Errorf("an unmeasured job of a measured workflow: next is %d, want at least 30G", n)
	}
}
