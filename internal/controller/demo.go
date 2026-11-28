package controller

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

// The demo is a made-up shop of four hosts and eight classes that the real
// placement (decide.Decide) runs, for the dashboard without a controller:
// `pumasi open --demo`. It is a pure function of its seed and the simulated
// second it is asked about, so a picture of it can be taken again. Every ten
// minutes the same scenes come round: a desktop someone works at, a nightly
// batch that waits, a deploy that goes first, a job killed at its class's
// memory limit, a host that drops off and comes back, a Mac short of disk.

const (
	demoCycle   = 600.0 // seconds; the scenes repeat with this period
	demoTick    = 2.0   // a placement round
	demoWarm    = -demoCycle
	demoStartAt = 150.0 // where a live demo starts: busy from the first look
	demoKeep    = 12    // checkpoints kept to go back by
)

// Demo serves the simulated controller.
type Demo struct {
	seed    int64
	cfg     *config.Config
	started time.Time
	clock   func() time.Time

	mu     sync.Mutex
	w      *demoWorld
	checks []*demoWorld
	specs  map[int][]demoSpec // arrivals by cycle, made once
	ops    demoOps            // what people did on the demo's priority controls; not replayed by seed
}

// NewDemo makes a demo whose clock starts at demoStartAt now.
func NewDemo(seed int64) *Demo {
	d := &Demo{seed: seed, cfg: demoConfig(), clock: time.Now, specs: map[int][]demoSpec{}}
	d.started = d.clock()
	return d
}

// Handler serves the dashboard and its JSON from the simulation. A request's
// `at` (seconds) pins the moment it shows; without it the demo runs in real time.
func (d *Demo) Handler() http.Handler {
	mux := webMux(func(r *http.Request) dashSource {
		t, now := d.when(r)
		return demoSource{d, t, now}
	})
	d.priorityRoutes(mux)
	return mux
}

// when is the sim second a request asks about, and the wall clock now.
func (d *Demo) when(r *http.Request) (float64, time.Time) {
	now := d.clock()
	t := demoStartAt + now.Sub(d.started).Seconds()
	if at, err := strconv.ParseFloat(r.URL.Query().Get("at"), 64); err == nil && at >= 0 && at < 1e7 {
		t = at
	}
	return t, now
}

type demoSource struct {
	d  *Demo
	t  float64
	at time.Time
}

func (s demoSource) live() Status { return s.d.view(s.t, s.at).live() }
func (s demoSource) state() (State, error) {
	return s.d.view(s.t, s.at).state(), nil
}
func (s demoSource) jobs() ([]ledger.FinishedJob, error) { return s.d.view(s.t, s.at).jobs(), nil }
func (s demoSource) job(runner string) ([]ledger.Decision, string, error) {
	ds, tail := s.d.view(s.t, s.at).job(runner)
	return ds, tail, nil
}
func (s demoSource) series() (Series, error) { return s.d.view(s.t, s.at).series(), nil }
func (s demoSource) url() string             { return s.d.cfg.URL }
func (s demoSource) demo() bool              { return true }
func (s demoSource) apis() []string {
	// priority covers /api/priority, /api/expedite, /api/mode and /api/priority/rules.
	return []string{"state", "jobs", "job", "series", "explain", "priority"}
}
func (s demoSource) now() time.Time { return s.at }
func (s demoSource) priority() *PriorityView {
	pv := s.d.view(s.t, s.at).priority()
	return &pv
}

// demoHost is a host of the demo and what it does over a cycle.
type demoHost struct {
	name, kind, os     string
	cpus               float64
	mem                int64
	budgetCPUs         float64
	budgetMem          int64
	otherCPU           float64 // what the rest of the machine uses, on average
	otherMem           int64
	disk               float64 // percent free
	rtt                float64
	inUse, down, short [2]float64 // local windows: someone at it, unreachable, disk short
}

const gb = int64(1) << 30

var demoHosts = []demoHost{
	{name: "linux-server", kind: "server", os: "linux", cpus: 32, mem: 128 * gb, otherCPU: 1.5, otherMem: 14 * gb, disk: 71, rtt: 2.1},
	{name: "mac-mini", kind: "server", os: "macos", cpus: 10, mem: 32 * gb, budgetCPUs: 8, budgetMem: 24 * gb, otherCPU: 1.1, otherMem: 6 * gb, disk: 41, rtt: 4.8,
		short: [2]float64{480, 570}},
	{name: "wsl-desktop", kind: "desktop", os: "linux", cpus: 16, mem: 64 * gb, budgetCPUs: 10, budgetMem: 32 * gb, otherCPU: 3, otherMem: 20 * gb, disk: 34, rtt: 9.5,
		inUse: [2]float64{0, 180}},
	{name: "linux-spare", kind: "server", os: "linux", cpus: 16, mem: 32 * gb, otherCPU: 0.6, otherMem: 3 * gb, disk: 22, rtt: 1.3,
		down: [2]float64{300, 420}},
}

func within(w [2]float64, local float64) bool { return w[1] > w[0] && local >= w[0] && local < w[1] }

// local is t's place in its cycle.
func local(t float64) float64 { return t - math.Floor(t/demoCycle)*demoCycle }

func demoConfig() *config.Config {
	c := &config.Config{URL: "https://github.com/example/app", WaitAlert: 4 * time.Minute, PriorityHeadStart: config.DefaultHeadStart,
		DiskMinFree: 10, CPUOvercommit: config.DefaultCPUOvercommit, PSICPUThreshold: 40, PSIMemThreshold: 10, PSIIOThreshold: 50, MaxStarting: 4,
		EvaluateEvery: 2 * time.Second, ProvisionTimeout: 3 * time.Minute, IdleTimeout: 5 * time.Minute, HostLostAfter: 2 * time.Minute}
	warm := config.DefaultWarm
	c.Warm = &warm
	cls := []config.Class{
		{Name: "pr-checks"},
		{Name: "e2e", CPUs: 4, Memory: 12 << 30, Docker: "inside", MaxDuration: 2 * time.Hour, Limit: config.Size{Memory: 20 << 30}},
		{Name: "build", CPUs: 6, Memory: 16 << 30, Driver: "native", Hosts: []string{"linux-server"}},
		{Name: "deploy", Reserve: 1},
		{Name: "macos", OS: "macos", CPUs: 4, Memory: 8 << 30},
		{Name: "nightly"},
		{Name: "light", CPUs: 1, Memory: 1 << 30, Overflow: true, Hosts: []string{"wsl-desktop"}, Labels: []string{"self-hosted", "light"}},
		{Name: "light-mac", OS: "macos", CPUs: 1, Memory: 1 << 30, Overflow: true, After: "light", Labels: []string{"self-hosted", "light"}},
	}
	for i := range cls {
		cl := &cls[i]
		cl.OS = cmp.Or(cl.OS, "linux")
		cl.CPUs = cmp.Or(cl.CPUs, config.DefaultCPUs)
		cl.Memory = cmp.Or(cl.Memory, config.DefaultMemory)
		if cl.Driver == "" {
			cl.Driver = "native"
			if cl.OS == "linux" {
				cl.Driver = "docker"
			}
		}
	}
	c.Classes = cls
	for _, h := range demoHosts {
		c.Hosts = append(c.Hosts, config.Host{Name: h.name, Desktop: h.kind == "desktop",
			Budget: config.Size{CPUs: h.budgetCPUs, Memory: config.Bytes(h.budgetMem)}})
	}
	high := config.PriorityRule{Level: "high"}
	high.Match.Workflow = "deploy-*.yml"
	low := config.PriorityRule{Level: "low"}
	low.Match.Event = "schedule"
	c.Priority = []config.PriorityRule{high, low}
	return c
}

