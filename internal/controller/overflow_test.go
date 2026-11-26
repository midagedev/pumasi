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
