// Package race runs one stream of jobs on the same four hosts several ways:
// on fixed runners, where each host has a set number of runners and a runner
// takes the next job of its labels whatever its size (three ways to size
// them), and on pumasi, where the real placement (decide.Decide) starts a
// runner for each job by what the job was measured to use (size.Learn).
// Anyone can make the numbers again:
//
//	go run ./internal/race/cmd
//
// It is a simulation: the jobs are the demo's (internal/controller/demo.go),
// a job's run time does not depend on what runs beside it, and nothing fails.
package race

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

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

func kindOf(name string) Kind {
	i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Name == name })
	return Kinds[i]
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
}

// use is what the job uses el seconds after it started: it comes up to its
// pace over the first fifth of its run.
func (j Job) use(el float64) (cpu, mem float64) {
	r := math.Min(1, el/math.Max(10, 0.2*j.Dur))
	return j.CPU * (0.4 + 0.6*r), j.Mem * (0.3 + 0.7*r)
}

// measure is what pumasi records of a whole run: its average cores and its peak memory.
func (j Job) measure() size.Sample {
	var sec float64
	for el := 0.0; el < j.Dur; el += Tick {
		c, _ := j.use(el)
		sec += c * math.Min(Tick, j.Dur-el)
	}
	return size.Sample{CPUs: sec / j.Dur, MemBytes: int64(j.Mem * gb)}
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
}

// Film is the scenario the film shows: a busy quarter hour.
var Film = Scenario{Seed: 7, Pushes: 18, Every: [2]float64{35, 55}, Provision: 8}