// demoKind is a job of the demo's repo and how it tends to run.
type demoKind struct {
	class, workflow, job, event string
	dur, cpu, mem               [2]float64 // seconds, cores, GiB
	unseen                      bool       // never measured before the demo
	forget                      bool       // never measured at all: placed blind and watched every time
}

var demoKinds = map[string]demoKind{
	"lint":             {"pr-checks", "pr-checks.yml", "lint", "pull_request", [2]float64{40, 70}, [2]float64{.5, .8}, [2]float64{.9, 1.3}, false, false},
	"unit-tests":       {"pr-checks", "pr-checks.yml", "unit-tests", "pull_request", [2]float64{90, 150}, [2]float64{1.6, 2.2}, [2]float64{2.8, 3.6}, false, false},
	"typecheck":        {"pr-checks", "pr-checks.yml", "typecheck", "pull_request", [2]float64{50, 80}, [2]float64{.9, 1.2}, [2]float64{2, 2.5}, false, false},
	"e2e (chromium)":   {"e2e", "pr-checks.yml", "e2e (chromium)", "pull_request", [2]float64{150, 230}, [2]float64{3, 3.6}, [2]float64{9, 11}, false, false},
	"e2e (webkit)":     {"e2e", "pr-checks.yml", "e2e (webkit)", "pull_request", [2]float64{140, 210}, [2]float64{2.6, 3}, [2]float64{8, 9.5}, false, false},
	"e2e (firefox)":    {"e2e", "pr-checks.yml", "e2e (firefox)", "pull_request", [2]float64{200, 240}, [2]float64{3, 3.4}, [2]float64{13.5, 14.5}, false, false},
	"build-image":      {"build", "pr-checks.yml", "build-image", "pull_request", [2]float64{100, 160}, [2]float64{4.5, 5.5}, [2]float64{10, 13}, false, false},
	"label-check":      {"light", "pr-labels.yml", "label-check", "pull_request", [2]float64{12, 20}, [2]float64{.15, .25}, [2]float64{.25, .35}, false, false},
	"size-report":      {"light", "pr-labels.yml", "size-report", "pull_request", [2]float64{20, 35}, [2]float64{.3, .5}, [2]float64{.5, .7}, true, false},
	"docs-preview":     {"pr-checks", "docs.yml", "docs-preview", "pull_request", [2]float64{60, 90}, [2]float64{.8, 1.2}, [2]float64{1.5, 2.5}, true, true},
	"ios-build":        {"macos", "ios.yml", "ios-build", "push", [2]float64{150, 200}, [2]float64{3.2, 3.8}, [2]float64{6.5, 7.5}, false, false},
	"xcode-tests":      {"macos", "ios.yml", "xcode-tests", "push", [2]float64{90, 140}, [2]float64{2.2, 2.8}, [2]float64{4.5, 5.5}, false, false},
	"nightly-soak 1/3": {"nightly", "nightly.yml", "nightly-soak 1/3", "schedule", [2]float64{280, 320}, [2]float64{3.4, 3.8}, [2]float64{13, 14.5}, false, false},
	"nightly-soak 2/3": {"nightly", "nightly.yml", "nightly-soak 2/3", "schedule", [2]float64{280, 320}, [2]float64{3.4, 3.8}, [2]float64{13, 14.5}, false, false},
	"nightly-soak 3/3": {"nightly", "nightly.yml", "nightly-soak 3/3", "schedule", [2]float64{280, 320}, [2]float64{3.4, 3.8}, [2]float64{13, 14.5}, false, false},
	"deploy-api":       {"deploy", "deploy-prod.yml", "deploy-api", "push", [2]float64{60, 90}, [2]float64{2.2, 2.8}, [2]float64{4.5, 5.5}, false, false},
	"deploy-web":       {"deploy", "deploy-prod.yml", "deploy-web", "push", [2]float64{50, 70}, [2]float64{1.8, 2.2}, [2]float64{3.5, 4.5}, false, false},
	"deploy-worker":    {"deploy", "deploy-prod.yml", "deploy-worker", "push", [2]float64{40, 60}, [2]float64{1.2, 1.6}, [2]float64{2.5, 3.2}, false, false},
}

const demoRepo = "example/app"

// demoSpec is one job as it will arrive and run.
type demoSpec struct {
	id     int
	at     float64 // arrival, sim seconds
	kind   string
	runID  int64
	dur    float64 // how long it runs
	end    float64 // when it ends, from its start (< dur when it fails or is cancelled)
	cpu    float64
	mem    float64 // GiB it settles at
	grow   float64 // GiB/s it keeps growing after its first half minute (the job that hits its limit)
	result string
	phase  float64
}

func (sp demoSpec) k() demoKind { return demoKinds[sp.kind] }

// use is what the job uses el seconds after it started.
func (sp demoSpec) use(el float64) (cpu float64, mem int64) {
	ramp := math.Min(1, el/math.Max(10, 0.3*sp.dur))
	cpu = sp.cpu * (0.35 + 0.65*ramp) * (1 + 0.18*math.Sin(el/6+sp.phase))
	m := sp.mem * (0.25 + 0.75*ramp) * (1 + 0.03*math.Sin(el/9+sp.phase))
	if sp.grow > 0 && el > 30 {
		m += sp.grow * (el - 30)
	}
	return math.Max(0.02, cpu), int64(m * float64(gb))
}

// cycleSpecs makes the arrivals of cycle k: the same for the same seed.
func (d *Demo) cycleSpecs(k int) []demoSpec {
	if s, ok := d.specs[k]; ok {
		return s
	}
	r := rand.New(rand.NewPCG(uint64(d.seed), uint64(k+1<<20)))
	base := float64(k) * demoCycle
	run := int64(9100000+k*100) * 10
	var out []demoSpec
	add := func(at float64, kind string, runID int64) *demoSpec {
		kd := demoKinds[kind]
		u := func(v [2]float64) float64 { return v[0] + r.Float64()*(v[1]-v[0]) }
		sp := demoSpec{at: base + at, kind: kind, runID: runID, dur: u(kd.dur), cpu: u(kd.cpu), mem: u(kd.mem), result: "succeeded", phase: r.Float64() * 6}
		sp.end = sp.dur
		switch x := r.Float64(); {
		case x < 0.07:
			sp.result, sp.end = "failed", sp.dur*(0.5+0.4*r.Float64())
		case x < 0.17:
			sp.mem *= 1.3 + 0.2*r.Float64() // a run larger than the job has been: over its estimate
		}
		out = append(out, sp)
		return &out[len(out)-1]
	}
	// A pull request pushed every minute or so.
	push := 0
	for at := 5 + r.Float64()*10; at < demoCycle-20; at += 48 + r.Float64()*22 {
		push++
		run++
		o := func() float64 { return at + r.Float64()*3 }
		add(o(), "lint", run)
		ut := add(o(), "unit-tests", run)
		if push == 3 {
			ut.result, ut.end = "canceled", ut.dur*0.5
		}
		add(o(), "typecheck", run)
		add(o(), "e2e (chromium)", run)
		if r.Float64() < 0.4 {
			add(o(), "e2e (webkit)", run)
		}
		if r.Float64() < 0.35 {
			add(o(), "build-image", run)
		}
		add(o(), "label-check", run)
		if r.Float64() < 0.6 {
			add(o(), "size-report", run)
		}
	}
	// A docs preview nobody has measured: placed on a guess and watched.
	run++
	add(170, "docs-preview", run)
	// The one that hits the e2e class's 20G limit.
	run++
	ff := add(140, "e2e (firefox)", run)
	ff.result, ff.end, ff.mem, ff.grow, ff.dur = "succeeded", 240, 14.8, 0.1, 240
	// iOS builds every two minutes or so.
	for at, i := 20+r.Float64()*15, 0; at < demoCycle-30; at, i = at+105+r.Float64()*30, i+1 {
		run++
		add(at, []string{"ios-build", "xcode-tests"}[i%2], run)
	}
	// The nightly batch, scheduled: low.
	run++
	for i := 1; i <= 3; i++ {
		add(60+float64(i), fmt.Sprintf("nightly-soak %d/3", i), run)
	}
	// A deploy: high.
	run++
	for i, j := range []string{"deploy-api", "deploy-web", "deploy-worker"} {
		add(200+float64(i), j, run)
	}
	slices.SortStableFunc(out, func(a, b demoSpec) int { return cmp.Compare(a.at, b.at) })
	for i := range out {
		out[i].id = (k+2)*1000 + i
	}
	d.specs[k] = out
	return out
}

