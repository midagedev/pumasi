package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

func ptr[T any](v T) *T { return &v }

func findOf(fs []Finding, kind string) (Finding, bool) {
	for _, f := range fs {
		if f.Kind == kind {
			return f, true
		}
	}
	return Finding{}, false
}

func runs(f Finding) string {
	var out []string
	for _, s := range f.Next {
		out = append(out, s.Run+s.Config)
	}
	return strings.Join(out, " | ")
}

var explainNow = time.Date(2026, 11, 27, 7, 30, 0, 0, time.UTC)

// A class waits for memory: the report names the host and how much it lacks.
func TestExplainSaysHowMuchRoomIsShort(t *testing.T) {
	s := State{Controller: true,
		Hosts: []HostState{{Name: "big-server", Kind: "server", OS: "linux", Reachable: ptr(true), CPUs: 32, MemBytes: 128 << 30,
			BudgetMem: 110 << 30, FreeCPUs: 20.3, FreeMem: 14 * gb / 10}, {Name: "desk", Kind: "desktop", Reachable: ptr(true), Closed: "in use"}},
		Waiting:     []WaitingState{{Class: "gate", Jobs: 1, Order: 1, OldestSeconds: 483, Reason: "no host with 7.5cpu/30.8G free (big-server 20.3cpu/1.4G, desk in use)"}},
		WaitingJobs: []JobWait{{Class: "gate", Job: "integration", WaitedSeconds: 483, Position: 1}},
		Classes:     []ClassState{{Name: "gate", Hosts: []string{"big-server"}}}}
	fs := Explain(s, nil, explainNow)
	f, ok := findOf(fs, "waiting")
	if !ok {
		t.Fatalf("no waiting finding in %+v", fs)
	}
	for _, want := range []string{"gate: 1 job wait", "the oldest 8m03s", "needs 7.5cpu/30.8G", "big-server has 20.3cpu/1.4G free, 29.4G memory short", "desk in use"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("text %q lacks %q", f.Text, want)
		}
	}
	if f.Level != "wait" || f.Class != "gate" || f.Job != "integration" {
		t.Errorf("finding %+v", f)
	}
	for _, want := range []string{"pumasi status", "pumasi priority", "priority", "budget", "hosts", "pumasi invite"} {
		if !strings.Contains(runs(f), want) {
			t.Errorf("steps %q lack %q", runs(f), want)
		}
	}
	var b bytes.Buffer
	WriteExplain(&b, s, fs, nil, "https://github.com/o/r", false, explainNow)
	for _, want := range []string{"ATTENTION", "LINE", "#1 integration 8m03s", "HOSTS", "RUNNERS", "NOTES", "RESOLVED", "desk is in use: people first"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "MODE") {
		t.Error("a source without priority controls has no MODE section")
	}
}

// A host that does not answer: run `pumasi doctor` there.
func TestExplainUnreachableHostSaysDoctor(t *testing.T) {
	s := State{Controller: true, Hosts: []HostState{{Name: "spare", Reachable: ptr(false), SeenSeconds: ptr(int64(95)), LastDrop: "keepalive timeout", Runners: 2}}}
	f, ok := findOf(Explain(s, nil, explainNow), "host")
	if !ok || f.Level != "problem" || !strings.Contains(f.Text, "spare does not answer (last answer 1m35s ago): keepalive timeout") {
		t.Fatalf("finding %+v", f)
	}
	if f.Next[0].Run != "pumasi doctor" || f.Next[0].Where != "on spare" {
		t.Errorf("first step %+v, want pumasi doctor on spare", f.Next[0])
	}
	// Once the incident is open it speaks for the host; no second finding.
	s.Incidents = []ledger.Incident{{Key: "host-unreachable/spare", Kind: "host-unreachable", Subject: "spare", Detail: "spare unreachable for 2m0s"}}
	fs := Explain(s, nil, explainNow)
	if _, dup := findOf(fs, "host"); dup || len(fs) != 1 || fs[0].Next[0].Run != "pumasi doctor" {
		t.Errorf("findings %+v", fs)
	}
}

