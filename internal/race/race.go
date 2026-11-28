// Package race runs one stream of jobs on the same four hosts several ways:
// on fixed runners, where each host has a set number of runners and a runner
// takes the next job of its labels whatever its size (three ways to size
// them), and on pumasi, where the real placement (decide.Decide) starts a
// runner for each job by what the job was measured to use (size.Learn).
// Anyone can make the numbers again:
//
//	go run ./internal/race/cmd
//
// It is a simulation: the jobs are the demo's (internal/controller/demo.go)
// and nothing fails. Where the jobs on a host want more cores than it has,
// they all slow down in proportion (see pace), and cpu pressure rises as
// Linux would report it, roughly.
package race

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/size"
)

const (
	gb   = 1 << 30
	Tick = 2.0 // seconds between placement rounds, as the controller's default
)

// Host is a machine both setups run on.
type Host struct {
	Name, Kind, OS string
	CPUs           float64
	Mem            int64
	// The share a desktop lends (0 = the whole machine).
	BudgetCPUs float64
	BudgetMem  int64
	// What the rest of the machine uses.
	OtherCPU float64
	OtherMem int64
}

// Hosts are the demo's four machines.
var Hosts = []Host{
	{Name: "linux-server", Kind: "server", OS: "linux", CPUs: 32, Mem: 128 * gb, OtherCPU: 1.5, OtherMem: 14 * gb},
	{Name: "mac-mini", Kind: "server", OS: "macos", CPUs: 10, Mem: 32 * gb, BudgetCPUs: 8, BudgetMem: 24 * gb, OtherCPU: 1.1, OtherMem: 6 * gb},
	{Name: "wsl-desktop", Kind: "desktop", OS: "linux", CPUs: 16, Mem: 64 * gb, BudgetCPUs: 10, BudgetMem: 32 * gb, OtherCPU: 3, OtherMem: 20 * gb},
	{Name: "linux-spare", Kind: "server", OS: "linux", CPUs: 16, Mem: 32 * gb, OtherCPU: 0.6, OtherMem: 3 * gb},
}

func (h Host) decide() decide.Host {
	return decide.Host{Name: h.Name, Kind: h.Kind, OS: h.OS, Up: true, CPUs: h.CPUs, MemBytes: h.Mem,
		BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem, NonRunnerCPU: h.OtherCPU, NonRunnerMem: h.OtherMem,
		KeepMem: min(max(h.Mem/10, gb), 4*gb)}
}

// phys is the memory, GiB, the machine has besides its other use.
func (h Host) phys() float64 { return float64(h.Mem-h.OtherMem) / gb }

// Room is what the host can give runners when none runs: the same for both setups.
func (h Host) Room() (cpu float64, mem int64) {
	c, m := decide.Free(h.decide(), 0, 0)
	return c, int64(m)
}

// Class is a pumasi class: a label and the size it guesses for a job it has never measured.
type Class struct {
	Name, OS string
	CPUs     float64
	Mem      int64
	Hosts    []string // empty = every host of its OS
}

// Classes are the demo's, less its overflow classes: the light jobs go with the pr checks.
var Classes = []Class{
	{Name: "pr-checks", OS: "linux", CPUs: 2, Mem: 4 * gb},
	{Name: "e2e", OS: "linux", CPUs: 4, Mem: 12 * gb},
	{Name: "build", OS: "linux", CPUs: 6, Mem: 16 * gb, Hosts: []string{"linux-server"}},
	{Name: "deploy", OS: "linux", CPUs: 2, Mem: 4 * gb},
	{Name: "macos", OS: "macos", CPUs: 4, Mem: 8 * gb},
}

func classOf(name string) Class {
	i := slices.IndexFunc(Classes, func(c Class) bool { return c.Name == name })
	return Classes[i]
}

// on reports whether a class's jobs may run on host h.
func (c Class) on(h Host) bool {
	return c.OS == h.OS && (len(c.Hosts) == 0 || slices.Contains(c.Hosts, h.Name))
}

// Kind is a job of the repo and how it runs: seconds, cores, GiB at its peak.
type Kind struct {
	Name, Class, Workflow string
	Dur, CPU, Mem         [2]float64
}

// Kinds are the demo's jobs that a busy hour of pull requests runs.
var Kinds = []Kind{
	{"lint", "pr-checks", "pr-checks.yml", [2]float64{40, 70}, [2]float64{.5, .8}, [2]float64{.9, 1.3}},
	{"unit-tests", "pr-checks", "pr-checks.yml", [2]float64{90, 150}, [2]float64{1.6, 2.2}, [2]float64{2.8, 3.6}},
	{"typecheck", "pr-checks", "pr-checks.yml", [2]float64{50, 80}, [2]float64{.9, 1.2}, [2]float64{2, 2.5}},
	{"e2e (chromium)", "e2e", "pr-checks.yml", [2]float64{150, 230}, [2]float64{3, 3.6}, [2]float64{9, 11}},
	{"e2e (webkit)", "e2e", "pr-checks.yml", [2]float64{140, 210}, [2]float64{2.6, 3}, [2]float64{8, 9.5}},
	{"build-image", "build", "pr-checks.yml", [2]float64{100, 160}, [2]float64{4.5, 5.5}, [2]float64{10, 13}},
	{"label-check", "pr-checks", "pr-labels.yml", [2]float64{12, 20}, [2]float64{.15, .25}, [2]float64{.25, .35}},
	{"size-report", "pr-checks", "pr-labels.yml", [2]float64{20, 35}, [2]float64{.3, .5}, [2]float64{.5, .7}},
	{"ios-build", "macos", "ios.yml", [2]float64{150, 200}, [2]float64{3.2, 3.8}, [2]float64{6.5, 7.5}},
	{"xcode-tests", "macos", "ios.yml", [2]float64{90, 140}, [2]float64{2.2, 2.8}, [2]float64{4.5, 5.5}},
	{"deploy-api", "deploy", "deploy-prod.yml", [2]float64{60, 90}, [2]float64{2.2, 2.8}, [2]float64{4.5, 5.5}},
	{"deploy-web", "deploy", "deploy-prod.yml", [2]float64{50, 70}, [2]float64{1.8, 2.2}, [2]float64{3.5, 4.5}},
}

// Later are jobs a repo gains after the fixed runners were sized and
// pumasi last measured (Drift.New): nobody sized anything for them.
var Later = []Kind{
	{"integration", "pr-checks", "pr-checks.yml", [2]float64{120, 200}, [2]float64{2, 3}, [2]float64{5, 7}},
}

func kindOf(name string) Kind {
	if i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Name == name }); i >= 0 {
		return Kinds[i]
	}
	return Later[slices.IndexFunc(Later, func(k Kind) bool { return k.Name == name })]
}