// The world at a moment: what changes from tick to tick. Plain values, so a
// checkpoint is a copy.
type demoWorld struct {
	t        float64 // the last tick done
	cycle    int     // the cycle whose arrivals are next
	next     int     // the next of its arrivals
	demand   []demoJob
	runners  []demoRunner
	finished []demoFinished // oldest first
	measured map[string][]size.Sample
	lastRun  map[string]float64
	incs     []demoIncident
	back     []demoBack
	defers   []decide.Defer
	take     map[string]int
	holds    map[string]int
	starts   map[string]demoDecision // runner -> its start decision
	seq      uint32
	highOnly []decide.Defer // jobs high-only mode keeps out of this round, by class
}

type demoJob struct {
	spec      demoSpec
	class     string // the class that holds it
	firstSeen float64
}

func (j demoJob) id() string { return "job-" + strconv.Itoa(j.spec.id) }

type demoRunner struct {
	name, class, host string
	slot              int
	created, prov     float64
	started           float64 // 0 = no job yet
	job               demoJob
	est               size.Estimate
	cpuSec            float64
	memPeak           int64
	ended             bool // its job is over while the host was away; it is told when the host is back
}

type demoFinished struct {
	r        demoRunner
	finished float64
	result   string
	oom      bool
}

type demoIncident struct {
	in               ledger.Incident
	opened, resolved float64 // resolved < opened = open
}

type demoBack struct {
	job  demoJob
	left float64
}

type demoDecision struct {
	at                   float64
	action, host, reason string
}

func (w *demoWorld) clone() *demoWorld {
	c := *w
	c.demand = slices.Clone(w.demand)
	c.runners = slices.Clone(w.runners)
	c.finished = slices.Clone(w.finished)
	c.incs = slices.Clone(w.incs)
	c.back = slices.Clone(w.back)
	c.defers = slices.Clone(w.defers)
	c.measured = make(map[string][]size.Sample, len(w.measured))
	for k, v := range w.measured {
		c.measured[k] = slices.Clone(v)
	}
	c.lastRun = cloneMap(w.lastRun)
	c.take = cloneMap(w.take)
	c.holds = cloneMap(w.holds)
	c.starts = cloneMap(w.starts)
	return &c
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	c := make(map[K]V, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func sizeKey(job, platform string) string {
	kd := demoKinds[job]
	return demoRepo + "|" + kd.workflow + "|" + kd.job + "|" + platform
}

// fresh is the world before the demo begins: the jobs measured over the
// days before, and a few problems that came and went.
func (d *Demo) fresh() *demoWorld {
	w := &demoWorld{t: demoWarm, cycle: int(math.Floor(demoWarm / demoCycle)), measured: map[string][]size.Sample{}, lastRun: map[string]float64{},
		take: map[string]int{}, holds: map[string]int{}, starts: map[string]demoDecision{}}
	r := rand.New(rand.NewPCG(uint64(d.seed), 7))
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range demoKinds {
			if !yield(k) {
				return
			}
		}
	})
	for i, name := range names {
		kd := demoKinds[name]
		if kd.unseen {
			continue
		}
		cl := d.class(kd.class)
		platforms := []string{cl.Platform()}
		if kd.class == "light" {
			platforms = append(platforms, d.class("light-mac").Platform())
		}
		for _, p := range platforms {
			k := sizeKey(name, p)
			for range size.Window {
				m := kd.mem[0] + r.Float64()*(kd.mem[1]-kd.mem[0])
				c := kd.cpu[0] + r.Float64()*(kd.cpu[1]-kd.cpu[0])
				sec := kd.dur[0] + r.Float64()*(kd.dur[1]-kd.dur[0])
				w.measured[k] = append(w.measured[k], size.Sample{CPUs: c * 0.8, MemBytes: int64(m * float64(gb)), Secs: sec})
			}
			w.lastRun[k] = demoWarm - 3600 - float64(i)*60
		}
	}
	past := []demoIncident{
		{in: ledger.Incident{Key: "controller-restart", Kind: "controller-restart", Subject: "controller",
			Detail: "controller is back: was down 9s, adopted 5 runners, 0 jobs finished meanwhile"}, opened: -6.2 * 3600, resolved: -6.2 * 3600},
		{in: ledger.Incident{Key: "waiting/e2e", Kind: "job-waiting", Subject: "e2e",
			Detail: "3 e2e jobs waiting, the oldest for 4m10s — no host with 4.4cpu/13.2G free (linux-server 3.1cpu/40.2G, wsl-desktop in use, linux-spare 2.0cpu/9.8G)"}, opened: -3.1 * 3600, resolved: -3.0 * 3600},
		{in: ledger.Incident{Key: "agent-restarted/linux-spare", Kind: "agent-restarted", Subject: "linux-spare",
			Detail: "the agent on linux-spare had stopped and was started again; runners were not touched", Action: "agent restarted"}, opened: -1.4 * 3600, resolved: -1.4 * 3600},
	}
	w.incs = past
	return w
}

func (d *Demo) class(name string) config.Class {
	for _, c := range d.cfg.Classes {
		if c.Name == name {
			return c
		}
	}
	return config.Class{}
}

func (d *Demo) host(name string) demoHost {
	for _, h := range demoHosts {
		if h.name == name {
			return h
		}
	}
	return demoHost{}
}

// at brings the world to time t: forward from where it is, or from the last
// checkpoint before t, or from the start.
func (d *Demo) at(t float64) *demoWorld {
	if d.w == nil || t < d.w.t {
		d.w = nil
		for i := len(d.checks) - 1; i >= 0; i-- {
			if d.checks[i].t <= t {
				d.w = d.checks[i].clone()
				d.checks = d.checks[:i+1]
				break
			}
		}
		if d.w == nil {
			d.w, d.checks = d.fresh(), nil
		}
	}
	for d.w.t+demoTick <= t {
		d.step(d.w)
		if n := int(math.Round(d.w.t / demoTick)); n%30 == 0 {
			d.checks = append(d.checks, d.w.clone())
			if len(d.checks) > demoKeep {
				d.checks = d.checks[1:]
			}
		}
	}
	return d.w
}