func TestExplainOOMAndNoController(t *testing.T) {
	s := State{Incidents: []ledger.Incident{{Kind: "out-of-memory", Subject: "e2e-h-1", Action: "none: raise or drop the class's limit, or fix the job; not rerun",
		Detail: "e2e (firefox) hit class e2e's memory limit (20.0G) on h; the kernel killed a process of the job"}},
		Runners: []RunnerState{{Name: "e2e-h-1", Class: "e2e", Host: "h", Job: "e2e (firefox)"}}}
	fs := Explain(s, nil, explainNow)
	if fs[0].Kind != "controller" || fs[0].Next[0].Run != "pumasi up" {
		t.Errorf("without a controller the first finding says so: %+v", fs[0])
	}
	f, _ := findOf(fs, "out-of-memory")
	if f.Class != "e2e" || f.Host != "h" || f.Runner != "e2e-h-1" || !strings.Contains(f.Text, "[pumasi: none: raise") || !strings.Contains(runs(f), "limit") {
		t.Errorf("oom finding %+v", f)
	}
}

func TestExplainWarning(t *testing.T) {
	s := State{Controller: true, Warnings: []string{"warning: jobs of class mac on desk1 can read this controller's GitHub token and keys"}}
	f, ok := findOf(Explain(s, nil, explainNow), "warning")
	if !ok || f.Level != "problem" || !strings.Contains(runs(f), "jobsMayReadCredentials: true") {
		t.Errorf("finding %+v", f)
	}
}

func TestExplainPriorityView(t *testing.T) {
	until := explainNow.Add(25 * time.Minute)
	pv := &PriorityView{Mode: Mode{Name: "high-only", Until: &until, By: "web 127.0.0.1"},
		Expedited: []Expedited{{Job: "job-7", Name: "deploy-api", Class: "deploy", Position: 1, Since: explainNow.Add(-time.Minute), By: "web 127.0.0.1"},
			// GitHub still holds it: its name for --clear is the line's.
			{Name: "e2e", Repo: "o/r", RunID: 5, Class: "spill", Position: 3, Ahead: 1, Pending: true, Since: explainNow.Add(-2 * time.Minute), By: "cli 127.0.0.1"}},
		Line: []LineJob{{Job: "x-4", Name: "e2e", Repo: "o/r", RunID: 5, Class: "spill", Position: 3, Pending: true}}}
	s := State{Controller: true}
	fs := Explain(s, pv, explainNow)
	if _, ok := findOf(fs, "mode"); !ok {
		t.Error("no mode finding")
	}
	var exp []Finding
	for _, f := range fs {
		if f.Kind == "expedited" {
			exp = append(exp, f)
		}
	}
	if len(exp) != 2 || exp[0].Subject != "job-7" || !strings.Contains(exp[0].Text, "expedited by web 127.0.0.1: place 1 in line;") ||
		exp[1].Subject != "x-4" || exp[1].Next[0].Run != "pumasi expedite --clear x-4" ||
		!strings.Contains(exp[1].Text, "place 3 in line, 1 older job of its class goes first") || !strings.Contains(exp[1].Text, "GitHub still holds it") {
		t.Errorf("expedited findings %+v", exp)
	}
	var b bytes.Buffer
	WriteExplain(&b, s, fs, pv, "", true, explainNow)
	for _, want := range []string{"MODE high-only until", "(set by web 127.0.0.1)", "EXPEDITED", "job-7  deploy-api (deploy)  place 1 in line; by web 127.0.0.1, since 1m00s", "x-4  e2e (spill)", "DEMO"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
}

// Every step names a command pumasi has or a key its config reads: nothing made up.
func TestExplainStepsAreRealCommandsAndKeys(t *testing.T) {
	keys := map[string]bool{}
	for _, v := range []any{config.Config{}, config.Class{}, config.Host{}} {
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			if tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("yaml"), ","); tag != "" && tag != "-" {
				keys[tag] = true
			}
		}
	}
	commands := map[string]bool{"status": true, "priority": true, "doctor": true, "invite": true, "leave": true, "up": true, "service": true, "open": true,
		"expedite": true, "mode": true}

	var states []State
	d := NewDemo(1)
	for at := 0.0; at < 1200; at += 30 {
		states = append(states, d.view(at, explainNow).state())
	}
	every := State{Controller: false, Warnings: []string{"w"}, Hosts: []HostState{{Name: "h", Reachable: ptr(true), Closed: "quarantined"},
		{Name: "g", Reachable: ptr(true), Closed: "disk 4% free"}, {Name: "k", Reachable: ptr(false)}},
		Classes: []ClassState{{Name: "c", Hosts: []string{"h"}, LimitMem: 1 << 30}},
		Waiting: []WaitingState{{Class: "c", Jobs: 1, Reason: "no host with 1.0cpu/2.0G free (h 0.5cpu/1.0G, k down, g reserved for d)"},
			{Class: "m", Jobs: 1, Reason: "no host has /srv/cache"}, {Class: "b", Jobs: 1, Reason: "behind an older job of its class that waits for room"}},
		Overflow: []Overflow{{Class: "o", After: "c"}}, TakenBack: []JobWait{{Class: "o"}},
		Runners: []RunnerState{{Name: "r", Class: "c", Host: "h", MemBytes: 95, MemLimit: 100}, {Name: "q", Class: "c", Host: "h", MemBytes: 5, EstMem: 4}}}
	for _, kind := range []string{"host-unreachable", "agent-down", "host-quarantined", "disk-low", "out-of-memory", "job-waiting", "runner-died",
		"host-rebooted", "github-auth", "github-unreachable", "leftover", "max-duration", "agent-version", "ledger-rebuilt", "rerun"} {
		every.Incidents = append(every.Incidents, ledger.Incident{Kind: kind, Subject: "h", Detail: "x hit class c's memory limit"})
	}
	states = append(states, every)
	until := explainNow.Add(time.Hour)
	pv := &PriorityView{Mode: Mode{Name: "high-only", Until: &until}, Expedited: []Expedited{{Job: "j"}}}
	n := 0
	for _, s := range states {
		for _, f := range Explain(s, pv, explainNow) {
			for _, st := range f.Next {
				n++
				switch {
				case st.Run != "" && st.Config != "":
					t.Errorf("a step is a command or a key, not both: %+v", st)
				case strings.HasPrefix(st.Run, "pumasi "):
					if sub := strings.Fields(st.Run)[1]; !commands[sub] {
						t.Errorf("no such command: %q", st.Run)
					}
				case strings.HasPrefix(st.Run, "curl "):
					if !strings.Contains(st.Run, "<dashboard>/api/") {
						t.Errorf("curl of something not the dashboard: %q", st.Run)
					}
				case st.Run != "":
					t.Errorf("not a pumasi command: %q", st.Run)
				default:
					key := strings.FieldsFunc(st.Config, func(r rune) bool { return r == ':' || r == ' ' })[0]
					if !keys[key] {
						t.Errorf("no such config key: %q", st.Config)
					}
				}
			}
		}
	}
	if n < 40 {
		t.Errorf("only %d steps checked", n)
	}
}