func (sc Scenario) draw(r *rand.Rand, at float64, kind string) Job {
	k := kindOf(kind)
	u := func(v [2]float64) float64 { return v[0] + r.Float64()*(v[1]-v[0]) }
	return Job{Kind: kind, At: at, Dur: math.Round(u(k.Dur)), CPU: round2(u(k.CPU)), Mem: round2(u(k.Mem))}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Stream is the scenario's jobs, in the order they arrive: the pushes, an
// iOS build every two minutes or so while they come, and one deploy.
func Stream(sc Scenario) []Job {
	r := rand.New(rand.NewPCG(sc.Seed, 1))
	var out []Job
	add := func(at float64, kind string) { out = append(out, sc.draw(r, at, kind)) }
	at := 5.0
	for range sc.Pushes {
		o := func() float64 { return math.Round((at+r.Float64()*3)*10) / 10 }
		add(o(), "lint")
		add(o(), "unit-tests")
		add(o(), "typecheck")
		add(o(), "e2e (chromium)")
		if r.Float64() < 0.4 {
			add(o(), "e2e (webkit)")
		}
		if r.Float64() < 0.35 {
			add(o(), "build-image")
		}
		add(o(), "label-check")
		if r.Float64() < 0.6 {
			add(o(), "size-report")
		}
		at += sc.Every[0] + r.Float64()*(sc.Every[1]-sc.Every[0])
	}
	for t, i := 20.0, 0; t < at; t, i = t+105+r.Float64()*30, i+1 {
		add(math.Round(t), []string{"ios-build", "xcode-tests"}[i%2])
	}
	add(math.Round(at/2), "deploy-api")
	add(math.Round(at/2)+1, "deploy-web")
	slices.SortStableFunc(out, func(a, b Job) int { return cmp.Compare(a.At, b.At) })
	for i := range out {
		out[i].ID = i + 1
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
	// The most cpu the jobs on any one host used at once, over its room (above
	// 1: the jobs there share cores, which the simulation does not slow down).
	PeakCPU float64 `json:"peakCpu"`
}

// Result is one setup's run.
type Result struct {
	Name    string    `json:"name"`
	Setup   string    `json:"setup"` // in words
	Waits   []float64 `json:"waits"` // by job, in stream order
	Summary Summary   `json:"summary"`
	Frames  []Frame   `json:"frames,omitempty"`
}

func summarize(res *Result, jobs []Job, ends []float64) {
	s := Summary{Jobs: len(jobs)}
	w := slices.Clone(res.Waits)
	slices.Sort(w)
	var sum float64
	for _, v := range w {
		sum += v
	}
	q := func(p float64) float64 { return w[min(len(w)-1, int(math.Ceil(p*float64(len(w))))-1)] }
	s.MedianWait, s.P90Wait, s.MaxWait, s.MeanWait = q(0.5), q(0.9), w[len(w)-1], math.Round(sum/float64(len(w)))
	for i, j := range jobs {
		s.Drained = math.Max(s.Drained, j.At+res.Waits[i])
	}
	s.Done = slices.Max(ends)
	var roomCPU, roomMem float64
	rooms := map[string]float64{}
	for _, h := range Hosts {
		c, m := h.Room()
		roomCPU, roomMem = roomCPU+c, roomMem+float64(m)/gb
		rooms[h.Name] = c
	}
	var n, cpu, mem, held float64
	for _, f := range res.Frames {
		s.PeakWaiting = max(s.PeakWaiting, len(f.Waiting))
		var c, m, hm float64
		any := false
		for _, h := range f.Hosts {
			var hc float64
			for _, r := range h.Jobs {
				c, m, hm, hc = c+r.CPU, m+r.Mem, hm+r.ResMem, hc+r.CPU
				any = any || !r.Starting
			}
			s.PeakCPU = math.Max(s.PeakCPU, hc/rooms[h.Name])
		}
		if any {
			n, cpu, mem, held = n+1, cpu+c, mem+m, held+hm
		}
	}
	if n > 0 {
		s.CPUUse, s.MemUse, s.MemHeld = cpu/n/roomCPU, mem/n/roomMem, held/n/roomMem
	}
	s.CPUUse, s.MemUse, s.MemHeld, s.PeakCPU = round2(s.CPUUse), round2(s.MemUse), round2(s.MemHeld), round2(s.PeakCPU)
	res.Summary = s
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
	if len(f.Small) > 0 && slices.Contains(f.Small, k.Name) != s.Small {
		return false
	}
	return s.Small || slices.Contains(s.Classes, k.Class)
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
	busy := make([]int, len(f.Slots)) // job index + 1
	started := make([]float64, len(jobs))
	ends := make([]float64, len(jobs))
	var queue []int
	next, done := 0, 0
	for t := 0.0; done < len(jobs); t += Tick {
		for next < len(jobs) && jobs[next].At <= t {
			queue = append(queue, next)
			next++
		}
		for i, b := range busy {
			if b > 0 && t-started[b-1] >= jobs[b-1].Dur {
				ends[b-1] = started[b-1] + jobs[b-1].Dur
				busy[i] = 0
				done++
			}
		}
		left := queue[:0:0]
		for _, ji := range queue {
			k := kindOf(jobs[ji].Kind)
			placed := false
			for si, s := range f.Slots {
				if busy[si] == 0 && f.takes(s, k) {
					busy[si], started[ji] = ji+1, t
					res.Waits[ji] = math.Max(0, t-jobs[ji].At)
					placed = true
					break
				}
			}
			if !placed {
				left = append(left, ji)
			}
		}
		queue = left
		fr := newFrame(t, done, ids(jobs, queue))
		perHost := map[string]int{}
		for si, s := range f.Slots {
			perHost[s.Host]++
			b := busy[si]
			if b == 0 {
				continue
			}
			j := jobs[b-1]
			c, m := j.use(t - started[b-1])
			fr.host(s.Host).Jobs = append(fr.host(s.Host).Jobs, Running{ID: j.ID, Kind: j.Kind, Slot: perHost[s.Host],
				ResCPU: s.CPU, ResMem: s.Mem, CPU: round2(c), Mem: round2(m)})
		}
		res.Frames = append(res.Frames, fr)
	}
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
	job               int // index + 1; 0 = none yet
	started           float64
	est               size.Estimate
}

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
			out[k.Name] = append(out[k.Name], sc.draw(r, 0, k.Name).measure())
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
	res := Result{Name: "pumasi", Setup: fmt.Sprintf("decide.Decide each %gs with sizes from size.Learn; a runner takes %gs to come up", Tick, sc.Provision), Waits: make([]float64, len(jobs))}
	if sc.Cold {
		res.Name, res.Setup = "pumasi-cold", res.Setup+"; nothing measured before"
	}
	measured := history(sc)
	ends := make([]float64, len(jobs))
	var demand []int
	var runners []runner
	next, done, seq := 0, 0, 0
	var hosts []decide.Host
	for _, h := range Hosts {
		hosts = append(hosts, h.decide())
	}
	var classes []decide.Class
	for _, c := range Classes {
		classes = append(classes, decide.Class{Name: c.Name, OS: c.OS, CPUs: c.CPUs, MemBytes: c.Mem, Hosts: c.Hosts})
	}
	charge := func(r runner, t float64) (float64, int64) {
		if r.job == 0 {
			hold := int64(1)
			if r.est.Blind {
				hold = size.BlindHold
			}
			return r.est.CPUs, r.est.MemBytes * hold
		}
		c, m := jobs[r.job-1].use(t - r.started)
		return size.Charge(r.est, c, int64(m*gb), r.est.Watched(at(r.started), at(t)))
	}
	for t := 0.0; done < len(jobs); t += Tick {
		if t > 24*3600 {
			panic("race: pumasi never ran every job")
		}
		for next < len(jobs) && jobs[next].At <= t {
			demand = append(demand, next)
			next++
		}
		keep := runners[:0:0]
		for _, r := range runners {
			if r.job == 0 {
				if t < r.ready {
					keep = append(keep, r)
					continue
				}
				i := slices.IndexFunc(demand, func(ji int) bool { return kindOf(jobs[ji].Kind).Class == r.class })
				if i < 0 {
					continue // nothing left for it: it goes
				}
				ji := demand[i]
				demand = slices.Delete(demand, i, i+1)
				r.job, r.started, r.est = ji+1, t, estimate(measured, jobs[ji].Kind)
				res.Waits[ji] = math.Max(0, t-jobs[ji].At)
				keep = append(keep, r)
				continue
			}
			j := jobs[r.job-1]
			if t-r.started < j.Dur {
				keep = append(keep, r)
				continue
			}
			ends[r.job-1] = r.started + j.Dur
			done++
			runs := append(measured[j.Kind], j.measure())
			measured[j.Kind] = slices.Clone(runs[max(0, len(runs)-size.Window):])
		}
		runners = keep

		s := decide.Snapshot{Now: at(t), Hosts: hosts, Classes: classes, MaxStarting: 4, BlindHold: size.BlindHold,
			ProvisionTimeout: 3 * time.Minute, IdleTimeout: 5 * time.Minute, HostLostAfter: 2 * time.Minute}
		for _, ji := range demand {
			e := estimate(measured, jobs[ji].Kind)
			d := decide.Demand{ID: fmt.Sprint(ji), Class: kindOf(jobs[ji].Kind).Class, FirstSeen: at(jobs[ji].At), Name: jobs[ji].Kind, Blind: e.Blind}
			if !e.Blind {
				d.CPUs, d.MemBytes = e.CPUs, e.MemBytes
			}
			s.Demand = append(s.Demand, d)
		}
		for _, r := range runners {
			dr := decide.Runner{Name: r.name, Class: r.class, Host: r.host, State: "starting", CreatedAt: at(r.created), Container: "running"}
			dr.CPUs, dr.MemBytes = charge(r, t)
			if r.job > 0 {
				dr.State, dr.JobKnown, dr.Working, dr.JobStartedAt, dr.ListeningAt = "busy", true, true, at(r.started), at(r.ready)
				dr.Watched = r.est.Watched(at(r.started), at(t))
			}
			s.Runners = append(s.Runners, dr)
		}
		for _, st := range decide.Decide(s).Start {
			seq++
			est := size.Estimate{CPUs: st.CPUs, MemBytes: st.MemBytes}
			var ji int
			if _, err := fmt.Sscan(st.Job, &ji); err == nil {
				est = estimate(measured, jobs[ji].Kind)
			}
			runners = append(runners, runner{name: fmt.Sprintf("r%d", seq), class: st.Class, host: st.Host, created: t, ready: t + sc.Provision, est: est})
		}

		fr := newFrame(t, done, ids(jobs, demand))
		for _, r := range runners {
			c, m := charge(r, t)
			run := Running{ResCPU: round2(c), ResMem: round2(float64(m) / gb), Starting: r.job == 0}
			if r.job > 0 {
				j := jobs[r.job-1]
				uc, um := j.use(t - r.started)
				run.ID, run.Kind, run.CPU, run.Mem = j.ID, j.Kind, round2(uc), round2(um)
			}
			fr.host(r.host).Jobs = append(fr.host(r.host).Jobs, run)
		}
		res.Frames = append(res.Frames, fr)
	}
	summarize(&res, jobs, ends)
	return res
}
