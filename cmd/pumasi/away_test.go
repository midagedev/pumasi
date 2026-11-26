package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

type awayRig struct {
	*awayWatch
	now  time.Time
	sent []awayNotice
	to   []string
	fail bool
}

func newAwayRig(t *testing.T, path string) *awayRig {
	t.Helper()
	r := &awayRig{now: time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)}
	r.awayWatch = newAwayWatch(path, "host1", "10.0.0.5:7433", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.awayWatch.now = func() time.Time { return r.now }
	if time.Since(r.st.Since) < time.Minute {
		r.st.Since = r.now // a fresh agent: not connected since it started, by the test's clock
	}
	r.post = func(_ context.Context, url string, n awayNotice) error {
		if r.fail {
			return errors.New("no route to host")
		}
		r.sent, r.to = append(r.sent, n), append(r.to, url)
		return nil
	}
	return r
}

func (r *awayRig) after(d time.Duration) {
	r.now = r.now.Add(d)
	r.tick(context.Background())
}

func TestHostSaysOnceThatTheControllerIsAwayAndOnceThatItIsBack(t *testing.T) {
	r := newAwayRig(t, filepath.Join(t.TempDir(), "away.json"))
	r.Link(true, "")
	r.Set(api.Away{Webhook: "https://hooks.example/a", AfterSec: 300, Rank: 1, Controller: "ctl", Classes: []string{"build", "deploy"}})
	r.after(time.Hour)
	if len(r.sent) != 0 {
		t.Fatalf("connected, yet it spoke: %+v", r.sent)
	}

	r.Link(false, "connection refused")
	r.after(4 * time.Minute)
	r.Link(false, "no answer (timeout)")
	if len(r.sent) != 0 {
		t.Fatalf("spoke before 5m: %+v", r.sent)
	}
	r.after(time.Minute)
	if len(r.sent) != 1 || r.sent[0].State != "problem" || r.sent[0].Key != "controller-away/host1" {
		t.Fatalf("after 5m: %+v", r.sent)
	}
	for _, want := range []string{"host1 has not reached the pumasi controller ctl at 10.0.0.5:7433 for 5m0s", "no answer (timeout)", "new jobs of build, deploy wait", "running jobs go on"} {
		if !strings.Contains(r.sent[0].Text, want) {
			t.Fatalf("missing %q in %q", want, r.sent[0].Text)
		}
	}
	for range 30 {
		r.after(10 * time.Minute)
	}
	if len(r.sent) != 1 {
		t.Fatalf("said it again: %d notices", len(r.sent))
	}

	// Back. The controller meanwhile names another host: the resolve still
	// goes where the alert went.
	r.Link(true, "")
	r.Set(api.Away{})
	r.fail = true
	r.after(10 * time.Second)
	r.fail = false
	r.after(10 * time.Second)
	r.after(10 * time.Second)
	if len(r.sent) != 2 || r.sent[1].State != "resolved" || r.to[1] != "https://hooks.example/a" || !strings.Contains(r.sent[1].Text, "again; it was out of reach for 5h5m20s") {
		t.Fatalf("back: %+v to %v", r.sent, r.to)
	}
}

func TestSecondHostWaitsTwiceAsLong(t *testing.T) {
	r := newAwayRig(t, "")
	r.Set(api.Away{Webhook: "https://hooks.example/a", AfterSec: 300, Rank: 2})
	r.Link(false, "connection refused")
	r.after(9 * time.Minute)
	if len(r.sent) != 0 {
		t.Fatalf("the second host spoke before 10m: %+v", r.sent)
	}
	r.after(time.Minute)
	if len(r.sent) != 1 || !strings.Contains(r.sent[0].Text, "(host 2 to say so)") {
		t.Fatalf("after 10m: %+v", r.sent)
	}
}

func TestHostNotAskedSaysNothing(t *testing.T) {
	r := newAwayRig(t, "")
	r.Set(api.Away{})
	r.Link(false, "connection refused")
	r.after(24 * time.Hour)
	if len(r.sent) != 0 {
		t.Fatalf("%+v", r.sent)
	}
}

// The host's own network is down: nothing can be told. If the controller is
// back before anything was, nothing is told at all.
func TestAwayThatNobodyHeardIsNotResolved(t *testing.T) {
	r := newAwayRig(t, "")
	r.Set(api.Away{Webhook: "https://hooks.example/a", AfterSec: 300, Rank: 1})
	r.Link(false, "network is unreachable")
	r.fail = true
	r.after(6 * time.Minute)
	r.after(time.Minute)
	r.fail = false
	r.Link(true, "")
	r.after(time.Minute)
	if len(r.sent) != 0 {
		t.Fatalf("%+v", r.sent)
	}
	// Had the network come back first, it would have been told then.
	r.Link(false, "connection refused")
	r.fail = true
	r.after(6 * time.Minute)
	r.fail = false
	r.after(10 * time.Second)
	if len(r.sent) != 1 || r.sent[0].State != "problem" {
		t.Fatalf("%+v", r.sent)
	}
}

// An agent that restarts while the controller is away knows since when, does
// not say it twice, and still owes the "back".
func TestAwaySurvivesAnAgentRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "away.json")
	r := newAwayRig(t, path)
	r.Link(true, "")
	r.Set(api.Away{Webhook: "https://hooks.example/a", AfterSec: 300, Rank: 1})
	r.Link(false, "connection refused")
	r.after(3 * time.Minute)

	r2 := newAwayRig(t, path)
	r2.now = r.now
	r2.Link(false, "connection refused")
	r2.after(2 * time.Minute)
	if len(r2.sent) != 1 {
		t.Fatalf("3m before the restart and 2m after it are 5m: %+v", r2.sent)
	}
	if st, _ := os.Stat(path); st == nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the file holds the webhook: %v", st)
	}

	r3 := newAwayRig(t, path)
	r3.now = r2.now
	r3.Link(false, "connection refused")
	r3.after(time.Hour)
	if len(r3.sent) != 0 {
		t.Fatalf("said again after a restart: %+v", r3.sent)
	}
	r3.Link(true, "")
	r3.after(10 * time.Second)
	if len(r3.sent) != 1 || r3.sent[0].State != "resolved" {
		t.Fatalf("back after a restart: %+v", r3.sent)
	}

	// A host that leaves writes nothing more.
	r3.Stop()
	_ = os.Remove(path)
	r3.Link(false, "x")
	r3.Set(api.Away{})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("written after Stop")
	}
}