// The real controller's state gives each waiting job its level and place in line.
func TestStateGivesWaitingJobsTheirPlace(t *testing.T) {
	l, err := ledger.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	add := func(id, class, wf, name, event string, ago time.Duration) {
		if err := l.UpsertDemand(ledger.Job{ID: id, Class: class, Repo: "o/r", Workflow: "o/r/.github/workflows/" + wf + "@refs/heads/main", Name: name, Event: event}, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "build", "ci.yml", "compile", "push", 10*time.Minute)
	add("b", "nightly", "nightly.yml", "soak", "schedule", 20*time.Minute)
	add("c", "deploy", "deploy-prod.yml", "ship", "push", time.Minute)
	cfg := &config.Config{PriorityHeadStart: 30 * time.Minute, Classes: []config.Class{{Name: "build", CPUs: 2, Memory: 4 << 30}}}
	high := config.PriorityRule{Level: "high"}
	high.Match.Workflow = "deploy-*.yml"
	low := config.PriorityRule{Level: "low"}
	low.Match.Event = "schedule"
	cfg.Priority = []config.PriorityRule{high, low}
	// The ledger alone: the line the rules give (a running controller's is
	// its last round's, with the jobs its runners take next at 0).
	st, err := CollectState(cfg, l, Status{}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]JobWait{}
	for _, j := range st.WaitingJobs {
		got[j.Job] = j
	}
	if j := got["ship"]; j.Position != 1 || j.Level != "high" || !strings.HasPrefix(j.Ranked, "high by rule 1") {
		t.Errorf("deploy job %+v, want first and high", j)
	}
	if j := got["compile"]; j.Position != 2 || j.Level != "" {
		t.Errorf("build job %+v, want second", j)
	}
	if j := got["soak"]; j.Position != 3 || j.Level != "low" {
		t.Errorf("nightly job %+v, want last and low", j)
	}
	if len(st.Classes) != 1 || st.Classes[0].Driver != "docker" || st.Classes[0].OS != "linux" || st.Classes[0].Labels[1] != "pumasi-build" {
		t.Errorf("classes %+v", st.Classes)
	}
	if st.Limits.PriorityHeadStartSeconds != 1800 {
		t.Errorf("limits %+v", st.Limits)
	}
}

// /api/explain answers in text with its sections, and in JSON on asking.
func TestExplainEndpoint(t *testing.T) {
	srv := httptest.NewServer(NewDemo(1).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/explain?at=350")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	for _, want := range []string{"DEMO", "ATTENTION", "MODE normal", "EXPEDITED", "LINE", "HOSTS", "RUNNERS", "OVERFLOW", "NOTES", "RESOLVED",
		"[host-unreachable] linux-spare unreachable", "`pumasi doctor` (on linux-spare)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("explain lacks %q:\n%s", want, b.String())
		}
	}
	resp, err = http.Get(srv.URL + "/api/explain?format=json&at=350")
	if err != nil {
		t.Fatal(err)
	}
	var j struct {
		Controller, Demo bool
		Findings         []Finding
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !j.Controller || !j.Demo || len(j.Findings) == 0 || j.Findings[0].Kind != "host-unreachable" {
		t.Errorf("json %+v", j)
	}

	// The state names the routes this server answers: the demo has priority, not metrics.
	resp, err = http.Get(srv.URL + "/api/state?at=350")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		API  []string
		Demo bool
		URL  string
	}
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Join(w.API, ",") != "state,jobs,job,series,explain,priority,trend,why-slow,forecast" || !w.Demo || w.URL != "https://github.com/example/app" {
		t.Errorf("state wrapper: %+v", w)
	}
	if got := strings.Join(ctlSource{}.apis(), ","); got != "state,jobs,job,series,explain,metrics,priority,trend,why-slow,forecast" {
		t.Errorf("controller apis %q", got)
	}
}