// Job is one job as it arrives and runs. CPU is the cores it uses at full
// pace, Mem the GiB it peaks at.
type Job struct {
	ID   int     `json:"id"`
	Kind string  `json:"kind"`
	At   float64 `json:"at"`
	Dur  float64 `json:"dur"`
	CPU  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
	// Busy: it uses its cpu from the first second to the last (a compile, a
	// test suite that saturates its cores) instead of coming up to it.
	Busy bool `json:"busy,omitempty"`
	// After: the job (ID) it needs; it comes Delay seconds after that one
	// ends (0 = it comes at At). Run: the workflow run it is of (0 = none).
	After int     `json:"after,omitempty"`
	Delay float64 `json:"delay,omitempty"`
	Run   int     `json:"run,omitempty"`
}

// use is what the job uses el seconds after it started: it comes up to its
// pace over the first fifth of its run.
func (j Job) use(el float64) (cpu, mem float64) {
	r := math.Min(1, el/math.Max(10, 0.2*j.Dur))
	if j.Busy {
		return j.CPU, j.Mem * (0.3 + 0.7*r)
	}
	return j.CPU * (0.4 + 0.6*r), j.Mem * (0.3 + 0.7*r)
}

// alone is what pumasi records of a run with the cores to itself: its
// average cores and its peak memory.
func (j Job) alone() size.Sample {
	var sec float64
	for el := 0.0; el < j.Dur; el += Tick {
		c, _ := j.use(el)
		sec += c * math.Min(Tick, j.Dur-el)
	}
	return size.Sample{CPUs: sec / j.Dur, MemBytes: int64(j.Mem * gb), Secs: j.Dur}
}

// Scenario is the stream of jobs and how pumasi meets it.
type Scenario struct {
	Seed   uint64     `json:"seed"`
	Pushes int        `json:"pushes"` // pull-request pushes, each starting the pr workflows
	Every  [2]float64 `json:"every"`  // seconds between pushes, drawn evenly
	// Provision: seconds a pumasi runner takes from its start to taking a
	// job. A fixed runner is up already.
	Provision float64 `json:"provision"`
	// Cold: pumasi has measured nothing before the stream; otherwise it has
	// seen each job's last runs (another day like this one).
	Cold bool `json:"cold"`
	// Busy: every job uses its cpu from start to end, the worst case for
	// sharing cores.
	Busy bool `json:"busy,omitempty"`
	// Tails: run times as CI has them, with a long tail: one run in ten takes
	// twice as long (a retry, a cold cache), one in twenty fails a third of
	// the way in.
	Tails bool `json:"tails,omitempty"`
	// CPUOvercommit is pumasi's cpuOvercommit (1 = none).
	CPUOvercommit float64 `json:"cpuOvercommit"`
	// Warm is pumasi's warm: the most runners a class keeps up without a
	// job (0 = none), from its arrivals of the last decide.WarmWindow.
	Warm int `json:"warm,omitempty"`
	// Drift: how the jobs changed since the fixed runners were sized and
	// pumasi last measured them (zero: they did not).
	Drift Drift `json:"drift,omitempty"`
	// Needs: jobs that need another come when it ends, as `needs:` makes
	// them: the e2e legs after the unit tests, the size report after lint,
	// the web deploy after the api's. Otherwise every job of a push comes
	// with it.
	Needs bool `json:"needs,omitempty"`
	// Native: a pumasi runner takes as long to come up as native runners
	// did on real hosts (see provision), not Provision every time.
	Native bool `json:"native,omitempty"`
}

// Drift is a change in a repo's jobs, of the kinds CI goes through: the
// jobs grow, the heavy ones come more often, a new job appears, the PR rush
// gives way to nightly batches.
type Drift struct {
	Name string `json:"name,omitempty"`
	// Grow: every job takes this many times as long, as many cores and as
	// much memory (0 = as before).
	Grow float64 `json:"grow,omitempty"`
	// Heavy: the share of pushes that run the optional heavy jobs, the
	// webkit e2e and the image build (0 = as before: 40% and 35%).
	Heavy float64 `json:"heavy,omitempty"`
	// New: each push also runs a job of Later.
	New bool `json:"new,omitempty"`
	// Nightly: half the pushes, and a nightly matrix (eight e2e legs, two
	// image builds) at the start and in the middle.
	Nightly bool `json:"nightly,omitempty"`
}

// Before is sc as it was before its drift: what the fixed runners were
// sized by and what pumasi measured.
func (sc Scenario) Before() Scenario {
	sc.Drift = Drift{}
	return sc
}

// Film is the scenario the film shows: a busy quarter hour.
var Film = Scenario{Seed: 7, Pushes: 18, Every: [2]float64{35, 55}, Provision: 8, CPUOvercommit: config.DefaultCPUOvercommit, Warm: config.DefaultWarm}

