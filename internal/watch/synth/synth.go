// Package synth makes up weeks of CI history: a repository whose pull
// requests run its checks and are merged to main, where every merge runs them
// again, on two hosts of different speed, with the noise real jobs have. Into
// it go regressions whose commit and pull request are known, so a detector
// can be scored, and the demo can show a CI that grows.
//
// The noise model (per job): a lognormal spread of a few percent; a busy-hour
// slowdown; slow outliers (a cold cache, a slow mirror) and rare fast ones; a
// run now and then on a host saturated by others, mostly flagged contended,
// sometimes not (a Mac reports no pressure); a few hours a week when one host
// is slow for everyone and nothing says so; and the slow natural growth of a
// suite where a quarter of merged pull requests add a little.
package synth

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

const gb = 1 << 30

// Job is a job of the repository and how it runs on the fast host.
type Job struct {
	Workflow, Name, Class string
	Sec                   float64 // median seconds
	Cores                 float64 // cpu-seconds per second
	MemGB                 float64
	Spread                float64 // lognormal sigma of its time
	Steps                 []Step
	Nightly               bool // runs once a night on main, not on pull requests
}

// Step is a step of a job and its share of the job's time.
type Step struct {
	Name  string
	Share float64
}

// Shop is the repository's jobs: a pull request checks workflow that also
// runs on every push to main, and a nightly one.
var Shop = []Job{
	{Workflow: ".github/workflows/ci.yml", Name: "lint", Class: "pr-checks", Sec: 55, Cores: 0.7, MemGB: 1.1, Spread: 0.06,
		Steps: steps("Set up job", .05, "Checkout", .1, "Install", .35, "Lint", .45, "Post", .05)},
	{Workflow: ".github/workflows/ci.yml", Name: "typecheck", Class: "pr-checks", Sec: 85, Cores: 1.0, MemGB: 2.2, Spread: 0.05,
		Steps: steps("Set up job", .03, "Checkout", .07, "Install", .3, "Typecheck", .57, "Post", .03)},
	{Workflow: ".github/workflows/ci.yml", Name: "unit-tests", Class: "pr-checks", Sec: 240, Cores: 1.9, MemGB: 3.2, Spread: 0.08,
		Steps: steps("Set up job", .02, "Checkout", .04, "Install", .14, "Build", .2, "Run tests", .55, "Upload coverage", .05)},
	{Workflow: ".github/workflows/ci.yml", Name: "build", Class: "build", Sec: 310, Cores: 5, MemGB: 11, Spread: 0.07,
		Steps: steps("Set up job", .02, "Checkout", .05, "Restore cache", .08, "Compile", .7, "Package", .12, "Post", .03)},
	{Workflow: ".github/workflows/ci.yml", Name: "e2e (chromium)", Class: "e2e", Sec: 620, Cores: 3.2, MemGB: 9.5, Spread: 0.1,
		Steps: steps("Set up job", .01, "Checkout", .02, "Install", .07, "Start services", .1, "Run e2e", .76, "Upload traces", .04)},
	{Workflow: ".github/workflows/ci.yml", Name: "e2e (webkit)", Class: "e2e", Sec: 560, Cores: 2.8, MemGB: 8.5, Spread: 0.11,
		Steps: steps("Set up job", .01, "Checkout", .02, "Install", .08, "Start services", .1, "Run e2e", .75, "Upload traces", .04)},
	{Workflow: ".github/workflows/ci.yml", Name: "integration", Class: "e2e", Sec: 420, Cores: 2.2, MemGB: 6, Spread: 0.09,
		Steps: steps("Set up job", .02, "Checkout", .03, "Start database", .12, "Migrate", .08, "Run integration", .72, "Post", .03)},
	{Workflow: ".github/workflows/nightly.yml", Name: "soak", Class: "nightly", Sec: 1800, Cores: 3.6, MemGB: 13.5, Spread: 0.05, Nightly: true,
		Steps: steps("Set up job", .01, "Checkout", .01, "Soak", .95, "Report", .03)},
}

func steps(kv ...any) []Step {
	var out []Step
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Step{Name: kv[i].(string), Share: kv[i+1].(float64)})
	}
	return out
}

