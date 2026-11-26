package decide

import (
	"strings"
	"testing"
)

// Jobs nobody has measured are not put side by side on a guess: a host takes
// one on the guess alone, and the next only with the hold free.
func TestBlindJobsAreNotPlacedSideBySide(t *testing.T) {
	jvm := Class{Name: "build", CPUs: 2, MemBytes: 4 * gb}
	host := Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb, KeepMem: 3 * gb} // 29G to lend
	blind := func(n int) []Demand {
		ds := demand("build", n)
		for i := range ds {
			ds[i].Blind = true
		}
		return ds
	}
	mk := func(hosts []Host, d []Demand, rs []Runner, hold float64) Plan {
		s := snap(hosts, d, rs)
		s.Classes, s.BlindHold = []Class{jvm}, hold
		return Decide(s)
	}

	// 8 jobs at 4G would all fit (32G > 29G: 7 of them); held at 12G each, two do.
	p := mk([]Host{host}, blind(8), nil, 3)
	if len(p.Start) != 2 {
		t.Fatalf("blind jobs started at once = %d, want 2 (12G held each of 29G)", len(p.Start))
	}
	if !strings.Contains(p.Start[0].Reason, "holds 12.0G while it is watched") || p.Start[0].MemBytes != 4*gb {
		t.Errorf("start: %+v", p.Start[0])
	}
	if len(p.Defer) != 1 || p.Defer[0].Count != 6 || !strings.Contains(p.Defer[0].Reason, "watches 2 unmeasured job") {
		t.Errorf("defer: %+v", p.Defer)
	}
	// Measured jobs of the same size are placed as before.
	if p := mk([]Host{host}, demand("build", 8), nil, 3); len(p.Start) != 7 {
		t.Errorf("measured jobs started = %d, want 7", len(p.Start))
	}
	// Without the hold nothing changes.
	if p := mk([]Host{host}, blind(8), nil, 0); len(p.Start) != 7 {
		t.Errorf("hold off: started = %d, want 7", len(p.Start))
	}

	// A host too small for the hold still takes one blind job: it must get measured somewhere.
	small := Host{Name: "h2", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb, KeepMem: 1 * gb}
	if p := mk([]Host{small}, blind(3), nil, 3); len(p.Start) != 1 {
		t.Errorf("small host: started = %d, want 1", len(p.Start))
	}
	// And none while it watches one (the runner's charge already is the hold).
	watching := []Runner{{Name: "r1", Class: "build", Host: "h2", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 12 * gb, Watched: true}}
	if p := mk([]Host{small}, blind(1), watching, 3); len(p.Start) != 0 {
		t.Errorf("small host that watches one: started = %d, want 0", len(p.Start))
	}
	// A measured job is not held back by a watched one where its size is free.
	big := []Runner{{Name: "r1", Class: "build", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 12 * gb, Watched: true}}
	if p := mk([]Host{host}, demand("build", 2), big, 3); len(p.Start) != 2 {
		t.Errorf("measured next to a watched one: started = %d, want 2", len(p.Start))
	}
	// Once the watched job has shown its size (9G, no longer watched) the next blind job is the first again.
	shown := []Runner{{Name: "r1", Class: "build", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true, CPUs: 2, MemBytes: 9 * gb}}
	if p := mk([]Host{host}, blind(4), shown, 3); len(p.Start) != 1 {
		// 29-9 = 20G: the first takes 4G and holds 12G, the next would need 12G of the 8G left.
		t.Errorf("after the first has shown its size: started = %d, want 1", len(p.Start))
	}
}
