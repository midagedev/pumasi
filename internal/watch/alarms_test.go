package watch

import (
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) // a Wednesday, noon

// run is a finished job of o/r's ci.yml (or wf) that ended ago before t0.
func run(job string, ago time.Duration, sec float64, mutate ...func(*ledger.JobRun)) ledger.JobRun {
	end := t0.Add(-ago)
	r := ledger.JobRun{Runner: job + end.String(), Class: "c", Host: "h1", Repo: "o/r", Workflow: ".github/workflows/ci.yml", Job: job,
		RunID: end.Unix(), Event: "push", Branch: "main", Result: "succeeded",
		StartedAt: end.Add(-time.Duration(sec) * time.Second), FinishedAt: end, RunSec: sec, CPUSec: sec * 2, MemWorkPeak: 2 << 30}
	for _, m := range mutate {
		m(&r)
	}
	return r
}

func keys(as []Alarm) string {
	var k []string
	for _, a := range as {
		k = append(k, a.Key)
	}
	return strings.Join(k, ",")
}

func TestJobTimeAndMemoryLimits(t *testing.T) {
	w := config.Watch{Limits: []config.Limit{{Match: config.RuleMatch{Workflow: "ci.yml", Job: "unit-*"}, MaxJobTime: config.Span(10 * time.Minute), MaxMemory: 3 << 30}}}
	var runs []ledger.JobRun
	for i := range 10 {
		runs = append(runs, run("unit-tests", time.Duration(20-i)*time.Hour, 500))
		runs = append(runs, run("lint", time.Duration(20-i)*time.Hour, 900)) // not matched
	}
	if a := Alarms(AlarmInput{Now: t0, Runs: runs}, w); len(a) != 0 {
		t.Fatalf("under the limits: %+v", a)
	}
	// Three of the last five over 10m: the median is over. One slow run alone is not.
	slow := append(runs, run("unit-tests", 3*time.Hour, 700), run("unit-tests", 2*time.Hour, 900, func(r *ledger.JobRun) { r.MemWorkPeak = 4 << 30 }))
	if a := Alarms(AlarmInput{Now: t0, Runs: slow}, w); len(a) != 0 {
		t.Fatalf("two slow runs of five: %+v", a)
	}
	slow = append(slow, run("unit-tests", time.Hour, 650, func(r *ledger.JobRun) { r.MemWorkPeak = 4 << 30 }),
		run("unit-tests", 30*time.Minute, 300, func(r *ledger.JobRun) { r.Result = "failed" }),    // a failed run says nothing
		run("unit-tests", 20*time.Minute, 2000, func(r *ledger.JobRun) { r.ContendedSec = 1000 })) // nor does a contended one
	a := Alarms(AlarmInput{Now: t0, Runs: slow}, w)
	if keys(a) != "watch/job-time/o/r/.github/workflows/ci.yml/unit-tests" ||
		a[0].Detail != "ci.yml / unit-tests takes 10m50s (median of its last 5 runs), over its 10m00s limit" || a[0].Kind != "ci-too-slow" {
		t.Fatalf("%+v", a)
	}
	// Memory: three of the last five over 3G.
	slow = append(slow, run("unit-tests", 10*time.Minute, 400, func(r *ledger.JobRun) { r.MemWorkPeak = 5 << 30 }))
	a = Alarms(AlarmInput{Now: t0, Runs: slow}, w)
	if len(a) != 2 || a[1].Detail != "ci.yml / unit-tests peaks at 4.0G of memory (median of its last 5 runs), over its 3.0G limit" {
		t.Fatalf("%+v", a)
	}
	// Back under: fast runs push the slow ones out of the last five.
	for i := range 5 {
		slow = append(slow, run("unit-tests", time.Duration(5-i)*time.Minute, 400))
	}
	if a := Alarms(AlarmInput{Now: t0, Runs: slow}, w); len(a) != 0 {
		t.Fatalf("resolved: %+v", a)
	}
}