func (sc Scenario) draw(r *rand.Rand, at float64, kind string) Job {
	k := kindOf(kind)
	u := func(v [2]float64) float64 { return v[0] + r.Float64()*(v[1]-v[0]) }
	j := Job{Kind: kind, At: at, Dur: math.Round(u(k.Dur)), CPU: round2(u(k.CPU)), Mem: round2(u(k.Mem)), Busy: sc.Busy}
	if g := sc.Drift.Grow; g > 0 {
		j.Dur, j.CPU, j.Mem = math.Round(j.Dur*g), round2(j.CPU*g), round2(j.Mem*g)
	}
	if sc.Tails {
		switch x := r.Float64(); {
		case x < 0.1:
			j.Dur *= 2
		case x < 0.15:
			j.Dur = math.Round(j.Dur / 3)
		}
	}
	return j
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Stream is the scenario's jobs, in the order they arrive: the pushes, an
// iOS build every two minutes or so while they come, and one deploy.
func Stream(sc Scenario) []Job {
	r := rand.New(rand.NewPCG(sc.Seed, 1))
	rn := rand.New(rand.NewPCG(sc.Seed, 3)) // the delays of needs, apart: the rest of the stream stays the same
	var out []Job
	run := 0
	add := func(at float64, kind string) int {
		j := sc.draw(r, at, kind)
		j.Run, j.ID = run, len(out)+1
		out = append(out, j)
		return len(out)
	}
	// need makes job i (an ID add gave) come after job of ID after, when Needs.
	need := func(i, after int) {
		if sc.Needs && after > 0 {
			out[i-1].After, out[i-1].Delay = after, math.Round((1.5+rn.Float64()*3)*10)/10
		}
	}
	at := 5.0
	webkit, build, pushes := 0.4, 0.35, sc.Pushes
	if h := sc.Drift.Heavy; h > 0 {
		webkit, build = h, h
	}
	if sc.Drift.Nightly {
		pushes = sc.Pushes / 2
	}
	nightly := func(at float64) {
		for i := range 8 {
			add(at+float64(i%3), []string{"e2e (chromium)", "e2e (webkit)"}[i%2])
		}
		add(at, "build-image")
		add(at+1, "build-image")
	}
	for i := range pushes {
		run = i + 1
		if sc.Drift.Nightly && (i == 0 || i == pushes/2) {
			nightly(at)
		}
		o := func() float64 { return math.Round((at+r.Float64()*3)*10) / 10 }
		lint := add(o(), "lint")
		unit := add(o(), "unit-tests")
		add(o(), "typecheck")
		need(add(o(), "e2e (chromium)"), unit)
		if r.Float64() < webkit {
			need(add(o(), "e2e (webkit)"), unit)
		}
		if r.Float64() < build {
			add(o(), "build-image")
		}
		add(o(), "label-check")
		if r.Float64() < 0.6 {
			need(add(o(), "size-report"), lint)
		}
		if sc.Drift.New {
			add(o(), Later[0].Name)
		}
		at += sc.Every[0] + r.Float64()*(sc.Every[1]-sc.Every[0])
	}
	for t, i := 20.0, 0; t < at; t, i = t+105+r.Float64()*30, i+1 {
		run = 1000 + i
		add(math.Round(t), []string{"ios-build", "xcode-tests"}[i%2])
	}
	run = 2000
	api := add(math.Round(at/2), "deploy-api")
	need(add(math.Round(at/2)+1, "deploy-web"), api)
	slices.SortStableFunc(out, func(a, b Job) int { return cmp.Compare(a.At, b.At) })
	id := map[int]int{} // as added -> in arrival order
	for i := range out {
		id[out[i].ID] = i + 1
	}
	for i := range out {
		out[i].ID, out[i].After = i+1, id[out[i].After]
	}
	return out
}

// ---------------------------------------------------------------- results

// Running is a job on a host in a frame. Res is what it holds of the host:
// for a fixed runner its whole slot, for pumasi its runner's reservation.
type Running struct {
	ID       int     `json:"id"`
	Kind     string  `json:"kind"`
	Slot     int     `json:"slot,omitempty"` // fixed: which runner of the host (1-based)
	ResCPU   float64 `json:"resCpu"`
	ResMem   float64 `json:"resMem"` // GiB
	CPU      float64 `json:"cpu"`
	Mem      float64 `json:"mem"`
	Starting bool    `json:"starting,omitempty"` // pumasi: the runner is coming up
}

// HostFrame is a host at a moment.
type HostFrame struct {
	Name string    `json:"name"`
	Jobs []Running `json:"jobs"`
	// Pace: the share of the cores everyone wanted that they got (1 = no wait).
	Pace float64 `json:"pace"`
	PSI  float64 `json:"psi,omitempty"` // cpu pressure, some avg10 (linux)
}

// Frame is the world after a placement round.
type Frame struct {
	T       float64     `json:"t"`
	Waiting []int       `json:"waiting"` // ids, oldest first
	Done    int         `json:"done"`
	Hosts   []HostFrame `json:"hosts"`
}

// Summary is a run in numbers. Waits are from a job's arrival to its start;
// busy is from the first arrival to the last job's end.
type Summary struct {
	Jobs       int     `json:"jobs"`
	MedianWait float64 `json:"medianWait"`
	P90Wait    float64 `json:"p90Wait"`
	MaxWait    float64 `json:"maxWait"`
	MeanWait   float64 `json:"meanWait"`
	// Drained: when the last job started. Done: when the last ended.
	Drained float64 `json:"drained"`
	Done    float64 `json:"done"`
	// PeakWaiting: the most jobs waiting at once.
	PeakWaiting int `json:"peakWaiting"`
	// The share of the hosts' room (Host.Room) the jobs used, on average
	// while any job ran: memory and cpu.
	MemUse float64 `json:"memUse"`
	CPUUse float64 `json:"cpuUse"`
	// The share of that room held (a fixed runner's slot, a pumasi runner's reservation).
	MemHeld float64 `json:"memHeld"`
	// Slowdown: how much longer jobs ran than they would alone, mean and p90
	// (1.1 = a tenth longer), from sharing cores.
	MeanSlowdown float64 `json:"meanSlowdown"`
	P90Slowdown  float64 `json:"p90Slowdown"`
	// PeakPSI: the highest cpu pressure on any linux host.
	PeakPSI float64 `json:"peakPsi"`
	// OverRunner: jobs whose peak memory passed what their runner was given:
	// a fixed runner's slot, the size pumasi placed it with. A fixed runner
	// with that as its memory limit loses the job; one without it squeezes
	// the host. pumasi lets the job run and counts what it uses.
	OverRunner int `json:"overRunner"`
	// HostMemPeak: the most memory the jobs used on any host, as a share of
	// its room; HostOverSecs: how long some host's jobs used more than it.
	HostMemPeak  float64 `json:"hostMemPeak"`
	HostOverSecs float64 `json:"hostOverSecs"`
	// Against the machine itself, budget or not: MemPressSecs, how long the
	// jobs on some host used memory the host keeps back (KeepMem: where the
	// kernel starts reclaiming and swapping); OOM, how many times the jobs
	// on a host came to use more than the machine has besides its other use,
	// and OOMSecs how long.
	MemPressSecs float64 `json:"memPressSecs"`
	OOM          int     `json:"oom"`
	OOMSecs      float64 `json:"oomSecs"`
}

// Result is one setup's run.
type Result struct {
	Name  string    `json:"name"`
	Setup string    `json:"setup"` // in words
	Waits []float64 `json:"waits"` // by job, in stream order
	// Arrived: when each job came (Job.At, or later for one that needs another).
	Arrived []float64 `json:"arrived,omitempty"`
	Summary Summary   `json:"summary"`
	Frames  []Frame   `json:"frames,omitempty"`
	// Told: pumasi only, what it said of each job's start while it waited.
	Told []Told `json:"told,omitempty"`
	// Idle: pumasi only, what runners without a job cost.
	Idle Idle `json:"idle,omitempty"`
}

// Idle is what pumasi's runners without a job cost: those coming up, those
// listening for a job, and those kept warm.
type Idle struct {
	// WarmStarted: runners started for no job, to keep warm; WarmServed: of
	// them, those a job took.
	WarmStarted int `json:"warmStarted"`
	WarmServed  int `json:"warmServed"`
	// Stopped: runners stopped before any job took them (the idle timeout, a
	// job of another class that needed the room, a host that closed).
	Stopped int `json:"stopped"`
	// What runners without a job held, on average over the run (cores, GiB),
	// and the most memory at once.
	HeldCPU float64 `json:"heldCpu"`
	HeldMem float64 `json:"heldMem"`
	PeakMem float64 `json:"peakMem"`
	// Delayed: jobs that waited a round in which the same placement, with
	// the runners kept warm and still without a job gone, would have started
	// them; DelayedSecs: those rounds, seconds in all; DelayedMax: the most
	// for one job. What runners kept warm cost the jobs, rather than room.
	Delayed     int     `json:"delayed"`
	DelayedSecs float64 `json:"delayedSecs"`
	DelayedMax  float64 `json:"delayedMax"`
}

// Told is what pumasi said, at one moment, of when a waiting job would start.
type Told struct {
	Job    int     `json:"job"` // index in the stream
	At     float64 `json:"at"`  // when it said so
	Starts float64 `json:"starts"`
	First  bool    `json:"first,omitempty"` // the first time it said anything of the job
}

func quantile(v []float64, p float64) float64 {
	w := slices.Clone(v)
	slices.Sort(w)
	return w[min(len(w)-1, max(0, int(math.Ceil(p*float64(len(w))))-1))]
}

func mean(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func summarize(res *Result, jobs []Job, ends []float64) {
	s := Summary{Jobs: len(jobs)}
	w := res.Waits
	s.MedianWait, s.P90Wait, s.MaxWait, s.MeanWait = quantile(w, 0.5), quantile(w, 0.9), slices.Max(w), math.Round(mean(w))
	slow := make([]float64, len(jobs))
	for i, j := range jobs {
		came := j.At
		if res.Arrived != nil {
			came = res.Arrived[i]
		}
		s.Drained = math.Max(s.Drained, came+res.Waits[i])
		slow[i] = (ends[i] - came - res.Waits[i]) / j.Dur
	}
	s.Done = math.Round(slices.Max(ends))
	s.MeanSlowdown, s.P90Slowdown = round2(mean(slow)), round2(quantile(slow, 0.9))
	var roomCPU, roomMem float64
	for _, h := range Hosts {
		c, m := h.Room()
		roomCPU, roomMem = roomCPU+c, roomMem+float64(m)/gb
	}
	hostRoom, phys, keep := map[string]float64{}, map[string]float64{}, map[string]float64{}
	for _, h := range Hosts {
		_, m := h.Room()
		hostRoom[h.Name] = float64(m) / gb
		phys[h.Name], keep[h.Name] = h.phys(), float64(h.decide().KeepMem)/gb
	}
	inOOM := map[string]bool{}
	var n, cpu, mem, held float64
	for _, f := range res.Frames {
		s.PeakWaiting = max(s.PeakWaiting, len(f.Waiting))
		var c, m, hm float64
		any, over, press, oom := false, false, false, false
		for _, h := range f.Hosts {
			var used float64
			for _, r := range h.Jobs {
				c, m, hm = c+r.CPU, m+r.Mem, hm+r.ResMem
				any = any || !r.Starting
				used += r.Mem
			}
			s.PeakPSI = math.Max(s.PeakPSI, h.PSI)
			s.HostMemPeak = math.Max(s.HostMemPeak, used/hostRoom[h.Name])
			over = over || used > hostRoom[h.Name]+1e-9
			press = press || used > phys[h.Name]-keep[h.Name]+1e-9
			o := used > phys[h.Name]+1e-9
			if o && !inOOM[h.Name] {
				s.OOM++
			}
			inOOM[h.Name], oom = o, oom || o
		}
		if over {
			s.HostOverSecs += Tick
		}
		if press {
			s.MemPressSecs += Tick
		}
		if oom {
			s.OOMSecs += Tick
		}
		if any {
			n, cpu, mem, held = n+1, cpu+c, mem+m, held+hm
		}
	}
	if n > 0 {
		s.CPUUse, s.MemUse, s.MemHeld = cpu/n/roomCPU, mem/n/roomMem, held/n/roomMem
	}
	s.CPUUse, s.MemUse, s.MemHeld, s.PeakPSI, s.HostMemPeak = round2(s.CPUUse), round2(s.MemUse), round2(s.MemHeld), math.Round(s.PeakPSI), round2(s.HostMemPeak)
	s.OverRunner = res.Summary.OverRunner
	res.Summary = s
}

// ---------------------------------------------------------------- the machines

// run is a job running on a host and how far it has got.
type run struct {
	job      int // index in the stream
	host     string
	started  float64
	work     float64 // seconds of its run done, at full pace
	cpuSec   float64
	cpu, mem float64 // what it used over the last tick
	end      float64 // when it ended (0 = still running)
}

// Pressure: cpu pressure ("some", the share of time something runnable
// waits for a core) is taken as the share of the wanted cpu that is not
// served, with the wants 15% burstier than their average: it starts rising
// near 87% busy, is 13% at 100% and 42% at 150%. avg10 smooths it over
// ten seconds, as the kernel does.
const (
	burst = 1.15
	avg10 = 10.0
)

// machines is the state of the hosts that outlives a tick: their pressure.
type machines struct{ psi, mem map[string]float64 }

func newMachines() *machines { return &machines{psi: map[string]float64{}, mem: map[string]float64{}} }

// psiMem is memory pressure as pumasi would read it on a linux host: none
// while the jobs leave the memory the host keeps back, rising through it to
// 100 when they take it all (reclaim, then swap).
func (m *machines) psiMem(h Host) float64 {
	k := float64(h.decide().KeepMem) / gb
	return math.Round(100*math.Min(1, math.Max(0, (m.mem[h.Name]-(h.phys()-k))/k))*100) / 100
}

// pace moves every running job on by one tick. Where the jobs on a host and
// the rest of the machine want more cores than it has, each gets the same
// share of what it wants (roughly the kernel's fair scheduler), and a job's
// run stretches by as much. It returns each host's pace, and marks a job
// that got to its end.
func (m *machines) pace(t float64, runs []*run, jobs []Job) map[string]float64 {
	out := map[string]float64{}
	for _, h := range Hosts {
		want := h.OtherCPU
		for _, r := range runs {
			if r.host == h.Name {
				c, _ := jobs[r.job].use(r.work)
				want += c
			}
		}
		f := math.Min(1, h.CPUs/want)
		out[h.Name] = f
		if h.OS == "linux" {
			now := 100 * math.Max(0, 1-h.CPUs/(burst*want))
			m.psi[h.Name] += (now - m.psi[h.Name]) * (1 - math.Exp(-Tick/avg10))
		}
		m.mem[h.Name] = 0
		for _, r := range runs {
			if r.host != h.Name {
				continue
			}
			j := jobs[r.job]
			c, mem := j.use(r.work)
			m.mem[h.Name] += mem
			step := Tick * f
			if r.work+step >= j.Dur {
				r.end = t - Tick + Tick*(j.Dur-r.work)/step
				step = j.Dur - r.work
			}
			r.work += step
			r.cpu, r.mem = c*f, mem
			r.cpuSec += c * step
		}
	}
	return out
}

func start(ji int, host string, t float64, jobs []Job) *run {
	r := &run{job: ji, host: host, started: t}
	r.cpu, r.mem = jobs[ji].use(0)
	return r
}

// measure is what pumasi records of a finished run: its cores on average
// over the time it took, and its peak memory.
func (r *run) measure(jobs []Job) size.Sample {
	return size.Sample{CPUs: r.cpuSec / (r.end - r.started), MemBytes: int64(jobs[r.job].Mem * gb), Secs: r.end - r.started}
}

func (m *machines) frame(f *Frame, pace map[string]float64) {
	for i := range f.Hosts {
		h := &f.Hosts[i]
		h.Pace, h.PSI = round2(pace[h.Name]), round2(m.psi[h.Name])
	}
}

// ---------------------------------------------------------------- fixed runners

// Slot is one fixed runner: the host it is on, the classes it takes jobs
// of, and the size it is set up for.
type Slot struct {
	Host    string   `json:"host"`
	Classes []string `json:"classes"`
	Small   bool     `json:"small,omitempty"` // a split setup's runner for the small jobs only
	CPU     float64  `json:"cpu"`
	Mem     float64  `json:"mem"` // GiB
}

// Fixed is a setup of fixed runners.
type Fixed struct {
	Name  string `json:"name"`
	Setup string `json:"setup"`
	Slots []Slot `json:"slots"`
	// Small: the jobs a split setup sends to its small runners ("" = not split).
	Small []string `json:"small,omitempty"`
}

// takes reports whether runner s takes a job of kind k.
func (f Fixed) takes(s Slot, k Kind) bool {
	if len(f.Small) > 0 && f.small(k) != s.Small {
		return false
	}
	return s.Small || slices.Contains(s.Classes, k.Class)
}

// small reports whether a split setup sends k to its small runners: the
// jobs it named, and a job added later to a class whose jobs it named — the
// new job's runs-on is its siblings'.
func (f Fixed) small(k Kind) bool {
	if slices.Contains(f.Small, k.Name) {
		return true
	}
	return slices.ContainsFunc(Later, func(l Kind) bool { return l.Name == k.Name }) &&
		slices.ContainsFunc(f.Small, func(n string) bool { return kindOf(n).Class == k.Class })
}

// peak is the most a kind's job uses.
func peak(k Kind) (cpu, mem float64) { return k.CPU[1], k.Mem[1] }

// largest is the size of the largest job of the kinds that may run on h.
func largest(h Host, keep func(Kind) bool) (cpu, mem float64) {
	for _, k := range Kinds {
		if classOf(k.Class).on(h) && keep(k) {
			kc, km := peak(k)
			cpu, mem = math.Max(cpu, kc), math.Max(mem, math.Ceil(km))
		}
	}
	return cpu, mem
}

func classesOn(h Host) []string {
	var out []string
	for _, c := range Classes {
		if c.on(h) {
			out = append(out, c.Name)
		}
	}
	return out
}

// Uniform is the usual setup: each host runs as many runners as fit the
// largest job that may land on it (its peak memory, rounded up to a whole
// GiB, no headroom), so that no mix of jobs runs it out of memory. Each runner
// takes every class the host takes. CPU is not held to it: the jobs may
// share cores, which costs them nothing here (see Summary.PeakCPU). A
// desktop's runners stay within its budget in cpu too, as pumasi's do.
func Uniform() Fixed {
	f := Fixed{Name: "fixed", Setup: "each host runs as many runners as fit its largest job's peak memory, rounded up to a whole GB, no headroom; a runner takes any job of the host's labels"}
	for _, h := range Hosts {
		cpu, mem := largest(h, func(Kind) bool { return true })
		rc, rm := h.Room()
		n := int(float64(rm) / gb / mem)
		if h.BudgetCPUs > 0 {
			n = min(n, int(rc/cpu))
		}
		for range n {
			f.Slots = append(f.Slots, Slot{Host: h.Name, Classes: classesOn(h), CPU: cpu, Mem: mem})
		}
	}
	return f
}

// Split is the setup of someone who tuned it: small runners for the pr
// checks that need 4 GiB at most and large ones for the rest, each linux
// host's memory split between the two in proportion to what the stream asks
// of each (known in advance, which nobody can).
func Split(jobs []Job) Fixed {
	f := Fixed{Name: "fixed-split", Setup: "small runners (4 GB) for the small pr checks, large ones for the rest; each linux host's memory split between them by what this stream asks of each, known in advance"}
	const smallMem = 4.0
	small := func(k Kind) bool { _, m := peak(k); return k.Class == "pr-checks" && m <= smallMem }
	for _, k := range Kinds {
		if small(k) {
			f.Small = append(f.Small, k.Name)
		}
	}
	var smallSec, largeSec float64
	for _, j := range jobs {
		k := kindOf(j.Kind)
		if classOf(k.Class).OS != "linux" {
			continue
		}
		if small(k) {
			smallSec += j.Dur * smallMem
		} else {
			_, m := peak(k)
			largeSec += j.Dur * math.Ceil(m)
		}
	}
	share := largeSec / (largeSec + smallSec)
	for _, h := range Hosts {
		if h.OS != "linux" {
			for _, s := range Uniform().Slots {
				if s.Host == h.Name {
					f.Slots = append(f.Slots, s)
				}
			}
			continue
		}
		rc, rm := h.Room()
		room := float64(rm) / gb
		lc, lm := largest(h, func(k Kind) bool { return !small(k) })
		nl := max(1, int(math.Round(room*share/lm)))
		ns := int((room - float64(nl)*lm) / smallMem)
		if h.BudgetCPUs > 0 {
			nl = min(nl, int(rc/lc))
			ns = min(ns, int((rc-float64(nl)*lc)/1))
		}
		for range nl {
			f.Slots = append(f.Slots, Slot{Host: h.Name, Classes: classesOn(h), CPU: lc, Mem: lm})
		}
		for range ns {
			f.Slots = append(f.Slots, Slot{Host: h.Name, Small: true, CPU: 1, Mem: smallMem})
		}
	}
	return f
}

// PerClass is runners per label: each class has its own runners, sized to
// its largest job, and each host's memory is split between the classes it
// takes in proportion to what this stream asks of each (known in advance,
// which nobody can). Leftover room goes to the class that asks most per runner.
func PerClass(jobs []Job) Fixed {
	f := Fixed{Name: "fixed-per-class", Setup: "runners per class, each sized to its class's largest job; each host's memory split between its classes by what this stream asks of each, known in advance"}
	size := map[string][2]float64{}
	ask := map[string]float64{}
	for _, c := range Classes {
		var cpu, mem float64
		for _, k := range Kinds {
			if k.Class == c.Name {
				kc, km := peak(k)
				cpu, mem = math.Max(cpu, kc), math.Max(mem, math.Ceil(km))
			}
		}
		// Jobs larger than their kind ever was (they grew, or are new) are
		// sized by what they used: the stream is known in advance.
		for _, j := range jobs {
			if kindOf(j.Kind).Class == c.Name {
				cpu, mem = math.Max(cpu, j.CPU), math.Max(mem, math.Ceil(j.Mem))
			}
		}
		size[c.Name] = [2]float64{cpu, mem}
	}
	for _, j := range jobs {
		c := kindOf(j.Kind).Class
		ask[c] += j.Dur * size[c][1]
	}
	// Where each class may go, by room: a class shares its ask among its hosts.
	hostsOf := map[string]float64{}
	for _, h := range Hosts {
		_, m := h.Room()
		for _, c := range classesOn(h) {
			hostsOf[c] += float64(m)
		}
	}
	first := map[string]string{} // class -> the host with the most room it may use
	firstRoom := map[string]int64{}
	for _, h := range Hosts {
		_, m := h.Room()
		for _, c := range classesOn(h) {
			if m > firstRoom[c] {
				first[c], firstRoom[c] = h.Name, m
			}
		}
	}
	for _, h := range Hosts {
		rc, rm := h.Room()
		room := float64(rm) / gb
		cls := classesOn(h)
		want := map[string]float64{}
		var total float64
		for _, c := range cls {
			want[c] = ask[c] * float64(rm) / hostsOf[c]
			total += want[c]
		}
		n := map[string]int{}
		left, cpuLeft := room, rc
		for _, c := range cls {
			n[c] = int(room * want[c] / total / size[c][1])
			// Every class has a runner on the largest host it may use.
			if n[c] == 0 && first[c] == h.Name {
				n[c] = 1
			}
			left -= float64(n[c]) * size[c][1]
			cpuLeft -= float64(n[c]) * size[c][0]
		}
		for {
			best, bestAsk := "", 0.0
			for _, c := range cls {
				fits := size[c][1] <= left && (h.BudgetCPUs == 0 || size[c][0] <= cpuLeft)
				if a := want[c] / float64(n[c]+1); fits && a > bestAsk {
					best, bestAsk = c, a
				}
			}
			if best == "" {
				break
			}
			n[best]++
			left -= size[best][1]
			cpuLeft -= size[best][0]
		}
		for _, c := range cls {
			for range n[c] {
				f.Slots = append(f.Slots, Slot{Host: h.Name, Classes: []string{c}, CPU: size[c][0], Mem: size[c][1]})
			}
		}
	}
	return f
}

// RunFixed runs the stream on fixed runners. GitHub hands each waiting job,
// oldest first, to an idle runner whose labels it asks for; the runner is up
// already and takes it at once.
func RunFixed(f Fixed, jobs []Job) Result {
	res := Result{Name: f.Name, Setup: f.Setup, Waits: make([]float64, len(jobs))}
	for _, j := range jobs {
		if !slices.ContainsFunc(f.Slots, func(s Slot) bool { return f.takes(s, kindOf(j.Kind)) }) {
			panic(fmt.Sprintf("race: no runner of %s takes %s", f.Name, j.Kind))
		}
	}
	m := newMachines()
	busy := make([]*run, len(f.Slots))
	ends := make([]float64, len(jobs))
	var queue []int
	done := 0
	arr := newArrivals(jobs)
	for t := 0.0; done < len(jobs); t += Tick {
		if t > 24*3600 {
			panic("race: " + f.Name + " never ran every job")
		}
		var runs []*run
		for _, r := range busy {
			if r != nil {
				runs = append(runs, r)
			}
		}
		pace := m.pace(t, runs, jobs)
		for i, r := range busy {
			if r != nil && r.end > 0 {
				ends[r.job] = r.end
				busy[i] = nil
				done++
			}
		}
		queue = append(queue, arr.due(t, ends)...)
		left := queue[:0:0]
		for _, ji := range queue {
			k := kindOf(jobs[ji].Kind)
			si := -1
			for i, s := range f.Slots {
				if busy[i] == nil && f.takes(s, k) {
					si = i
					break
				}
			}
			if si < 0 {
				left = append(left, ji)
				continue
			}
			busy[si] = start(ji, f.Slots[si].Host, t, jobs)
			res.Waits[ji] = math.Max(0, t-arr.at[ji])
			if jobs[ji].Mem > f.Slots[si].Mem+1e-9 {
				res.Summary.OverRunner++
			}
		}
		queue = left
		fr := newFrame(t, done, ids(jobs, queue))
		m.frame(&fr, pace)
		perHost := map[string]int{}
		for si, s := range f.Slots {
			perHost[s.Host]++
			r := busy[si]
			if r == nil {
				continue
			}
			j := jobs[r.job]
			fr.host(s.Host).Jobs = append(fr.host(s.Host).Jobs, Running{ID: j.ID, Kind: j.Kind, Slot: perHost[s.Host],
				ResCPU: s.CPU, ResMem: s.Mem, CPU: round2(r.cpu), Mem: round2(r.mem)})
		}
		res.Frames = append(res.Frames, fr)
	}
	res.Arrived = arr.at
	summarize(&res, jobs, ends)
	return res
}

func newFrame(t float64, done int, waiting []int) Frame {
	f := Frame{T: t, Done: done, Waiting: waiting}
	for _, h := range Hosts {
		f.Hosts = append(f.Hosts, HostFrame{Name: h.Name, Jobs: []Running{}})
	}
	return f
}

func (f *Frame) host(name string) *HostFrame {
	i := slices.IndexFunc(f.Hosts, func(h HostFrame) bool { return h.Name == name })
	return &f.Hosts[i]
}

func ids(jobs []Job, idx []int) []int {
	out := make([]int, len(idx))
	for i, ji := range idx {
		out[i] = jobs[ji].ID
	}
	return out
}

// ---------------------------------------------------------------- pumasi

type runner struct {
	name, class, host string
	created, ready    float64
	run               *run // nil = no job yet
	est               size.Estimate
	warm              bool // started for no job
}

// PSICPUThreshold and PSIMemThreshold are the controller's: cpu and memory
// pressure at which a host gets no new runners.
const (
	PSICPUThreshold = 40
	PSIMemThreshold = 10
)

var epoch = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return epoch.Add(time.Duration(s * float64(time.Second))) }

// history is what pumasi measured of each job before the stream: its last
// size.Window runs, drawn as the stream's are.
func history(sc Scenario) map[string][]size.Sample {
	out := map[string][]size.Sample{}
	if sc.Cold {
		return out
	}
	r := rand.New(rand.NewPCG(sc.Seed, 2))
	for _, k := range Kinds {
		for range size.Window {
			out[k.Name] = append(out[k.Name], sc.Before().draw(r, 0, k.Name).alone())
		}
	}
	return out
}

// estimate is what pumasi places a job of kind with, as the controller works it out.
func estimate(measured map[string][]size.Sample, kind string) size.Estimate {
	k := kindOf(kind)
	cl := classOf(k.Class)
	first := size.Estimate{CPUs: cl.CPUs, MemBytes: cl.Mem}
	if runs := measured[kind]; len(runs) > 0 {
		return size.Learn(runs, first)
	}
	var wf []size.Sample
	for _, o := range Kinds {
		if o.Name != kind && o.Workflow == k.Workflow {
			wf = append(wf, measured[o.Name]...)
		}
	}
	if len(wf) > 0 {
		return size.Borrow(first, size.Learn(wf, size.Estimate{}))
	}
	first.Blind = true
	return first
}

// RunPumasi runs the stream on pumasi: each round, decide.Decide starts
// runners for the waiting jobs where their measured size fits; a runner comes
// up in sc.Provision seconds and GitHub hands it its class's oldest job.
// A finished job's run is measured for the next.
func RunPumasi(sc Scenario, jobs []Job) Result {
	res := Result{Name: "pumasi", Setup: fmt.Sprintf("decide.Decide each %gs with sizes from size.Learn, cpuOvercommit %g; a runner takes %gs to come up", Tick, max(1, sc.CPUOvercommit), sc.Provision), Waits: make([]float64, len(jobs))}
	if sc.Warm > 0 {
		res.Setup += fmt.Sprintf("; up to %d runners a class kept warm", sc.Warm)
	}
	switch {
	case sc.Cold:
		res.Name, res.Setup = "pumasi-cold", res.Setup+"; nothing measured before"
	case sc.CPUOvercommit <= 1:
		res.Name = "pumasi-strict"
	case sc.Warm <= 0:
		res.Name = "pumasi-nowarm"
	}
	measured := history(sc)
	told := map[int]bool{}
	m := newMachines()
	pressed, memPressed := map[string]bool{}, map[string]bool{}
	delayed := map[int]float64{}
	ends := make([]float64, len(jobs))
	var demand []int
	var runners []runner
	done, seq := 0, 0
	ticks := 0.0
	arr := newArrivals(jobs)
	// take hands a listening runner its class's oldest job, as GitHub does.
	take := func(r *runner, t float64) bool {
		i := slices.IndexFunc(demand, func(ji int) bool { return kindOf(jobs[ji].Kind).Class == r.class })
		if i < 0 {
			return false
		}
		ji := demand[i]
		demand = slices.Delete(demand, i, i+1)
		r.run, r.est = start(ji, r.host, t, jobs), estimate(measured, jobs[ji].Kind)
		res.Waits[ji] = math.Max(0, t-arr.at[ji])
		if jobs[ji].Mem*gb > float64(r.est.MemBytes)+1 {
			res.Summary.OverRunner++
		}
		if r.warm {
			res.Idle.WarmServed++
		}
		return true
	}
	var classes []decide.Class
	for _, c := range Classes {
		classes = append(classes, decide.Class{Name: c.Name, OS: c.OS, CPUs: c.CPUs, MemBytes: c.Mem, Hosts: c.Hosts})
	}
	charge := func(r runner, t float64) (float64, int64) {
		if r.run == nil {
			hold := int64(1)
			if r.est.Blind {
				hold = size.BlindHold
			}
			return r.est.CPUs, r.est.MemBytes * hold
		}
		return size.Charge(r.est, r.run.cpu, int64(r.run.mem*gb), r.est.Watched(at(r.run.started), at(t)))
	}
	for t := 0.0; done < len(jobs); t += Tick {
		if t > 24*3600 {
			panic("race: pumasi never ran every job")
		}
		var runs []*run
		for _, r := range runners {
			if r.run != nil {
				runs = append(runs, r.run)
			}
		}
		pace := m.pace(t, runs, jobs)
		demand = append(demand, arr.due(t, ends)...)
		keep := runners[:0:0]
		for _, r := range runners {
			if r.run == nil {
				if t >= r.ready {
					take(&r, t) // or it listens until a job comes or pumasi stops it
				}
				keep = append(keep, r)
				continue
			}
			if r.run.end == 0 {
				keep = append(keep, r)
				continue
			}
			j := jobs[r.run.job]
			ends[r.run.job] = r.run.end
			done++
			runs := append(measured[j.Kind], r.run.measure(jobs))
			measured[j.Kind] = slices.Clone(runs[max(0, len(runs)-size.Window):])
		}
		runners = keep
		// Jobs that came as the ones they need ended go to a runner listening.
		if later := arr.due(t, ends); len(later) > 0 {
			demand = append(demand, later...)
			for i := range runners {
				if r := &runners[i]; r.run == nil && t >= r.ready {
					take(r, t)
				}
			}
		}

		s := decide.Snapshot{Now: at(t), Classes: warmed(classes, sc, jobs, arr, measured, t), MaxStarting: 4, BlindHold: size.BlindHold, PSICPUThreshold: PSICPUThreshold, PSIMemThreshold: PSIMemThreshold, CPUOvercommit: sc.CPUOvercommit,
			ProvisionTimeout: 3 * time.Minute, IdleTimeout: 5 * time.Minute, HostLostAfter: 2 * time.Minute}
		for _, h := range Hosts {
			dh := h.decide()
			if h.OS == "linux" {
				dh.PSICPU, dh.CPUPressureKnown = math.Round(m.psi[h.Name]*100)/100, true
				pressed[h.Name] = decide.Over(dh.PSICPU, PSICPUThreshold, pressed[h.Name])
				if pressed[h.Name] {
					dh.Pressed = []string{"cpu"}
				}
				dh.PSIMem = m.psiMem(h)
				memPressed[h.Name] = decide.Over(dh.PSIMem, PSIMemThreshold, memPressed[h.Name])
				if memPressed[h.Name] {
					dh.Pressed = append(dh.Pressed, "memory")
				}
			}
			s.Hosts = append(s.Hosts, dh)
		}
		for _, ji := range demand {
			e := estimate(measured, jobs[ji].Kind)
			d := decide.Demand{ID: fmt.Sprint(ji), Class: kindOf(jobs[ji].Kind).Class, FirstSeen: at(arr.at[ji]), Name: jobs[ji].Kind, Blind: e.Blind}
			if !e.Blind {
				d.CPUs, d.MemBytes = e.CPUs, e.MemBytes
			}
			s.Demand = append(s.Demand, d)
		}
		for _, r := range runners {
			dr := decide.Runner{Name: r.name, Class: r.class, Host: r.host, State: "starting", CreatedAt: at(r.created), Container: "running"}
			dr.CPUs, dr.MemBytes = charge(r, t)
			switch {
			case r.run != nil:
				dr.State, dr.JobKnown, dr.Working, dr.JobStartedAt, dr.ListeningAt = "busy", true, true, at(r.run.started), at(r.ready)
				dr.Watched = r.est.Watched(at(r.run.started), at(t))
			case t >= r.ready:
				dr.State, dr.ListeningAt = "idle", at(r.ready)
			}
			s.Runners = append(s.Runners, dr)
		}
		res.Told = append(res.Told, tell(s, runners, measured, jobs, sc, t, told)...)
		plan := decide.Decide(s)
		for _, ji := range warmCost(s, plan, runners) {
			delayed[ji] += Tick
		}
		for _, st := range plan.Stop {
			i := slices.IndexFunc(runners, func(r runner) bool { return r.name == st.Runner })
			if i < 0 || runners[i].run != nil {
				continue // a job's runner goes when the job ends
			}
			runners = slices.Delete(runners, i, i+1)
			res.Idle.Stopped++
		}
		for _, st := range plan.Start {
			seq++
			est := size.Estimate{CPUs: st.CPUs, MemBytes: st.MemBytes}
			var ji int
			if _, err := fmt.Sscan(st.Job, &ji); err == nil {
				est = estimate(measured, jobs[ji].Kind)
			}
			runners = append(runners, runner{name: fmt.Sprintf("r%d", seq), class: st.Class, host: st.Host, created: t, ready: t + sc.provision(seq), est: est, warm: st.Job == ""})
			if st.Job == "" {
				res.Idle.WarmStarted++
			}
		}

		fr := newFrame(t, done, ids(jobs, demand))
		m.frame(&fr, pace)
		var idleCPU, idleMem float64
		for _, r := range runners {
			if r.run == nil {
				c, mem := charge(r, t)
				idleCPU, idleMem = idleCPU+c, idleMem+float64(mem)/gb
			}
		}
		res.Idle.HeldCPU, res.Idle.HeldMem = res.Idle.HeldCPU+idleCPU, res.Idle.HeldMem+idleMem
		res.Idle.PeakMem = math.Max(res.Idle.PeakMem, round2(idleMem))
		ticks++
		for _, r := range runners {
			c, mem := charge(r, t)
			x := Running{ResCPU: round2(c), ResMem: round2(float64(mem) / gb), Starting: r.run == nil}
			if r.run != nil {
				j := jobs[r.run.job]
				x.ID, x.Kind, x.CPU, x.Mem = j.ID, j.Kind, round2(r.run.cpu), round2(r.run.mem)
			}
			fr.host(r.host).Jobs = append(fr.host(r.host).Jobs, x)
		}
		res.Frames = append(res.Frames, fr)
	}
	res.Idle.HeldCPU, res.Idle.HeldMem = round2(res.Idle.HeldCPU/ticks), round2(res.Idle.HeldMem/ticks)
	res.Arrived = arr.at
	for _, d := range delayed {
		res.Idle.Delayed++
		res.Idle.DelayedSecs += d
		res.Idle.DelayedMax = math.Max(res.Idle.DelayedMax, d)
	}
	summarize(&res, jobs, ends)
	return res
}

// warmCost is the waiting jobs plan does not start that the same snapshot
// without the runners started warm and still without a job would have.
func warmCost(s decide.Snapshot, plan decide.Plan, runners []runner) []int {
	waits := map[string]bool{}
	for _, l := range plan.Line {
		if l.Reason != "" {
			waits[l.ID] = true
		}
	}
	idle := map[string]bool{}
	for _, r := range runners {
		if r.warm && r.run == nil {
			idle[r.name] = true
		}
	}
	if len(waits) == 0 || len(idle) == 0 {
		return nil
	}
	s.Runners = slices.DeleteFunc(slices.Clone(s.Runners), func(r decide.Runner) bool { return idle[r.Name] })
	var out []int
	for _, st := range decide.Decide(s).Start {
		var ji int
		if _, err := fmt.Sscan(st.Job, &ji); err == nil && waits[st.Job] {
			out = append(out, ji)
		}
	}
	return out
}

// warmed is the classes with how many runners each keeps warm at t
// (decide.WarmCount of the arrivals of the last decide.WarmWindow) and the size of
// its next job: the largest estimate of the kinds that arrived.
func warmed(classes []decide.Class, sc Scenario, jobs []Job, arr *arrivals, measured map[string][]size.Sample, t float64) []decide.Class {
	if sc.Warm <= 0 {
		return classes
	}
	out := slices.Clone(classes)
	came := arr.came()
	for i := range out {
		c := &out[i]
		var ts []time.Time
		for _, ji := range came {
			j := jobs[ji]
			if kindOf(j.Kind).Class != c.Name || arr.at[ji] < t-decide.WarmWindow.Seconds() {
				continue
			}
			ts = append(ts, at(arr.at[ji]))
			e := estimate(measured, j.Kind)
			c.NextCPUs, c.NextMem = math.Max(c.NextCPUs, e.CPUs), max(c.NextMem, e.MemBytes)
		}
		c.Warm = decide.WarmCount(ts, sc.Warm)
	}
	return out
}

// tell is what pumasi would say now of when each waiting job starts: the
// forecast from the same snapshot the placement sees, with run times learned
// as the sizes are (size.Runtime of the job's measured runs).
func tell(s decide.Snapshot, runners []runner, measured map[string][]size.Sample, jobs []Job, sc Scenario, t float64, told map[int]bool) []Told {
	runtime := func(kind string) (time.Duration, bool) { return size.Runtime(measured[kind]) }
	busy := map[string]decide.Busy{}
	for _, r := range runners {
		if r.run == nil {
			continue
		}
		kind := jobs[r.run.job].Kind
		b := decide.Busy{Job: kind}
		if d, ok := runtime(kind); ok {
			b.Ends = decide.EndOf(at(r.run.started), at(t), d)
		}
		busy[r.name] = b
	}
	runs := map[string]time.Duration{}
	for _, d := range s.Demand {
		if r, ok := runtime(d.Name); ok {
			runs[d.ID] = r
		}
	}
	var out []Told
	for id, e := range decide.Forecast(s, busy, runs, time.Duration(sc.Provision*float64(time.Second))) {
		var ji int
		fmt.Sscan(id, &ji)
		out = append(out, Told{Job: ji, At: t, Starts: t + e.In.Seconds(), First: !told[ji]})
		told[ji] = true
	}
	slices.SortFunc(out, func(a, b Told) int { return cmp.Compare(a.Job, b.Job) })
	return out
}
