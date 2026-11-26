package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/midagedev/pumasi/internal/api"
)

// The invariant of {slot}: no two runners of a class that the agent knows
// hold the same number, whatever the order of starts and removals, and a
// restart of the agent changes nobody's number.
func TestNoTwoLiveRunnersOfAClassShareASlot(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "runners.json")
	open := func(drv Driver) *Agent {
		a := New(Config{Host: "h1", StateFile: state}, drv, &CgroupMeter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		a.load()
		return a
	}
	drv := &fakeDriver{cs: map[string]Container{}}
	a := open(drv)
	check := func(when string) map[string]int {
		t.Helper()
		got, seen := map[string]int{}, map[string]string{}
		for _, r := range a.Snapshot().Runners {
			key := fmt.Sprintf("%s/%d", r.Class, r.Slot)
			if other, dup := seen[key]; dup || r.Slot < 1 {
				t.Fatalf("%s: %s and %s both hold %s", when, r.Name, other, key)
			}
			seen[key], got[r.Name] = r.Name, r.Slot
		}
		return got
	}
	start := func(name, class string) int {
		t.Helper()
		r, err := a.Start(ctx, api.StartRequest{Name: name, Class: class, CPUs: 1, MemBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		return r.Slot
	}

	// Lowest free number first, per class.
	if s := []int{start("a1", "a"), start("a2", "a"), start("b1", "b"), start("a3", "a")}; !slices.Equal(s, []int{1, 2, 1, 3}) {
		t.Fatalf("slots %v", s)
	}
	// A removed runner's number is the next one given.
	if _, err := a.Delete(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if s := start("a4", "a"); s != 2 {
		t.Fatalf("reused slot %d, want 2", s)
	}
	before := check("after reuse")

	// The agent restarts: the runners keep their numbers and the next one fits around them.
	a = open(drv)
	if after := check("after restart"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("restart changed slots: %v -> %v", before, after)
	}
	if s := start("a5", "a"); s != 4 {
		t.Fatalf("after restart slot %d, want 4", s)
	}

	// Starts and removals from many sides at once.
	rng := rand.New(rand.NewSource(1))
	var mu sync.Mutex
	var wg sync.WaitGroup
	live := []string{}
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			remove := len(live) > 3 && rng.Intn(3) == 0
			var name string
			if remove {
				j := rng.Intn(len(live))
				name, live = live[j], slices.Delete(live, j, j+1)
			}
			mu.Unlock()
			if remove {
				_, _ = a.Delete(ctx, name)
				return
			}
			name = fmt.Sprintf("c%d", i)
			if _, err := a.Start(ctx, api.StartRequest{Name: name, Class: []string{"a", "b"}[i%2], CPUs: 1, MemBytes: 1 << 20}); err == nil {
				mu.Lock()
				live = append(live, name)
				mu.Unlock()
			}
			check("in the crowd")
		}()
	}
	wg.Wait()
	check("after the crowd")
}

func TestSlotInEnvAndPath(t *testing.T) {
	s := Spec{Class: "build", Slot: 3, Path: []string{"/srv/ci/{slot}/bin"},
		Env: map[string]string{"HOME": "/srv/ci-home/{slot}", "GRADLE_USER_HOME": "/srv/ci-home/{slot}/.gradle", "TMP": "{runner}/tmp", "TAG": "worker-{slot}"}}
	env := classEnv([]string{"PATH=/usr/bin"}, s, "/r/x")
	for _, want := range []string{"HOME=/srv/ci-home/3", "GRADLE_USER_HOME=/srv/ci-home/3/.gradle", "TMP=/r/x/tmp", "TAG=worker-3", "PATH=/srv/ci/3/bin:/usr/bin"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s: %v", want, env)
		}
	}
	// Made before the runner starts: the element that carries the number, once, and nothing below it.
	if got := s.SlotDirs(); !slices.Equal(got, []string{"/srv/ci-home/3", "/srv/ci/3"}) {
		t.Errorf("slot dirs %v", got)
	}
	// No slot given (an agent older than the controller): the text stays, nothing is made.
	s.Slot = 0
	if env := classEnv(nil, s, "/r/x"); !slices.Contains(env, "HOME=/srv/ci-home/{slot}") || len(s.SlotDirs()) != 0 {
		t.Errorf("without a slot: %v %v", env, s.SlotDirs())
	}
}
