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

	"github.com/midagedev/pumasi/internal/config"
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
	open := func() { c.book.open("host-unreachable/server1", "host-unreachable", "server1", "server1 unreachable for 2m") }
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