func TestRunTimeLimit(t *testing.T) {
	w := config.Watch{Limits: []config.Limit{{Match: config.RuleMatch{Workflow: "ci.yml"}, MaxRunTime: config.Span(15 * time.Minute)}}}
	var runs []ledger.JobRun
	for i := range 5 {
		id := func(r *ledger.JobRun) { r.RunID = int64(100 + i) }
		end := time.Duration(10-i) * time.Hour
		runs = append(runs, run("build", end+10*time.Minute, 600, id), run("test", end, 400, id)) // 10m + 10m wait + ... = build start to test end
	}
	a := Alarms(AlarmInput{Now: t0, Runs: runs}, w)
	if keys(a) != "watch/run-time/o/r/.github/workflows/ci.yml" || !strings.Contains(a[0].Detail, "a run of ci.yml takes 20m00s, first job to last") {
		t.Fatalf("%+v", a)
	}
}

func TestWaitLimit(t *testing.T) {
	w := config.Watch{Limits: []config.Limit{{Match: config.RuleMatch{Workflow: "deploy*.yml"}, MaxWait: config.Span(5 * time.Minute)}}}
	live := []Live{
		{Repo: "o/r", Workflow: ".github/workflows/deploy.yml", Job: "api", RunID: 7, Since: t0.Add(-8 * time.Minute), Waiting: true},
		{Repo: "o/r", Workflow: ".github/workflows/deploy.yml", Job: "web", RunID: 7, Since: t0.Add(-2 * time.Minute), Waiting: true},
		{Repo: "o/r", Workflow: ".github/workflows/deploy.yml", Job: "db", RunID: 7, Since: t0.Add(-30 * time.Minute)}, // running
		{Repo: "o/r", Workflow: ".github/workflows/ci.yml", Job: "lint", RunID: 8, Since: t0.Add(-40 * time.Minute), Waiting: true},
	}
	a := Alarms(AlarmInput{Now: t0, Live: live}, w)
	if keys(a) != "watch/wait/1" || a[0].Detail != "deploy.yml / api of run 7 has waited 8m00s for a runner, over the 5m00s limit (workflow: deploy*.yml)" {
		t.Fatalf("%+v", a)
	}
	live[0].Waiting = false // it got a runner
	if a := Alarms(AlarmInput{Now: t0, Live: live}, w); len(a) != 0 {
		t.Fatalf("resolved: %+v", a)
	}
}

func TestFinishBy(t *testing.T) {
	w := config.Watch{Limits: []config.Limit{{Match: config.RuleMatch{Workflow: "nightly.yml", Event: "schedule"}, FinishBy: "07:00"}}}
	nightly := func(r *ledger.JobRun) {
		r.Workflow, r.Event, r.RunID = ".github/workflows/nightly.yml", "schedule", 50
	}
	day := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	in := func(now time.Time, runs []ledger.JobRun, live []Live) []Alarm {
		return Alarms(AlarmInput{Now: now, Loc: time.UTC, Runs: runs, Live: live}, w)
	}
	// Yesterday's run finished in time: before 07:00 nothing is due yet.
	yesterday := run("soak", t0.Sub(at(-20, 0)), 3600, nightly, func(r *ledger.JobRun) { r.RunID = 49 })
	if a := in(at(6, 59), []ledger.JobRun{yesterday}, nil); len(a) != 0 {
		t.Fatalf("before the deadline: %+v", a)
	}
	// 07:05 and today's run is still going: late.
	live := []Live{{Repo: "o/r", Workflow: ".github/workflows/nightly.yml", Job: "soak", Event: "schedule", RunID: 50, Since: at(3, 0)}}
	part := run("prepare", t0.Sub(at(3, 0)), 60, nightly)
	a := in(at(7, 5), []ledger.JobRun{yesterday, part}, live)
	if keys(a) != "watch/deadline/workflow: nightly.yml, event: schedule" || a[0].Kind != "ci-late" ||
		a[0].Detail != "the run of workflow: nightly.yml, event: schedule that started Wed 02:59 did not finish by 07:00 (soak running 4h05m)" {
		t.Fatalf("%+v", a)
	}
	// It finishes at 07:40: resolved.
	done := run("soak", t0.Sub(at(7, 40)), 4*3600, nightly)
	if a := in(at(7, 45), []ledger.JobRun{yesterday, part, done}, nil); len(a) != 0 {
		t.Fatalf("finished late: %+v", a)
	}
	// Tomorrow at 07:01 with no run since 07:00 today: it did not run.
	a = in(at(31, 1), []ledger.JobRun{yesterday, part, done}, nil)
	if len(a) != 1 || a[0].Detail != "no run of workflow: nightly.yml, event: schedule finished by 07:00: none has started since Wed 07:00" {
		t.Fatalf("%+v", a)
	}
}