// Change is what a merged pull request does to one job: a factor on its time
// (or on one step's), its memory or its cpu.
type Change struct {
	Job    string
	Metric string // duration | mem | cpu
	Step   string // with duration: only this step grows ("" = the whole job)
	Factor float64
	// Creep: the factor comes over this many merges, a little with each,
	// instead of at once.
	Creep int
}

// Injected is a change that was put in a history, and where it went.
type Injected struct {
	Change
	PR    int
	SHA   string // the merge commit on main
	At    time.Time
	Label string
}

// Config is a history to make.
type Config struct {
	Seed   uint64
	Start  time.Time
	Days   int
	Jobs   []Job   // default Shop
	Merges float64 // merges to main on a weekday (default 8)
	// Inject: changes at a moment, each carried by the pull request merged
	// first after it.
	Inject []Planned
	// Natural: the slow growth of a suite (default on).
	NoNatural bool
	// ContendedRate: share of runs on a host saturated by others (default 6%).
	ContendedRate float64
	// Growth: the merge rate grows by this share each week (a team that grows).
	Growth float64
	// Cores: when set, the hosts hold this many cores together and a job
	// waits (first come, first served) until its cores are free; otherwise
	// every job finds room.
	Cores float64
}

// Planned is a change to inject after a moment.
type Planned struct {
	Change
	After time.Duration // from Start
	Label string
}

// History is what came of a config.
type History struct {
	Runs     []ledger.JobRun
	Injected []Injected
	Merges   int
}

type merge struct {
	at      time.Time
	pr      int
	sha     string
	changes []Change
	opened  time.Time
}

type host struct {
	name  string
	speed float64 // time factor
	share float64
}

var hosts = []host{{"linux-fast", 1.0, 0.65}, {"linux-slow", 1.3, 0.35}}

