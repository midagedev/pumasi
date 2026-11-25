package controller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

func testBook(t *testing.T) (*Controller, *time.Time, func() []string) {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	out := filepath.Join(dir, "notices")
	c := &Controller{cfg: &config.Config{Notify: config.Notify{Command: "cat >> " + out, Level: "problem"}},
		ledger: l, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.now = func() time.Time { return now }
	c.book = newIncidentBook(c)
	return c, &now, func() []string {
		c.book.flush(context.Background())
		b, _ := os.ReadFile(out)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func TestIncidentOpenRemindResolve(t *testing.T) {
	c, now, sent := testBook(t)
	open := func() {
		c.book.open("host-unreachable/server1", "host-unreachable", "server1", "server1 unreachable for 2m")
	}
	open()
	open() // same key: no second notification
	*now = now.Add(31 * time.Minute)
	open() // reminder
	*now = now.Add(time.Minute)
	c.book.resolve("host-unreachable/server1")
	c.book.resolve("host-unreachable/server1") // nothing open: no-op

	got := sent()
	if len(got) != 3 || !strings.Contains(got[0], `"text":"[pumasi] server1 unreachable for 2m"`) || !strings.Contains(got[0], `"state":"problem"`) ||
		!strings.Contains(got[1], `"state":"reminder"`) || !strings.Contains(got[2], "server1: host unreachable resolved after 32m0s") {
		t.Fatalf("notices:\n%s", strings.Join(got, "\n"))
	}
	if open, _ := c.ledger.OpenIncidents(); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
}

func TestIncidentFlappingIsCollapsed(t *testing.T) {
	c, now, sent := testBook(t)
	for range 8 {
		c.book.open("k", "host-unreachable", "desk1", "desk1 unreachable")
		*now = now.Add(20 * time.Second)
		c.book.resolve("k")
		*now = now.Add(20 * time.Second)
	}
	got := sent()
	flapping := 0
	for _, g := range got {
		if strings.Contains(g, `"state":"flapping"`) {
			flapping++
		}
	}
	// 3 full open/resolve pairs, the 4th open, then one flapping notice and silence.
	if flapping != 1 || len(got) != 8 {
		t.Fatalf("%d notices, %d flapping:\n%s", len(got), flapping, strings.Join(got, "\n"))
	}
}

func TestFailedNotificationIsKeptAndRetried(t *testing.T) {
	c, _, sent := testBook(t)
	good := c.cfg.Notify.Command
	c.cfg.Notify.Command = "exit 3"
	c.book.report("problem", "runner-died/r1", "runner-died", "r1", "runner r1 died", "")
	c.book.flush(context.Background())
	if un, _ := c.ledger.Unsent(); len(un) != 1 {
		t.Fatalf("unsent %+v", un)
	}
	c.cfg.Notify.Command = good
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], "runner r1 died") {
		t.Fatalf("after retry: %v", got)
	}
	if un, _ := c.ledger.Unsent(); len(un) != 0 {
		t.Fatalf("still unsent %+v", un)
	}
}

func TestLevelFiltersSelfHealedAndInfo(t *testing.T) {
	c, _, sent := testBook(t)
	c.book.report("recovered", "a", "agent-restarted", "server1", "agent restarted", "")
	c.book.report("info", "b", "note", "x", "note", "")
	if got := sent(); len(got) != 1 || got[0] != "" {
		t.Fatalf("level problem let through: %v", got)
	}
}

func TestEndCauseSeparatesOurFailuresFromTheJobsOwn(t *testing.T) {
	code := func(n int) *api.Runner { return &api.Runner{ExitCode: &n} }
	busy := ledger.Runner{Name: "r", State: "busy"}
	reported := ledger.Runner{Name: "r", State: "busy", Result: "failed"}
	oom := ledger.Runner{Name: "r", State: "busy", OOMKills: 1}
	for _, tc := range []struct {
		name   string
		before ledger.Runner
		reason string
		final  *api.Runner
		want   string
	}{
		{"killed from outside", busy, "runner exited during or after its job (code 137)", code(137), endDied},
		{"container gone", busy, "container missing on server1", nil, endDied},
		{"host rebooted", busy, rebootReason, code(255), endRebooted},
		{"job finished, runner exit 0", busy, "runner exited during or after its job (code 0)", code(0), endNormal},
		{"GitHub already reported the job", reported, "runner exited during or after its job (code 1)", code(1), endNormal},
		{"memory limit", oom, "runner exited during or after its job (code 137)", code(137), endOOM},
		{"memory limit seen only at the end", busy, "runner exited during or after its job (code 137)", &api.Runner{ExitCode: code(137).ExitCode, OOMKills: 1}, endOOM},
		// The controller's own stop of a running container has no exit code and must not count as a death.
		{"maxDuration", busy, decide.OverMaxDuration + "1h0m0s", &api.Runner{}, endMaxDuration},
		{"idle timeout", ledger.Runner{Name: "r", State: "idle"}, "idle timeout: no job 5m0s after listening", &api.Runner{}, endNormal},
	} {
		if got := endCause(tc.before, decide.Stop{Runner: "r", Reason: tc.reason}, tc.final); got != tc.want {
			t.Errorf("%s: cause %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestThreeProvisionFailuresQuarantineUntilTheCheckPasses(t *testing.T) {
	c, _, sent := testBook(t)
	h := &host{cfg: config.Host{Name: "server1"}}
	c.hosts = []*host{h}
	c.provisionFailed(h, "docker run: no space left")
	c.provisionFailed(h, "docker run: no space left")
	if !h.quarantined.IsZero() {
		t.Fatal("quarantined after two failures")
	}
	c.provisionOK(h) // a runner came up in between: the count starts over
	c.provisionFailed(h, "x")
	c.provisionFailed(h, "x")
	if !h.quarantined.IsZero() {
		t.Fatal("quarantined although a runner came up in between")
	}
	c.provisionFailed(h, "provision timeout: not listening 3m0s after start")
	c.provisionFailed(h, "again") // more failures while quarantined do not open it twice
	if h.quarantined.IsZero() || c.closedReason(h) != "quarantined" {
		t.Fatal("not quarantined after three failures in a row")
	}
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], "server1 quarantined: 3 runners in a row") {
		t.Fatalf("notices %q", got)
	}
}

func TestDiskBelowThresholdClosesHost(t *testing.T) {
	c, _, sent := testBook(t)
	c.cfg.DiskMinFree = 10
	h := &host{cfg: config.Host{Name: "server1"}, snap: &api.Snapshot{DiskFree: 4 << 30, DiskTotal: 100 << 30}}
	if got := c.closedReason(h); got != "disk 4% free" {
		t.Fatalf("closed %q", got)
	}
	h.snap = &api.Snapshot{DiskFree: 40 << 30, DiskTotal: 100 << 30}
	if got := c.closedReason(h); got != "" {
		t.Fatalf("closed %q with 40%% free", got)
	}
	h.snap = &api.Snapshot{} // unknown disk: never a reason to close
	if got := c.closedReason(h); got != "" {
		t.Fatalf("closed %q with unknown disk", got)
	}
	got := sent()
	if len(got) != 2 || !strings.Contains(got[0], "4.0G free on the disk that holds its runners") || !strings.Contains(got[1], "resolved") {
		t.Fatalf("notices %q", got)
	}
}
