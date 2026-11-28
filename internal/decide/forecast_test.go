package decide

import (
	"testing"
	"time"
)

// one host with room for one 4-cpu job: 6 cores, 2 used by the rest of the machine.
func small() Host {
	return Host{Name: "box", Kind: "server", OS: "linux", Up: true, CPUs: 6, MemBytes: 20 * gb, NonRunnerCPU: 2, NonRunnerMem: 2 * gb, KeepMem: 2 * gb}
}

func job(id string, at time.Duration) Demand {
	return Demand{ID: id, Name: id, Class: "linux-s", FirstSeen: t0.Add(at), CPUs: 4, MemBytes: 4 * gb}
}

func running(name string, started time.Time) Runner {
	return Runner{Name: name, Class: "linux-s", Host: "box", State: "busy", JobKnown: true, Working: true, Container: "running",
		CreatedAt: started, ListeningAt: started, JobStartedAt: started, CPUs: 4, MemBytes: 4 * gb}
}

const prov = 10 * time.Second

// The box is full until e2e ends in 2m; the next job starts there then, and
// the one after when that one has run its 5m.
func TestForecastWaitsForTheEndThatMakesRoom(t *testing.T) {
	s := snap([]Host{small()}, []Demand{job("a", 0), job("b", time.Second)}, []Runner{running("r1", t0)})
	busy := map[string]Busy{"r1": {Ends: s.Now.Add(2 * time.Minute), Job: "e2e"}}
	runs := map[string]time.Duration{"a": 5 * time.Minute, "b": time.Minute}
	got := Forecast(s, busy, runs, prov)
	if a := got["a"]; a.In != 2*time.Minute+prov || a.Host != "box" || a.After != "e2e" {
		t.Fatalf("a: %+v", a)
	}
	if b := got["b"]; b.In != 2*time.Minute+prov+5*time.Minute+prov || b.After != "a" {
		t.Fatalf("b: %+v", b)
	}
}

// Room now: a runner starts this round, and the job has it once it is up.
func TestForecastWithRoomNow(t *testing.T) {
	s := snap([]Host{small()}, []Demand{job("a", 0)}, nil)
	if a := Forecast(s, nil, nil, prov)["a"]; a.In != prov || a.Host != "box" || a.After != "" {
		t.Fatalf("a: %+v", a)
	}
}

// A runner of the class coming up takes the oldest job when it is up.
func TestForecastRunnerComingUp(t *testing.T) {
	up := Runner{Name: "r2", Class: "linux-s", Host: "box", State: "starting", Container: "running", CreatedAt: t0.Add(55 * time.Second), CPUs: 4, MemBytes: 4 * gb}
	s := snap([]Host{small()}, []Demand{job("a", 0)}, []Runner{up})
	if a, ok := Forecast(s, nil, nil, prov)["a"]; !ok || a.In != 5*time.Second || a.Host != "box" {
		t.Fatalf("a: %+v %v", a, ok)
	}
}

// No time where nothing that ends opens the room: a job whose end nobody
// knows, a closed host, a job past the horizon.
func TestForecastSaysNothingItCannotKnow(t *testing.T) {
	s := snap([]Host{small()}, []Demand{job("a", 0)}, []Runner{running("r1", t0)})
	if got := Forecast(s, map[string]Busy{"r1": {Job: "e2e"}}, nil, prov); len(got) != 0 {
		t.Fatalf("end unknown: %+v", got)
	}
	closed := small()
	closed.Closed = "in use"
	s = snap([]Host{closed}, []Demand{job("a", 0)}, nil)
	if got := Forecast(s, nil, nil, prov); len(got) != 0 {
		t.Fatalf("closed host: %+v", got)
	}
	s = snap([]Host{small()}, []Demand{job("a", 0)}, []Runner{running("r1", t0)})
	if got := Forecast(s, map[string]Busy{"r1": {Ends: s.Now.Add(3 * time.Hour)}}, nil, prov); len(got) != 0 {
		t.Fatalf("past the horizon: %+v", got)
	}
}

// Cpu pressure that keeps the box shut eases when a job there ends.
func TestForecastPressureEasesWhenAJobEnds(t *testing.T) {
	h := small()
	h.CPUs, h.PSICPU = 12, 50
	s := snap([]Host{h}, []Demand{job("a", 0)}, []Runner{running("r1", t0)})
	busy := map[string]Busy{"r1": {Ends: s.Now.Add(time.Minute), Job: "build"}}
	if a := Forecast(s, busy, nil, prov)["a"]; a.In != time.Minute+prov || a.After != "build" {
		t.Fatalf("a: %+v", a)
	}
}

func TestEndOf(t *testing.T) {
	now := t0.Add(10 * time.Minute)
	if e := EndOf(t0.Add(8*time.Minute), now, 5*time.Minute); !e.Equal(t0.Add(13 * time.Minute)) {
		t.Fatalf("on time: %v", e.Sub(t0))
	}
	// Late: a tenth of its runtime from now, at least 15s.
	if e := EndOf(t0, now, 5*time.Minute); !e.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("late: %v", e.Sub(now))
	}
	if e := EndOf(t0, now, time.Minute); !e.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("late, short: %v", e.Sub(now))
	}
}

// A runner coming up on a host that is unreachable gives its job no time.
func TestForecastRunnerOnAHostThatIsDown(t *testing.T) {
	h := small()
	h.Up, h.DownFor = false, time.Minute
	upr := Runner{Name: "r2", Class: "linux-s", Host: "box", State: "starting", Container: "running", CreatedAt: t0, CPUs: 4, MemBytes: 4 * gb}
	s := snap([]Host{h}, []Demand{job("a", 0)}, []Runner{upr})
	if got := Forecast(s, nil, nil, prov); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