func (d *Demo) step(w *demoWorld) {
	t := w.t + demoTick
	w.t = t
	d.arrive(w, t)
	d.progress(w, t)
	d.giveBack(w, t)
	d.hostIncidents(w, t)
	plan := decide.Decide(d.snapshot(w, t))
	d.apply(w, t, plan)
	d.waitIncidents(w, t)
}

func (d *Demo) arrive(w *demoWorld, t float64) {
	for {
		specs := d.cycleSpecs(w.cycle)
		if w.next >= len(specs) {
			if float64(w.cycle+1)*demoCycle > t {
				return
			}
			w.cycle, w.next = w.cycle+1, 0
			continue
		}
		sp := specs[w.next]
		if sp.at > t {
			return
		}
		w.next++
		class := sp.k().class
		if d.class(class).Overflow {
			// GitHub gives an overflow class no more than it takes; the rest go
			// to the class after it, or to runners pumasi does not see.
			class = ""
			for _, c := range []string{"light", "light-mac"} {
				if w.take[c] > w.holds[c] {
					class = c
					w.holds[c]++
					break
				}
			}
			if class == "" {
				continue
			}
		}
		w.demand = append(w.demand, demoJob{spec: sp, class: class, firstSeen: sp.at})
	}
}

func (d *Demo) up(host string, t float64) bool { return !within(d.host(host).down, local(t)) }

func (d *Demo) limitOf(r demoRunner) int64 {
	if l := int64(d.class(r.class).Limit.Memory); l > 0 {
		return l
	}
	h := d.host(r.host)
	return int64(minPositive(float64(h.mem/5*4), float64(h.budgetMem)))
}

func (d *Demo) progress(w *demoWorld, t float64) {
	var keep []demoRunner
	for _, r := range w.runners {
		up := d.up(r.host, t)
		if r.started == 0 {
			if t < r.created+r.prov || !up {
				keep = append(keep, r)
				continue
			}
			// Listening: GitHub hands it the class's oldest job.
			i := slices.IndexFunc(w.demand, func(j demoJob) bool { return j.class == r.class })
			if i < 0 {
				keep = append(keep, r) // it listens until a job comes or pumasi stops it
				continue
			}
			r.job, r.started = w.demand[i], t
			w.demand = slices.Delete(w.demand, i, i+1)
			r.est = d.estimate(w, r.job.spec.kind, r.class)
			keep = append(keep, r)
			continue
		}
		el := t - r.started
		cpu, mem := r.job.spec.use(el)
		r.cpuSec += cpu * demoTick
		r.memPeak = max(r.memPeak, mem)
		oom := r.job.spec.grow > 0 && mem >= d.limitOf(r)
		if (el < r.job.spec.end && !oom) || !up {
			if oom {
				r.ended = true
			}
			keep = append(keep, r)
			continue
		}
		result := r.job.spec.result
		if oom {
			result = "failed"
		}
		d.finish(w, r, t, result, oom)
	}
	w.runners = keep
}

func (d *Demo) finish(w *demoWorld, r demoRunner, t float64, result string, oom bool) {
	w.finished = append(w.finished, demoFinished{r: r, finished: t, result: result, oom: oom})
	if len(w.finished) > 60 {
		w.finished = slices.Delete(w.finished, 0, len(w.finished)-60)
	}
	if run := t - r.started; run > 0 && result != "canceled" && !r.job.spec.k().forget {
		k := sizeKey(r.job.spec.kind, d.class(r.class).Platform())
		runs := append(w.measured[k], size.Sample{CPUs: r.cpuSec / run, MemBytes: r.memPeak, OOM: oom, Secs: run})
		if len(runs) > size.Window {
			runs = runs[len(runs)-size.Window:]
		}
		w.measured[k] = slices.Clone(runs)
		w.lastRun[k] = t
	}
	if oom {
		w.incs = append(w.incs, demoIncident{in: ledger.Incident{Key: "oom/" + r.name, Kind: "out-of-memory", Subject: r.name,
			Detail: fmt.Sprintf("%s hit class %s's memory limit (%s) on %s; the kernel killed a process of the job", r.job.spec.kind, r.class, gib(d.limitOf(r)), r.host),
			Action: "none: raise or drop the class's limit, or fix the job; not rerun"}, opened: t, resolved: t})
	}
	delete(w.starts, r.name)
}

// giveBack: an overflow job held and not placed for half a minute goes back
// to GitHub, which gives it to another runner of its labels.
func (d *Demo) giveBack(w *demoWorld, t float64) {
	deferred := map[string]bool{}
	for _, df := range w.defers {
		deferred[df.Class] = true
	}
	covering := map[string]int{}
	for _, r := range w.runners {
		if r.started == 0 {
			covering[r.class]++
		}
	}
	var keep []demoJob
	for _, j := range w.demand {
		if d.class(j.class).Overflow && deferred[j.class] {
			if covering[j.class] > 0 {
				covering[j.class]--
			} else if t-j.firstSeen >= 30 {
				w.back = append(w.back, demoBack{job: j, left: t})
				continue
			}
		}
		keep = append(keep, j)
	}
	w.demand = keep
	var back []demoBack
	for _, b := range w.back {
		if t-b.left <= 3600 {
			back = append(back, b)
		}
	}
	w.back = back
}

func (d *Demo) openIncident(w *demoWorld, t float64, in ledger.Incident) {
	for i := range w.incs {
		if w.incs[i].in.Key == in.Key && w.incs[i].resolved < w.incs[i].opened {
			w.incs[i].in.Detail = in.Detail
			return
		}
	}
	w.incs = append(w.incs, demoIncident{in: in, opened: t, resolved: -1e18})
}

func (d *Demo) resolveIncident(w *demoWorld, t float64, key string) {
	for i := range w.incs {
		if w.incs[i].in.Key == key && w.incs[i].resolved < w.incs[i].opened {
			w.incs[i].resolved = t
		}
	}
}

func (d *Demo) hostIncidents(w *demoWorld, t float64) {
	lt := local(t)
	for _, h := range demoHosts {
		if within(h.down, lt) && lt-h.down[0] >= 30 {
			jobs := 0
			for _, r := range w.runners {
				if r.host == h.name && r.started > 0 {
					jobs++
				}
			}
			d.openIncident(w, t, ledger.Incident{Key: "host-unreachable/" + h.name, Kind: "host-unreachable", Subject: h.name, Detail: fmt.Sprintf(
				"%s unreachable for %s (keepalive timeout) — %d jobs still running there, no new runners placed", h.name, time.Duration(lt-h.down[0])*time.Second, jobs)})
		} else {
			d.resolveIncident(w, t, "host-unreachable/"+h.name)
		}
		if within(h.short, lt) {
			total := 460 * gb
			d.openIncident(w, t, ledger.Incident{Key: "disk-low/" + h.name, Kind: "disk-low", Subject: h.name, Detail: fmt.Sprintf(
				"%s has %s free on the disk that holds its runners (%.0f%%, below %.0f%%); no new runners placed there",
				h.name, gib(total*8/100), 8.0, d.cfg.DiskMinFree)})
		} else {
			d.resolveIncident(w, t, "disk-low/"+h.name)
		}
	}
}