// A job larger than any host of its class could ever lend waits only for a
// host to be empty: the report says its size, what it will run with, and
// that the size may be a measurement that counted memory twice.
func TestExplainJobLargerThanAnyHost(t *testing.T) {
	s := State{Controller: true,
		Hosts: []HostState{{Name: "mac-a", Kind: "server", OS: "macos", Reachable: ptr(true), CPUs: 10, MemBytes: 32 << 30, FreeCPUs: 7, FreeMem: 20 * gb}},
		Waiting: []WaitingState{{Class: "mac-diag", Jobs: 1, Order: 1, OldestSeconds: 600,
			Reason: "no host with 6.2cpu/29.0G free (mac-a 7.0cpu/20.0G); its size 6.2cpu/127.2G is more than any host of mac-diag can ever lend (10.0cpu/29.0G at most): it runs alone on one once one is empty"}},
		Classes: []ClassState{{Name: "mac-diag"}}}
	f, ok := findOf(Explain(s, nil, explainNow), "waiting")
	if !ok {
		t.Fatal("no waiting finding")
	}
	for _, want := range []string{"needs 6.2cpu/29.0G", "mac-a has 7.0cpu/20.0G free, 9.0G memory short",
		"Its size 6.2cpu/127.2G is more than any host of mac-diag can ever lend (10.0cpu/29.0G at most): it is placed at what a host lends with no runner on it",
		"counted shared memory twice"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("text %q lacks %q", f.Text, want)
		}
	}
}

// Two e2e jobs are open and placement defers one: a runner is starting for the
// first in line, so the class's oldest and the finding's job are the other's.
func TestWaitingCountsOnlyJobsNoRunnerIsStartingFor(t *testing.T) {
	s := State{Controller: true,
		Waiting: []WaitingState{{Class: "e2e", Jobs: 1, Reason: "behind release", Order: 1, OldestSeconds: 84}},
		WaitingJobs: []JobWait{
			{Class: "e2e", Job: "e2e (webkit)", WaitedSeconds: 31, Position: 2},
			{Class: "e2e", Job: "e2e (chromium)", WaitedSeconds: 84, Position: 1},
		}}
	oldestWaiting(&s)
	if got := s.Waiting[0].OldestSeconds; got != 31 {
		t.Fatalf("oldest = %d, want 31 (the job that waits)", got)
	}
	// a job with no place has a runner of its class up for it: it is the one starting
	if js := stillWaiting([]JobWait{{Job: "b", Position: 1}, {Job: "a"}}, 1); len(js) != 1 || js[0].Job != "b" {
		t.Errorf("still waiting = %+v, want b", js)
	}
	f, ok := findOf(Explain(s, nil, explainNow), "waiting")
	if !ok || f.Job != "e2e (webkit)" || !strings.Contains(f.Text, "1 job waits, the oldest 31s") {
		t.Fatalf("finding = %+v", f)
	}
	var b bytes.Buffer
	WriteExplain(&b, s, Explain(s, nil, explainNow), nil, "", false, explainNow)
	for _, want := range []string{"1 job waits (and 1 job's runner is starting)", "e2e (chromium) 1m24s (a runner is starting for it)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("explain lacks %q:\n%s", want, b.String())
		}
	}
}
