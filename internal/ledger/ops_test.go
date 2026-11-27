package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

// What operators set outlives the controller: it is in the ledger, with who
// set it and when, and what it came to.
func TestOpsAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := l.AddExpedite(Expedite{Repo: "o/r", RunID: 7, Job: "hotfix", Class: "small", By: "cli a@127.0.0.1", At: t0, QueuedAt: t0.Add(-time.Minute)})
	must(err)
	b, err := l.AddExpedite(Expedite{Repo: "o/r", RunID: 8, Job: "deploy", GHJobID: 99, By: "api 10.0.0.3", At: t0.Add(time.Second)})
	must(err)
	r1, err := l.AddRule(`{"match":{"class":"small"},"level":"high"}`, "cli", t0, time.Time{})
	must(err)
	r2, err := l.AddRule(`{"match":{"event":"schedule"},"level":"low"}`, "cli", t0, t0.Add(time.Hour))
	must(err)
	must(l.SetSetting("mode", `{"name":"high-only"}`, t0))
	l.Close()

	l, err = Open(path) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	act, err := l.ActiveExpedites()
	must(err)
	if len(act) != 2 || act[0].ID != a.ID || act[1].GHJobID != 99 || act[0].By != "cli a@127.0.0.1" || !act[0].QueuedAt.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("active %+v", act)
	}
	must(l.EndExpedite(a.ID, "started", t0.Add(2*time.Minute), t0.Add(90*time.Second), "small-h1-x"))
	must(l.EndExpedite(a.ID, "cleared", t0.Add(3*time.Minute), time.Time{}, "")) // over once only
	act, _ = l.ActiveExpedites()
	rec, _ := l.RecentExpedites(5)
	if len(act) != 1 || act[0].ID != b.ID || len(rec) != 1 || rec[0].Ended != "started" || rec[0].StartedAt.Sub(rec[0].At) != 90*time.Second {
		t.Fatalf("after the start: active %+v recent %+v", act, rec)
	}

	rs, _ := l.Rules(t0.Add(time.Minute))
	if len(rs) != 2 || RuleID(rs[0].ID) != "r-1" || rs[1].Expires.IsZero() {
		t.Fatalf("rules %+v", rs)
	}
	if rs, _ := l.Rules(t0.Add(2 * time.Hour)); len(rs) != 1 {
		t.Errorf("an hour-long rule after two hours: %+v", rs)
	}
	if ok, _ := l.ReplaceRule(r2.ID, `{"match":{"event":"schedule"},"level":"normal"}`, "cli", t0.Add(2*time.Hour), time.Time{}); ok {
		t.Error("a rule out of time was changed")
	}
	if ok, _ := l.RemoveRule(r1.ID, "cli", t0.Add(time.Minute)); !ok {
		t.Error("remove r-1")
	}
	if ok, _ := l.RemoveRule(r1.ID, "cli", t0.Add(time.Minute)); ok {
		t.Error("removed twice")
	}
	if n, err := ParseRuleID("r-12"); err != nil || n != 12 {
		t.Errorf("ParseRuleID: %d %v", n, err)
	}
	if _, err := ParseRuleID("file-1"); err == nil {
		t.Error("file-1 read as a runtime rule")
	}
	if v, at, ok := l.Setting("mode"); !ok || v != `{"name":"high-only"}` || !at.Equal(t0) {
		t.Errorf("setting %q %s %v", v, at, ok)
	}
}