// estimate is what the next run of a job is placed with, as the controller works it out.
func (d *Demo) estimate(w *demoWorld, job, class string) size.Estimate {
	cl := d.class(class)
	first := size.Estimate{CPUs: cl.CPUs, MemBytes: int64(cl.Memory)}
	if runs := w.measured[sizeKey(job, cl.Platform())]; len(runs) > 0 {
		return size.Learn(runs, first)
	}
	kd := demoKinds[job]
	var wf []size.Sample
	for name, o := range demoKinds {
		if name != job && o.workflow == kd.workflow {
			wf = append(wf, w.measured[sizeKey(name, cl.Platform())]...)
		}
	}
	if len(wf) > 0 {
		return size.Borrow(first, size.Learn(wf, size.Estimate{}))
	}
	first.Blind = true
	return first
}

// hostNow is what the host's agent would say at t.
type demoHostNow struct {
	up, inUse, short bool
	otherCPU         float64
	otherMem         int64
	psiCPU, psiIO    float64
	disk             float64
}

func (d *Demo) hostNow(w *demoWorld, h demoHost, t float64) demoHostNow {
	lt := local(t)
	n := demoHostNow{up: !within(h.down, lt), inUse: within(h.inUse, lt), short: within(h.short, lt), disk: h.disk}
	wob := math.Sin(t/37 + float64(len(h.name)))
	n.otherCPU = h.otherCPU * (1 + 0.25*wob)
	n.otherMem = int64(float64(h.otherMem) * (1 + 0.05*math.Sin(t/53+float64(len(h.name)))))
	if h.inUse[1] > 0 && !n.inUse {
		n.otherCPU, n.otherMem = n.otherCPU*0.4, n.otherMem*6/10 // nobody at it: less open
	}
	load, starting := n.otherCPU, 0
	for _, r := range w.runners {
		if r.host != h.name {
			continue
		}
		if r.started > 0 {
			c, _ := r.job.spec.use(t - r.started)
			load += c
		} else {
			starting++
		}
	}
	n.psiCPU = math.Round(math.Max(0, load/h.cpus-0.55)*90*100) / 100
	n.psiIO = math.Round((3+4*float64(starting)+2*math.Abs(math.Sin(t/23)))*100) / 100
	if n.short {
		n.disk = 8
	}
	return n
}

func (d *Demo) snapshot(w *demoWorld, t float64) decide.Snapshot {
	now := demoEpoch.Add(time.Duration(t * float64(time.Second)))
	at := func(s float64) time.Time { return demoEpoch.Add(time.Duration(s * float64(time.Second))) }
	s := decide.Snapshot{Now: now, PSICPUThreshold: d.cfg.PSICPUThreshold, PSIMemThreshold: d.cfg.PSIMemThreshold, PSIIOThreshold: d.cfg.PSIIOThreshold,
		MaxStarting: d.cfg.MaxStarting, HeadStart: d.cfg.PriorityHeadStart, BlindHold: size.BlindHold, CPUOvercommit: d.cfg.CPUOvercommit,
		ProvisionTimeout: d.cfg.ProvisionTimeout, IdleTimeout: d.cfg.IdleTimeout, HostLostAfter: d.cfg.HostLostAfter}
	for _, h := range demoHosts {
		n := d.hostNow(w, h, t)
		dh := decide.Host{Name: h.name, Kind: h.kind, OS: h.os, Up: n.up, CPUs: h.cpus, MemBytes: h.mem, BudgetCPUs: h.budgetCPUs, BudgetMem: h.budgetMem,
			NonRunnerCPU: n.otherCPU, NonRunnerMem: n.otherMem, KeepMem: keepMem(h.mem)}
		if !n.up {
			dh.DownFor = time.Duration((local(t) - h.down[0]) * float64(time.Second))
		}
		if h.os == "linux" {
			dh.PSICPU, dh.PSIIO, dh.CPUPressureKnown = n.psiCPU, n.psiIO, true
		}
		dh.Closed = d.closed(h, n)
		s.Hosts = append(s.Hosts, dh)
	}
	for _, cl := range d.cfg.Classes {
		dc := decide.Class{Name: cl.Name, OS: cl.OS, CPUs: cl.CPUs, MemBytes: int64(cl.Memory), Hosts: cl.Hosts, Reserve: cl.Reserve,
			Native: cl.Driver == "native", MaxDuration: cl.MaxDuration, Overflow: cl.Overflow}
		if cl.Overflow {
			for _, job := range []string{"label-check", "size-report"} {
				e := d.estimate(w, job, cl.Name)
				if e.Runs > 0 || e.Borrowed {
					dc.NextCPUs, dc.NextMem = math.Max(dc.NextCPUs, e.CPUs), max(dc.NextMem, e.MemBytes)
				}
			}
		} else {
			d.warm(w, &dc, t)
		}
		s.Classes = append(s.Classes, dc)
	}
	for _, j := range w.demand {
		e := d.estimate(w, j.spec.kind, j.class)
		dd := decide.Demand{ID: j.id(), Class: j.class, FirstSeen: at(j.firstSeen), Name: j.spec.kind, Sized: "(" + e.Source() + ")", Blind: e.Blind}
		if !e.Blind {
			dd.CPUs, dd.MemBytes = e.CPUs, e.MemBytes
		}
		dd.Level, dd.Ranked, dd.Expedite = d.standing(d.ops, j, t)
		s.Demand = append(s.Demand, dd)
	}
	// High-only mode: normal and low jobs are not in the round; they wait.
	w.highOnly = nil
	if mode, until := d.ops.modeAt(t); mode == "high-only" {
		reason := "high-only mode (normal and low jobs wait)"
		if !math.IsInf(until, 1) {
			// relative: a demo pinned with ?at= has no wall clock of its own
			reason = "high-only mode for " + shortDur(time.Duration((until-t)*float64(time.Second))) + " more (normal and low jobs wait)"
		}
		idx := map[string]int{}
		kept := s.Demand[:0]
		for _, dd := range decide.Line(s.Demand, nil, s.HeadStart) {
			if dd.Level >= 1 || dd.Expedite > 0 {
				kept = append(kept, dd)
				continue
			}
			if i, ok := idx[dd.Class]; ok {
				w.highOnly[i].Count++
				continue
			}
			idx[dd.Class] = len(w.highOnly)
			w.highOnly = append(w.highOnly, decide.Defer{Class: dd.Class, Count: 1, Reason: reason})
		}
		s.Demand = kept
	}
	for _, r := range w.runners {
		dr := decide.Runner{Name: r.name, Class: r.class, Host: r.host, State: "starting", CreatedAt: at(r.created), Container: "running"}
		cpus, mem := d.charge(r, t)
		dr.CPUs, dr.MemBytes = cpus, mem
		switch {
		case r.started > 0:
			dr.State, dr.JobKnown, dr.Working, dr.JobStartedAt, dr.ListeningAt = "busy", true, true, at(r.started), at(r.created+r.prov)
			dr.Watched = r.est.Watched(at(r.started), now)
		case t >= r.created+r.prov:
			dr.State, dr.ListeningAt = "idle", at(r.created+r.prov)
		}
		s.Runners = append(s.Runners, dr)
	}
	return s
}

var demoEpoch = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