// Generate makes the history of a config.
func Generate(cfg Config) History {
	if cfg.Jobs == nil {
		cfg.Jobs = Shop
	}
	if cfg.Merges == 0 {
		cfg.Merges = 8
	}
	if cfg.ContendedRate == 0 {
		cfg.ContendedRate = 0.06
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	end := cfg.Start.AddDate(0, 0, cfg.Days)

	// The merges to main: working hours on weekdays, a few on weekends.
	var merges []merge
	pr := 1000
	for d := 0; d < cfg.Days; d++ {
		day := cfg.Start.AddDate(0, 0, d)
		rate := cfg.Merges * math.Pow(1+cfg.Growth, float64(d)/7)
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			rate /= 8
		}
		n := poisson(rng, rate)
		for range n {
			at := day.Add(time.Duration((9 + 10*rng.Float64()) * float64(time.Hour)))
			pr++
			merges = append(merges, merge{at: at, pr: pr, opened: at.Add(-time.Duration((2 + 60*rng.Float64()) * float64(time.Hour)))})
		}
	}
	slices.SortFunc(merges, func(a, b merge) int { return a.at.Compare(b.at) })
	for i := range merges {
		merges[i].sha = sha(cfg.Seed, "merge", i)
	}
	// What each merge changes: natural growth, and the injected changes.
	jobNames := []string{}
	for _, j := range cfg.Jobs {
		if !j.Nightly {
			jobNames = append(jobNames, j.Name)
		}
	}
	if !cfg.NoNatural {
		for i := range merges {
			switch r := rng.Float64(); {
			case r < 0.25:
				merges[i].changes = append(merges[i].changes, Change{Job: jobNames[rng.IntN(len(jobNames))], Metric: "duration", Factor: 1 + 0.002 + 0.008*rng.Float64()})
			case r < 0.30:
				merges[i].changes = append(merges[i].changes, Change{Job: jobNames[rng.IntN(len(jobNames))], Metric: "duration", Factor: 1 - 0.01 - 0.02*rng.Float64()})
			case r < 0.33:
				merges[i].changes = append(merges[i].changes, Change{Job: jobNames[rng.IntN(len(jobNames))], Metric: "mem", Factor: 1 + 0.01*rng.Float64()})
			}
		}
	}
	var injected []Injected
	for _, p := range cfg.Inject {
		at := cfg.Start.Add(p.After)
		i := slices.IndexFunc(merges, func(m merge) bool { return !m.at.Before(at) })
		if i < 0 {
			continue
		}
		if p.Creep > 1 {
			// A little with each of the next Creep merges.
			f := math.Pow(p.Factor, 1/float64(p.Creep))
			for k := 0; k < p.Creep && i+k < len(merges); k++ {
				c := p.Change
				c.Factor, c.Creep = f, 0
				merges[i+k].changes = append(merges[i+k].changes, c)
			}
		} else {
			merges[i].changes = append(merges[i].changes, p.Change)
		}
		injected = append(injected, Injected{Change: p.Change, PR: merges[i].pr, SHA: merges[i].sha, At: merges[i].at, Label: p.Label})
	}

	// Bad spells: a few hours a week when one host is slow for everyone (a
	// failing disk, a slow registry mirror) and nothing says so.
	var spells []spell
	for wk := 0; wk*7 < cfg.Days; wk++ {
		for range 1 + rng.IntN(2) {
			at := cfg.Start.AddDate(0, 0, wk*7).Add(time.Duration(rng.Float64() * 7 * 24 * float64(time.Hour)))
			spells = append(spells, spell{host: hosts[rng.IntN(len(hosts))].name, from: at,
				to: at.Add(time.Duration((2 + 6*rng.Float64()) * float64(time.Hour))), factor: 1.3 + 0.5*rng.Float64()})
		}
	}
	g := &gen{cfg: cfg, rng: rng, merges: merges, spells: spells}
	// merged: how many merges main had at a moment.
	merged := func(t time.Time) int {
		n, _ := slices.BinarySearchFunc(merges, t, func(m merge, t time.Time) int { return m.at.Compare(t) })
		return n
	}
	// Runs: each pull request's pushes (GitHub tests the pull request merged
	// into main as main is at that moment), each merge's push to main, each night.
	for i, m := range merges {
		pushes := 1 + poisson(rng, 1.5)
		for p := range pushes {
			at := m.opened.Add(time.Duration(float64(m.at.Sub(m.opened)) * (float64(p) + rng.Float64()*0.8) / float64(pushes)))
			g.run(at, "pull_request", fmt.Sprintf("feature-%d", m.pr), sha(cfg.Seed, fmt.Sprintf("pr%d", m.pr), p), m.pr, min(merged(at), i), i, false)
		}
		g.run(m.at.Add(5*time.Second), "push", "main", m.sha, 0, i+1, -1, false)
	}
	for d := 0; d < cfg.Days; d++ {
		at := cfg.Start.AddDate(0, 0, d).Add(3 * time.Hour)
		n := merged(at)
		s := "initial"
		if n > 0 {
			s = merges[n-1].sha
		}
		g.run(at, "schedule", "main", s, 0, n, -1, true)
	}
	if cfg.Cores > 0 {
		queue(g.runs, cfg)
	}
	var runs []ledger.JobRun
	for _, r := range g.runs {
		if r.FinishedAt.Before(end) {
			runs = append(runs, r)
		}
	}
	slices.SortFunc(runs, func(a, b ledger.JobRun) int { return a.FinishedAt.Compare(b.FinishedAt) })
	return History{Runs: runs, Injected: injected, Merges: len(merges)}
}

type spell struct {
	host     string
	from, to time.Time
	factor   float64
}

type gen struct {
	spells []spell
	cfg    Config
	rng    *rand.Rand
	merges []merge
	runs   []ledger.JobRun
	runID  int64
}