func TestCPUBudget(t *testing.T) {
	w := config.Watch{CPUHoursPerDay: 2}
	runs := []ledger.JobRun{
		run("build", 13*time.Hour, 3600, func(r *ledger.JobRun) { r.CPUSec = 36000 }), // yesterday: not counted
		run("build", 2*time.Hour, 1800, func(r *ledger.JobRun) { r.CPUSec = 3600 }),
		run("soak", time.Hour, 1800, func(r *ledger.JobRun) { r.CPUSec = 3000; r.Workflow = ".github/workflows/nightly.yml" }),
	}
	if a := Alarms(AlarmInput{Now: t0, Loc: time.UTC, Runs: runs}, w); len(a) != 0 {
		t.Fatalf("1.8 cpu-hours: %+v", a)
	}
	runs = append(runs, run("lint", 10*time.Minute, 600, func(r *ledger.JobRun) { r.CPUSec = 1200 }))
	a := Alarms(AlarmInput{Now: t0, Loc: time.UTC, Runs: runs}, w)
	if keys(a) != "watch/cpu-budget" || a[0].Detail != "CI used 2 cpu-hours today, over the budget of 2 a day (the most: ci.yml, 1)" {
		t.Fatalf("%+v", a)
	}
	// After midnight the day starts again.
	if a := Alarms(AlarmInput{Now: t0.Add(12*time.Hour + time.Minute), Loc: time.UTC, Runs: runs}, w); len(a) != 0 {
		t.Fatalf("next day: %+v", a)
	}
}

func TestGrowthAlarmWithHysteresis(t *testing.T) {
	w := config.Watch{Slope: 0.10}
	mk := func(lastWeek float64) []ledger.JobRun {
		var rs []ledger.JobRun
		for i := range 30 {
			rs = append(rs, run("e2e", time.Duration(8*24+i*4)*time.Hour, 600+float64(i%5)))
			rs = append(rs, run("e2e", time.Duration(1*24+i*4)*time.Hour, lastWeek+float64(i%5)))
		}
		return rs
	}
	if a := Alarms(AlarmInput{Now: t0, Runs: mk(640)}, w); len(a) != 0 {
		t.Fatalf("+7%%: %+v", a)
	}
	a := Alarms(AlarmInput{Now: t0, Runs: mk(690)}, w)
	if keys(a) != "watch/growth/o/r/.github/workflows/ci.yml/e2e" || a[0].Detail != "ci.yml / e2e grew 15% in a week (10m02s -> 11m32s, median), more than the 10% a week watch.slope allows" {
		t.Fatalf("%+v", a)
	}
	// Open, it stays open down to half the limit, then goes.
	open := map[string]bool{a[0].Key: true}
	if a := Alarms(AlarmInput{Now: t0, Runs: mk(640), Open: open}, w); len(a) != 1 {
		t.Fatalf("+7%% while open: %+v", a)
	}
	if a := Alarms(AlarmInput{Now: t0, Runs: mk(620), Open: open}, w); len(a) != 0 {
		t.Fatalf("+3%% while open: %+v", a)
	}
}