func (d *Demo) waitingJob(j demoJob) config.WaitingJob {
	kd := j.spec.k()
	return config.WaitingJob{Class: j.class, Repo: demoRepo, WorkflowRef: ".github/workflows/" + kd.workflow + "@refs/heads/main", Name: kd.job, Event: kd.event}
}

// charge is what a runner holds: its estimate (times the hold while watched), or its use when more.
func (d *Demo) charge(r demoRunner, t float64) (float64, int64) {
	if r.started == 0 {
		hold := int64(1)
		if r.est.Blind {
			hold = size.BlindHold
		}
		return r.est.CPUs, r.est.MemBytes * hold
	}
	cpu, mem := r.job.spec.use(t - r.started)
	watched := r.est.Watched(demoEpoch.Add(time.Duration(r.started*float64(time.Second))), demoEpoch.Add(time.Duration(t*float64(time.Second))))
	return size.Charge(r.est, cpu, mem, watched)
}

func (d *Demo) closed(h demoHost, n demoHostNow) string {
	switch {
	case !n.up:
		return ""
	case n.disk < d.cfg.DiskMinFree:
		return fmt.Sprintf("disk %.0f%% free", n.disk)
	case n.inUse:
		return "in use"
	}
	return ""
}

// warm is how many runners a class keeps warm at t and how large its next
// job is taken to be, as the controller works them out: from the jobs that
// arrived in the last decide.WarmWindow, the largest of their estimates.
func (d *Demo) warm(w *demoWorld, dc *decide.Class, t float64) {
	var arrived []time.Time
	first := int(math.Floor(demoWarm / demoCycle))
	for k := max(first, int(math.Floor((t-decide.WarmWindow.Seconds())/demoCycle))); float64(k)*demoCycle <= t; k++ {
		for _, sp := range d.cycleSpecs(k) {
			if sp.k().class != dc.Name || sp.at > t || sp.at < t-decide.WarmWindow.Seconds() {
				continue
			}
			arrived = append(arrived, demoEpoch.Add(time.Duration(sp.at*float64(time.Second))))
			if e := d.estimate(w, sp.kind, dc.Name); !e.Blind {
				dc.NextCPUs, dc.NextMem = math.Max(dc.NextCPUs, e.CPUs), max(dc.NextMem, e.MemBytes)
			}
		}
	}
	dc.Warm = decide.WarmCount(arrived, d.cfg.WarmMax())
}

func (d *Demo) apply(w *demoWorld, t float64, p decide.Plan) {
	for _, st := range p.Stop {
		// A runner with a job goes when the job ends; one without leaves now.
		if i := slices.IndexFunc(w.runners, func(r demoRunner) bool { return r.name == st.Runner }); i >= 0 && w.runners[i].started == 0 {
			delete(w.starts, st.Runner)
			w.runners = slices.Delete(w.runners, i, i+1)
		}
	}
	for _, st := range p.Start {
		w.seq++
		hh := fnv.New32a()
		fmt.Fprintf(hh, "%d/%s/%d", d.seed, st.Job, w.seq)
		name := fmt.Sprintf("%s-%s-%08x", st.Class, st.Host, hh.Sum32())
		est := size.Estimate{CPUs: st.CPUs, MemBytes: st.MemBytes}
		if i := slices.IndexFunc(w.demand, func(j demoJob) bool { return j.id() == st.Job }); i >= 0 {
			est = d.estimate(w, w.demand[i].spec.kind, st.Class)
		}
		used := map[int]bool{}
		for _, r := range w.runners {
			if r.host == st.Host && r.class == st.Class {
				used[r.slot] = true
			}
		}
		slot := 1
		for used[slot] {
			slot++
		}
		w.runners = append(w.runners, demoRunner{name: name, class: st.Class, host: st.Host, slot: slot, created: t, prov: 3 + float64(hh.Sum32()%2000)/1000, est: est})
		w.starts[name] = demoDecision{at: t, action: "start", host: st.Host, reason: st.Reason}
	}
	w.defers = append(slices.Clone(p.Defer), w.highOnly...)
	// Each overflow class's take, as setTakes works it out.
	held, unplaced := map[string]int{}, map[string]int{}
	for _, j := range w.demand {
		held[j.class]++
	}
	for _, r := range w.runners {
		if r.started > 0 {
			held[r.class]++
		}
	}
	for _, df := range p.Defer {
		unplaced[df.Class] += df.Count
	}
	for _, cl := range d.cfg.Classes {
		if !cl.Overflow {
			continue
		}
		room := p.Room[cl.Name]
		if cl.After != "" && p.Room[cl.After] > 0 {
			room = 0
		}
		w.holds[cl.Name] = held[cl.Name]
		w.take[cl.Name] = max(0, held[cl.Name]-unplaced[cl.Name]) + room
	}
}

func (d *Demo) waitIncidents(w *demoWorld, t float64) {
	late := map[string]bool{}
	for _, df := range w.defers {
		var of []demoJob
		for _, j := range w.demand {
			if j.class == df.Class {
				of = append(of, j)
			}
		}
		if df.Count > len(of) {
			continue
		}
		waited := t - of[len(of)-df.Count].firstSeen
		if waited < d.cfg.WaitAlert.Seconds() {
			continue
		}
		late[df.Class] = true
		d.openIncident(w, t, ledger.Incident{Key: "waiting/" + df.Class, Kind: "job-waiting", Subject: df.Class, Detail: fmt.Sprintf(
			"%d %s jobs waiting, the oldest for %s — %s", df.Count, df.Class, time.Duration(waited)*time.Second, df.Reason)})
	}
	for _, cl := range d.cfg.Classes {
		if !late[cl.Name] {
			d.resolveIncident(w, t, "waiting/"+cl.Name)
		}
	}
}

// demoView is the world at t, told as of the wall clock now.
type demoView struct {
	d      *Demo
	w      *demoWorld
	t      float64
	now    time.Time
	ops    demoOps
	expect map[string]decide.Expect // by job id
}

func (d *Demo) view(t float64, now time.Time) demoView {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.at(t).clone()
	return demoView{d, w, t, now, d.ops.clone(), d.forecast(w.clone(), t)}
}

// forecast is when each waiting job is expected to start, as the controller
// works it out: run times from the measured runs, a runner up in 4s.
func (d *Demo) forecast(w *demoWorld, t float64) map[string]decide.Expect {
	s := d.snapshot(w, t)
	runtime := func(kind, class string) (time.Duration, bool) {
		return size.Runtime(w.measured[sizeKey(kind, d.class(class).Platform())])
	}
	busy := map[string]decide.Busy{}
	for _, r := range w.runners {
		if r.started == 0 {
			continue
		}
		b := decide.Busy{Job: r.job.spec.kind}
		if rt, ok := runtime(r.job.spec.kind, r.class); ok {
			b.Ends = decide.EndOf(demoEpoch.Add(time.Duration(r.started*float64(time.Second))), s.Now, rt)
		}
		busy[r.name] = b
	}
	runs := map[string]time.Duration{}
	for _, j := range w.demand {
		if rt, ok := runtime(j.spec.kind, j.class); ok {
			runs[j.id()] = rt
		}
	}
	return decide.Forecast(s, busy, runs, 4*time.Second)
}

// ts is the wall time of sim second s.
func (v demoView) ts(s float64) time.Time {
	return v.now.Add(time.Duration((s - v.t) * float64(time.Second))).UTC()
}