// run is one workflow run: its jobs on a code state that has the first
// `upto` merges and, for a pull request, its own changes (those of merge
// index own, not yet merged; -1 = none).
func (g *gen) run(at time.Time, event, branch, sha string, pr, upto, own int, nightly bool) {
	g.runID++
	rng := g.rng
	for ji, j := range g.cfg.Jobs {
		if j.Nightly != nightly {
			continue
		}
		dur, mem, cpu := 1.0, 1.0, 1.0
		stepF := map[string]float64{}
		apply := func(c Change) {
			if c.Job != j.Name {
				return
			}
			switch c.Metric {
			case "duration":
				if c.Step != "" {
					stepF[c.Step] = cmpOr(stepF[c.Step], 1) * c.Factor
				} else {
					dur *= c.Factor
				}
			case "mem":
				mem *= c.Factor
			case "cpu":
				cpu *= c.Factor
			}
		}
		for k := 0; k < upto && k < len(g.merges); k++ {
			for _, c := range g.merges[k].changes {
				apply(c)
			}
		}
		if own >= 0 {
			for _, c := range g.merges[own].changes {
				apply(c)
			}
		}
		h := hosts[0]
		if rng.Float64() >= h.share {
			h = hosts[1]
		}
		// Noise: the job's spread, the busy hours, outliers, contention.
		f := math.Exp(rng.NormFloat64() * j.Spread)
		if hr := at.Hour(); hr >= 10 && hr < 18 {
			f *= 1.03
		}
		switch r := rng.Float64(); {
		case r < 0.03:
			f *= 1.3 + 0.7*rng.Float64() // a cold cache, a slow mirror
		case r < 0.04:
			f *= 0.7 + 0.15*rng.Float64() // a warm cache skipped a step
		}
		for _, sp := range g.spells {
			if sp.host == h.name && !at.Before(sp.from) && at.Before(sp.to) {
				f *= sp.factor
			}
		}
		contended, flagged := rng.Float64() < g.cfg.ContendedRate, false
		if contended {
			f *= 1.15 + 0.65*rng.Float64()
			flagged = rng.Float64() < 0.8
		}
		start := at.Add(time.Duration((5 + 25*rng.Float64()) * float64(time.Second)))
		r := ledger.JobRun{Runner: fmt.Sprintf("r%d-%d", g.runID, ji), Class: j.Class, Host: h.name, Platform: "linux/docker",
			Repo: "example/app", Workflow: j.Workflow, Job: j.Name, RunID: g.runID, GHJobID: g.runID*100 + int64(ji), Attempt: 1,
			Event: event, Branch: branch, SHA: sha, PR: pr, Result: "succeeded", QueuedAt: at, StartedAt: start}
		// The steps: each its share of the job, with a spread of its own.
		t := start
		total := 0.0
		for si, s := range j.Steps {
			sec := j.Sec * s.Share * dur * h.speed * f * cmpOr(stepF[s.Name], 1) * math.Exp(rng.NormFloat64()*0.04)
			sec = math.Max(1, math.Round(sec))
			r.Steps = append(r.Steps, ledger.Step{Number: si + 1, Name: s.Name, StartedAt: t, CompletedAt: t.Add(time.Duration(sec) * time.Second), Sec: sec, Conclusion: "success"})
			t = t.Add(time.Duration(sec) * time.Second)
			total += sec
		}
		r.FinishedAt = t.Add(time.Second)
		r.RunSec = r.FinishedAt.Sub(r.StartedAt).Seconds()
		r.WaitSec = r.StartedAt.Sub(r.QueuedAt).Seconds()
		// cpu-seconds are the work: a slower host spends more on it, a wait
		// on the network or on a busy host's other jobs only a little.
		r.CPUSec = math.Round(total/f*math.Pow(f, 0.3)*j.Cores*cpu*math.Exp(rng.NormFloat64()*0.03)*10) / 10
		r.MemWorkPeak = int64(j.MemGB * mem * math.Exp(rng.NormFloat64()*0.025) * gb)
		r.MemPeak = r.MemWorkPeak + r.MemWorkPeak/3
		if flagged {
			r.ContendedSec = math.Round(r.RunSec * (0.3 + 0.6*rng.Float64()))
			r.ContendedWhy = "host cpu 97% busy, 9.1 of 16 cores by others"
		}
		g.runs = append(g.runs, r)
	}
}

func cmpOr(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}