func secs(f float64) int64 { return int64(math.Max(0, f)) }

func (v demoView) state() State {
	d, w, t := v.d, v.w, v.t
	out := State{Controller: true, Incidents: []ledger.Incident{}, Overflow: []Overflow{}, Warnings: []string{}, Hosts: []HostState{}, Runners: []RunnerState{},
		Waiting: []WaitingState{}, WaitingJobs: []JobWait{}, TakenBack: []JobWait{}, RecentIncidents: []ledger.Incident{}, Sizes: []JobSize{}, Expedited: []Expedited{}, Classes: classStates(d.cfg)}
	out.Limits = StateLimits{PSICPU: d.cfg.PSICPUThreshold, PSIMemory: d.cfg.PSIMemThreshold, PSIIO: d.cfg.PSIIOThreshold, DiskMinFree: d.cfg.DiskMinFree,
		WaitAlertSeconds: int64(d.cfg.WaitAlert.Seconds()), PriorityHeadStartSeconds: int64(d.cfg.PriorityHeadStart.Seconds())}
	for _, in := range w.incs {
		i := in.in
		i.OpenedAt = v.ts(in.opened)
		switch {
		case in.resolved < in.opened:
			out.Incidents = append(out.Incidents, i)
		case t-in.resolved <= 24*3600:
			i.ResolvedAt = v.ts(in.resolved)
			out.RecentIncidents = append(out.RecentIncidents, i)
		}
	}
	slices.SortStableFunc(out.RecentIncidents, func(a, b ledger.Incident) int { return a.ResolvedAt.Compare(b.ResolvedAt) })
	for i, cl := range out.Classes {
		if !cl.Overflow {
			dc := decide.Class{Name: cl.Name}
			d.warm(w, &dc, t)
			out.Classes[i].Warm = dc.Warm
		}
	}
	hostIdx := map[string]int{}
	for _, h := range demoHosts {
		n := d.hostNow(w, h, t)
		hs := HostState{Name: h.name, Kind: h.kind, BudgetCPUs: h.budgetCPUs, BudgetMem: h.budgetMem, Reachable: &n.up, Agent: "a41c07e2d9b3", RTTMs: math.Round(h.rtt*(1+0.2*math.Sin(t/11))*1000) / 1000}
		seen := int64(1 + int(t)%3)
		if !n.up {
			seen = secs(local(t) - h.down[0])
			hs.LastDrop = "keepalive timeout"
		} else if h.down[1] > 0 {
			hs.LastDrop = "keepalive timeout"
		}
		hs.SeenSeconds = &seen
		if n.up {
			hs.OS = h.os
			hs.CPUs, hs.MemBytes = h.cpus, h.mem
			if h.os == "linux" {
				c, m, io := n.psiCPU, 0.0, n.psiIO
				hs.CPUPressure, hs.MemPressure, hs.IOPressure = &c, &m, &io
			}
			hs.OtherCPUs, hs.OtherMem, hs.KeepMem = n.otherCPU, n.otherMem, keepMem(h.mem)
			hs.Closed = d.closed(h, n)
			disk := n.disk
			hs.DiskFreePct = &disk
		}
		out.Hosts = append(out.Hosts, hs)
		hostIdx[h.name] = len(out.Hosts) - 1
	}
	for _, r := range w.runners {
		cl := d.class(r.class)
		rs := RunnerState{Name: r.name, Class: r.class, Host: r.host, State: "starting", AgeSeconds: secs(t - r.created), Slot: r.slot,
			EstCPUs: cmp.Or(r.est.CPUs, cl.CPUs), EstMem: cmp.Or(r.est.MemBytes, int64(cl.Memory)), EstRuns: r.est.Runs}
		if r.started == 0 && t >= r.created+r.prov {
			rs.State = "idle"
		}
		if r.started > 0 {
			rs.State, rs.Job = "busy", r.job.spec.kind
			rs.Watched = r.est.Watched(demoEpoch.Add(time.Duration(r.started*float64(time.Second))), demoEpoch.Add(time.Duration(t*float64(time.Second))))
		}
		if d.up(r.host, t) {
			rs.MemLimit = d.limitOf(r)
			if r.started > 0 {
				cpu, mem := r.job.spec.use(t - r.started)
				rs.CPUNow, rs.MemBytes, rs.CPUSec = math.Round(cpu*1000)/1000, mem, math.Round(r.cpuSec*10)/10
			} else {
				rs.CPUNow, rs.MemBytes = 0.3, 180<<20
			}
		}
		rs.HeldCPUs, rs.HeldMem = size.Charge(size.Estimate{CPUs: rs.EstCPUs, MemBytes: rs.EstMem}, rs.CPUNow, rs.MemBytes, rs.Watched || (r.started == 0 && r.est.Blind))
		out.Runners = append(out.Runners, rs)
		h := &out.Hosts[hostIdx[r.host]]
		h.Runners++
		h.UsedCPUs += rs.HeldCPUs
		h.UsedMem += rs.HeldMem
	}
	for i := range out.Hosts {
		h := &out.Hosts[i]
		if h.MemBytes == 0 {
			continue
		}
		cpu, mem := decide.Free(decide.Host{CPUs: h.CPUs, MemBytes: h.MemBytes, BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem,
			NonRunnerCPU: h.OtherCPUs, NonRunnerMem: h.OtherMem, KeepMem: h.KeepMem}, h.UsedCPUs, float64(h.UsedMem))
		h.FreeCPUs, h.FreeMem = cpu, int64(mem)
	}
	for _, cl := range d.cfg.Classes {
		if cl.Overflow {
			out.Overflow = append(out.Overflow, Overflow{Class: cl.Name, Take: w.take[cl.Name], Held: w.holds[cl.Name], After: cl.After})
		}
	}
	reason := map[string]string{}
	for i, df := range w.defers {
		reason[df.Class] = df.Reason
		oldest := int64(0)
		for _, j := range w.demand {
			if j.class == df.Class {
				oldest = secs(t - j.firstSeen)
				break
			}
		}
		out.Waiting = append(out.Waiting, WaitingState{Class: df.Class, Jobs: df.Count, Reason: df.Reason, Order: i + 1, Priority: df.Priority, OldestSeconds: oldest})
	}
	line := d.lineOf(v.ops, w, t)
	for _, j := range w.demand {
		kd := j.spec.k()
		jw := JobWait{Class: j.class, Repo: demoRepo, Workflow: kd.workflow, Job: kd.job, RunID: j.spec.runID, WaitedSeconds: secs(t - j.firstSeen), Reason: reason[j.class],
			ID: j.id(), GithubJobID: j.githubID()}
		lj := line[j.id()]
		jw.Level, jw.Ranked, jw.Position = cond(lj.level() == "normal", "", lj.level()), lj.Ranked, lj.place
		jw.Expedited = v.ops.expeditedAt(j.id(), t)
		if e, ok := v.expect[j.id()]; ok {
			jw.Expect = &JobExpect{InSeconds: int64(e.In.Seconds()), Host: e.Host, After: e.After}
		}
		out.WaitingJobs = append(out.WaitingJobs, jw)
	}
	pv := v.priority()
	out.Mode, out.Expedited = pv.Mode, pv.Expedited
	for _, b := range w.back {
		kd := b.job.spec.k()
		out.TakenBack = append(out.TakenBack, JobWait{Class: b.job.class, Repo: demoRepo, Workflow: kd.workflow, Job: kd.job, RunID: b.job.spec.runID,
			WaitedSeconds: secs(b.left - b.job.firstSeen), LeftAt: v.ts(b.left)})
	}
	keys := slices.Collect(func(yield func(string) bool) {
		for k := range w.measured {
			if !yield(k) {
				return
			}
		}
	})
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(w.lastRun[b], w.lastRun[a]), cmp.Compare(a, b)) })
	for _, k := range keys {
		runs := w.measured[k]
		parts := splitKey(k)
		e := size.Learn(runs, size.Estimate{})
		last := runs[len(runs)-1]
		out.Sizes = append(out.Sizes, JobSize{Repo: parts[0], Workflow: parts[1], Job: parts[2], Platform: parts[3], EstCPUs: e.CPUs, EstMem: e.MemBytes,
			LastCPUs: last.CPUs, LastMem: last.MemBytes, TopCPUs: e.TopCPUs, TopMem: e.TopMem, Runs: e.Runs})
	}
	return out
}

func splitKey(k string) [4]string {
	var out [4]string
	for i := range 3 {
		j := 0
		for j < len(k) && k[j] != '|' {
			j++
		}
		out[i], k = k[:j], k[min(j+1, len(k)):]
	}
	out[3] = k
	return out
}

func (v demoView) live() Status {
	st := Status{Hosts: map[string]*api.Snapshot{}, Waiting: []Waiting{}, Closed: map[string]string{}, Links: map[string]Link{}}
	s := v.state()
	for _, h := range s.Hosts {
		ln := Link{SeenAt: v.now.Add(-time.Duration(*h.SeenSeconds) * time.Second).UTC(), RTTMs: h.RTTMs, Agent: h.Agent, LastDrop: h.LastDrop}
		st.Links[h.Name] = ln
		if !*h.Reachable {
			st.Hosts[h.Name] = nil
			continue
		}
		snap := &api.Snapshot{Host: h.Name, Kind: h.Kind, Version: h.Agent, OS: h.OS, CPUs: h.CPUs, MemBytes: h.MemBytes, BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem,
			NonRunnerCPU: h.OtherCPUs, NonRunnerMem: h.OtherMem, SampledAt: v.now.UTC(), DiskTotal: 460 * gb, DiskFree: int64(*h.DiskFreePct / 100 * float64(460*gb))}
		if h.CPUPressure != nil {
			snap.PSI = map[string]float64{"cpu": *h.CPUPressure, "memory": *h.MemPressure, "io": *h.IOPressure}
		}
		for _, r := range s.Runners {
			if r.Host == h.Name {
				snap.Runners = append(snap.Runners, api.Runner{Name: r.Name, Class: r.Class, Slot: r.Slot, CPUs: r.EstCPUs, MemBytes: r.EstMem, State: "running",
					CPUSec: r.CPUSec, CPUNow: r.CPUNow, MemCurrent: r.MemBytes, MemWork: r.MemBytes, LimitMem: r.MemLimit})
			}
		}
		st.Hosts[h.Name] = snap
		if h.Closed != "" {
			st.Closed[h.Name] = h.Closed
		}
	}
	for _, w := range s.Waiting {
		st.Waiting = append(st.Waiting, Waiting{Class: w.Class, Jobs: w.Jobs, Reason: w.Reason, Priority: w.Priority})
	}
	st.Overflow = s.Overflow
	return st
}

func (v demoView) jobs() []ledger.FinishedJob {
	out := []ledger.FinishedJob{}
	for i := len(v.w.finished) - 1; i >= 0 && len(out) < 50; i-- {
		f := v.w.finished[i]
		r := f.r
		j := ledger.FinishedJob{Runner: r.name, Class: r.class, Host: r.host, Job: r.job.spec.kind, Repo: demoRepo, RunID: r.job.spec.runID,
			State: "done", Result: f.result, Reason: "job completed", CreatedAt: v.ts(r.created), StartedAt: v.ts(r.started), FinishedAt: v.ts(f.finished),
			WaitSec: r.started - r.job.firstSeen, RunSec: f.finished - r.started, ProvisionMs: int64(r.prov * 1000), CPUSec: math.Round(r.cpuSec*10) / 10,
			MemWorkPeak: r.memPeak, MemPeak: r.memPeak + r.memPeak/3, EstCPUs: r.est.CPUs, EstMem: r.est.MemBytes}
		if f.oom {
			j.OOMKills = 1
		}
		out = append(out, j)
	}
	return out
}

func (v demoView) job(runner string) ([]ledger.Decision, string) {
	ds := []ledger.Decision{}
	if st, ok := v.w.starts[runner]; ok {
		ds = append(ds, ledger.Decision{At: v.ts(st.at), Action: st.action, Host: st.host, Reason: st.reason})
		return ds, ""
	}
	for _, f := range v.w.finished {
		if f.r.name != runner {
			continue
		}
		r := f.r
		ds = append(ds, ledger.Decision{At: v.ts(r.created), Action: "start", Host: r.host, Reason: fmt.Sprintf("takes %.1fcpu/%s for %s (%s)", r.est.CPUs, gib(r.est.MemBytes), r.job.spec.kind, r.est.Source())},
			ledger.Decision{At: v.ts(f.finished), Action: "stop", Host: r.host, Reason: "job completed"})
		tail := fmt.Sprintf("√ Connected to GitHub\nCurrent runner version: '2.338.2'\nListening for Jobs\nRunning job: %s\nJob %s completed with result: %s\n",
			r.job.spec.kind, r.job.spec.kind, map[string]string{"succeeded": "Succeeded", "failed": "Failed", "canceled": "Canceled"}[f.result])
		return ds, tail
	}
	return ds, ""
}

// series is a made-up last day: busy in the working hours, with the sim's own last step.
func (v demoView) series() Series {
	const step, steps = 10 * time.Minute, 144
	r := rand.New(rand.NewPCG(uint64(v.d.seed), 99))
	s := Series{HostCPUs: map[string][]float64{}}
	end := v.now.Truncate(step)
	day := func(i int) float64 { // 0 at night, 1 in the afternoon
		h := float64((i+18)%144) / 6 // hours since 06:00, as if now were mid-afternoon
		return math.Max(0, math.Sin(math.Pi*h/14))
	}
	st := v.state()
	for _, h := range demoHosts {
		line := make([]float64, steps)
		top := cmp.Or(h.budgetCPUs, h.cpus) * 0.8
		for i := range steps {
			line[i] = math.Round(top*day(i)*(0.5+0.5*r.Float64())*10) / 10
		}
		for _, hs := range st.Hosts {
			if hs.Name == h.name {
				line[steps-1] = math.Round(hs.UsedCPUs*10) / 10
			}
		}
		s.HostCPUs[h.name] = line
	}
	for i := range steps {
		s.T = append(s.T, end.Add(-time.Duration(steps-1-i)*step).Unix())
		wq := int(math.Round(day(i) * 6 * r.Float64() * r.Float64() * 2))
		s.Waiting = append(s.Waiting, wq)
		med := -1.0
		if day(i) > 0.1 {
			med = math.Round(4 + 80*r.Float64()*r.Float64()*day(i))
		}
		s.WaitMedianSec = append(s.WaitMedianSec, med)
	}
	s.Waiting[steps-1] = len(st.WaitingJobs)
	return s
}