func poisson(rng *rand.Rand, mean float64) int {
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func sha(seed uint64, kind string, i int) string {
	s := sha1.Sum(fmt.Appendf(nil, "%d/%s/%d", seed, kind, i))
	return hex.EncodeToString(s[:])
}

// DemoCores are the cores the demo's history ran on, for its forecast.
const DemoCores = 24

// Demo is the history `--demo` shows: six weeks of a repository, the last
// of them the week of end, whose team grows a little each week, with a
// typecheck made faster, an e2e job creeping up, a test step that got slower
// and a build that takes more memory. It is the same history every time,
// moved by whole weeks so its weekdays stay; runs after end are dropped.
func Demo(end time.Time) History {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // a Monday
	day := func(d float64) time.Duration { return time.Duration(d*24*float64(time.Hour)) + 10*time.Hour }
	h := Generate(Config{Seed: 42, Start: base, Days: 42, Merges: 7, Growth: 0.06, Inject: []Planned{
		{Change: Change{Job: "e2e (chromium)", Metric: "duration", Factor: 1.25, Creep: 150}, After: day(9), Label: "e2e creeping up"},
		{Change: Change{Job: "unit-tests", Metric: "duration", Step: "Run tests", Factor: 1.7}, After: day(28), Label: "a slower test step"},
		{Change: Change{Job: "typecheck", Metric: "duration", Factor: 0.75}, After: day(29), Label: "typecheck made faster"},
		{Change: Change{Job: "build", Metric: "mem", Factor: 1.35}, After: day(31), Label: "build takes more memory"},
	}})
	e := end.UTC()
	monday := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -((int(e.Weekday()) + 6) % 7))
	shift := monday.Sub(base.AddDate(0, 0, 35))
	mv := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.Add(shift).In(end.Location())
	}
	var runs []ledger.JobRun
	for _, r := range h.Runs {
		r.QueuedAt, r.StartedAt, r.FinishedAt = mv(r.QueuedAt), mv(r.StartedAt), mv(r.FinishedAt)
		if r.FinishedAt.After(end) {
			continue
		}
		steps := make([]ledger.Step, len(r.Steps))
		for i, st := range r.Steps {
			st.StartedAt, st.CompletedAt = mv(st.StartedAt), mv(st.CompletedAt)
			steps[i] = st
		}
		r.Steps = steps
		runs = append(runs, r)
	}
	h.Runs = runs
	for i := range h.Injected {
		h.Injected[i].At = mv(h.Injected[i].At)
	}
	return h
}

// queue makes each job wait until its job's cores are free among cfg.Cores,
// first come first served, and moves its times by the wait.
func queue(runs []ledger.JobRun, cfg Config) {
	cores := map[string]float64{}
	for _, j := range cfg.Jobs {
		cores[j.Name] = j.Cores
	}
	slices.SortStableFunc(runs, func(a, b ledger.JobRun) int { return a.StartedAt.Compare(b.StartedAt) })
	type busy struct {
		until time.Time
		cores float64
	}
	var running []busy
	free := cfg.Cores
	var last time.Time
	for i := range runs {
		r := &runs[i]
		need := math.Min(cores[r.Job], cfg.Cores)
		at := r.StartedAt // ready: queued and its runner up
		if at.Before(last) {
			at = last
		}
		for {
			// Free what ended by then.
			kept := running[:0]
			for _, b := range running {
				if !b.until.After(at) {
					free += b.cores
				} else {
					kept = append(kept, b)
				}
			}
			running = kept
			if free >= need-1e-9 {
				break
			}
			next := running[0].until
			for _, b := range running {
				if b.until.Before(next) {
					next = b.until
				}
			}
			at = next
		}
		d := at.Sub(r.StartedAt)
		r.StartedAt = r.StartedAt.Add(d)
		r.FinishedAt = r.FinishedAt.Add(d)
		for k := range r.Steps {
			r.Steps[k].StartedAt = r.Steps[k].StartedAt.Add(d)
			r.Steps[k].CompletedAt = r.Steps[k].CompletedAt.Add(d)
		}
		r.WaitSec = r.StartedAt.Sub(r.QueuedAt).Seconds()
		free -= need
		running = append(running, busy{r.FinishedAt, need})
		last = at
	}
}
