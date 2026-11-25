// Package decide holds the placement rule: a pure function from a snapshot of
// hosts, classes, demand and runners to a plan, with a reason for every action.
package decide

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

type Class struct {
	Name       string
	CPUs       float64
	MemBytes   int64
	MaxRunners int
	Hosts      []string // empty = every host
}

type Host struct {
	Name string
	Kind string // server | desktop
	Up   bool
	// How long the agent has been unreachable (0 while Up).
	DownFor  time.Duration
	CPUs     float64
	MemBytes int64
	// Effective budget (0 = none): what pumasi may lend on this host.
	BudgetCPUs   float64
	BudgetMem    int64
	NonRunnerCPU float64
	NonRunnerMem int64
	PSICPU       float64
}

// Runner joins the ledger row with what the host's agent reports.
type Runner struct {
	Name     string
	Class    string
	Host     string
	CPUs     float64
	MemBytes int64
	// Ledger state: starting | idle | busy | done. Empty = not in the ledger (orphan).
	State string
	// JobKnown: GitHub's JobStarted message has named this runner's job. Until then
	// the runner counts against a waiting job even when it already runs one, because
	// that job's demand closes only with the message.
	JobKnown    bool
	CreatedAt   time.Time
	ListeningAt time.Time // zero until the runner listens for jobs
	// Agent view: "" when the agent (up) does not report it, else starting | created | running | exited.
	Container string
	ExitCode  int
}

// Demand is one job waiting for a runner of its class.
type Demand struct {
	ID        string
	Class     string
	FirstSeen time.Time
}

type Snapshot struct {
	Now              time.Time
	Hosts            []Host
	Classes          []Class
	Demand           []Demand
	Runners          []Runner
	PSICPUThreshold  float64
	ProvisionTimeout time.Duration
	IdleTimeout      time.Duration
	HostLostAfter    time.Duration
}

type Start struct {
	Class, Host, Reason string
}

// Stop removes a runner. State is the ledger state to set ("" = keep).
type Stop struct {
	Runner, Host, State, Reason string
}

type Defer struct {
	Class  string
	Count  int
	Reason string
}

type Plan struct {
	Start []Start
	Stop  []Stop
	Defer []Defer
}

// Decide is deterministic: the same snapshot always gives the same plan.
func Decide(s Snapshot) Plan {
	var p Plan
	classes := map[string]Class{}
	for _, c := range s.Classes {
		classes[c.Name] = c
	}
	hosts := map[string]Host{}
	for _, h := range s.Hosts {
		hosts[h.Name] = h
	}

	runners := slices.Clone(s.Runners)
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })

	// 1. Runners to remove, and what the rest occupy.
	alloc := map[string][2]float64{} // host -> cpus, mem
	live := map[string]int{}         // class -> runners holding capacity
	covering := map[string]int{}     // class -> runners not yet matched to a job by GitHub
	for _, r := range runners {
		if st, ok := stopRule(s, hosts[r.Host], r); ok {
			p.Stop = append(p.Stop, st)
			continue
		}
		if r.Container == "exited" {
			continue
		}
		cpus, mem := r.CPUs, float64(r.MemBytes)
		if c, ok := classes[r.Class]; ok {
			cpus, mem = c.CPUs, float64(c.MemBytes)
		}
		a := alloc[r.Host]
		alloc[r.Host] = [2]float64{a[0] + cpus, a[1] + mem}
		live[r.Class]++
		if !r.JobKnown {
			covering[r.Class]++
		}
	}

	// 2. Demand not covered by a runner that is starting or idle: the newest jobs of each class.
	byClass := map[string][]Demand{}
	for _, d := range s.Demand {
		byClass[d.Class] = append(byClass[d.Class], d)
	}
	var open []Demand
	for c, ds := range byClass {
		sort.Slice(ds, func(i, j int) bool { return less(ds[i], ds[j]) })
		if n := covering[c]; n < len(ds) {
			open = append(open, ds[n:]...)
		}
	}
	sort.Slice(open, func(i, j int) bool { return less(open[i], open[j]) })

	// 3. Oldest job first; a smaller class may fill a gap a bigger one cannot use.
	hostNames := make([]string, 0, len(hosts))
	for n := range hosts {
		hostNames = append(hostNames, n)
	}
	sort.Strings(hostNames)
	deferIdx := map[string]int{}
	deferred := func(class, reason string) {
		if i, ok := deferIdx[class]; ok {
			p.Defer[i].Count++
			return
		}
		deferIdx[class] = len(p.Defer)
		p.Defer = append(p.Defer, Defer{Class: class, Count: 1, Reason: reason})
	}
	for _, d := range open {
		c, ok := classes[d.Class]
		if !ok {
			deferred(d.Class, "unknown class")
			continue
		}
		if c.MaxRunners > 0 && live[c.Name] >= c.MaxRunners {
			deferred(c.Name, fmt.Sprintf("at maxRunners %d", c.MaxRunners))
			continue
		}
		best, bestSlots := "", -1.0
		var why []string
		for _, hn := range hostNames {
			h := hosts[hn]
			freeCPU, freeMem := free(h, alloc[hn])
			switch {
			case len(c.Hosts) > 0 && !slices.Contains(c.Hosts, hn):
				continue
			case !h.Up:
				why = append(why, hn+" down")
				continue
			case s.PSICPUThreshold > 0 && h.PSICPU >= s.PSICPUThreshold:
				why = append(why, fmt.Sprintf("%s cpu pressure %.0f", hn, h.PSICPU))
				continue
			case freeCPU+1e-9 < c.CPUs || freeMem < float64(c.MemBytes):
				why = append(why, fmt.Sprintf("%s %.1fcpu/%s", hn, freeCPU, gib(freeMem)))
				continue
			}
			slots := math.Min(freeCPU/c.CPUs, freeMem/float64(c.MemBytes))
			if best == "" || better(h, slots, hosts[best], bestSlots) {
				best, bestSlots = hn, slots
			}
		}
		if best == "" {
			if len(why) == 0 {
				why = append(why, "no allowed host")
			}
			deferred(c.Name, fmt.Sprintf("no host with %.1fcpu/%s free (%s)", c.CPUs, gib(float64(c.MemBytes)), strings.Join(why, ", ")))
			continue
		}
		h := hosts[best]
		freeCPU, freeMem := free(h, alloc[best])
		p.Start = append(p.Start, Start{Class: c.Name, Host: best, Reason: fmt.Sprintf(
			"free %.1fcpu/%s", freeCPU, gib(freeMem))})
		a := alloc[best]
		alloc[best] = [2]float64{a[0] + c.CPUs, a[1] + float64(c.MemBytes)}
		live[c.Name]++
	}
	return p
}

// stopRule returns the removal of a runner that no longer serves a purpose.
func stopRule(s Snapshot, h Host, r Runner) (Stop, bool) {
	stop := func(state, format string, args ...any) (Stop, bool) {
		return Stop{Runner: r.Name, Host: r.Host, State: state, Reason: fmt.Sprintf(format, args...)}, true
	}
	switch {
	case r.State == "":
		return stop("", "not in the ledger")
	case !h.Up:
		if h.Name == "" || h.DownFor > s.HostLostAfter {
			return stop("lost", "host %s unreachable for %s", r.Host, h.DownFor.Round(time.Second))
		}
		return Stop{}, false // judge it when the agent answers again
	case r.State == "done":
		return stop("", "job completed")
	case r.Container == "":
		return stop(failedUnlessJob(r), "container missing on %s", r.Host)
	case r.Container == "exited" && r.State == "busy":
		return stop("", "runner exited during or after its job (code %d); result comes with JobCompleted", r.ExitCode)
	case r.Container == "exited":
		return stop("failed", "exited before a job (code %d)", r.ExitCode)
	case r.State == "starting" && r.ListeningAt.IsZero() && s.ProvisionTimeout > 0 && s.Now.Sub(r.CreatedAt) > s.ProvisionTimeout:
		return stop("failed", "provision timeout: not listening %s after start", s.ProvisionTimeout)
	case r.State != "busy" && !r.ListeningAt.IsZero() && s.IdleTimeout > 0 && s.Now.Sub(r.ListeningAt) > s.IdleTimeout:
		return stop("failed", "idle timeout: no job %s after listening", s.IdleTimeout)
	}
	return Stop{}, false
}

func failedUnlessJob(r Runner) string {
	if r.State == "busy" {
		return ""
	}
	return "failed"
}

// free is what a class may still take on a host: the smaller of what the budget
// leaves and what the machine leaves after everyone else's use.
func free(h Host, a [2]float64) (cpu, mem float64) {
	cpu = h.CPUs - a[0] - h.NonRunnerCPU
	mem = float64(h.MemBytes) - a[1] - float64(h.NonRunnerMem)
	if h.BudgetCPUs > 0 {
		cpu = math.Min(cpu, h.BudgetCPUs-a[0])
	}
	if h.BudgetMem > 0 {
		mem = math.Min(mem, float64(h.BudgetMem)-a[1])
	}
	return math.Max(cpu, 0), math.Max(mem, 0)
}

// better ranks servers before desktops, then more free slots, then name (callers iterate by name).
func better(h Host, slots float64, cur Host, curSlots float64) bool {
	if (h.Kind == "server") != (cur.Kind == "server") {
		return h.Kind == "server"
	}
	return slots > curSlots+1e-9
}

func less(a, b Demand) bool {
	if !a.FirstSeen.Equal(b.FirstSeen) {
		return a.FirstSeen.Before(b.FirstSeen)
	}
	if a.Class != b.Class {
		return a.Class < b.Class
	}
	return a.ID < b.ID
}

func gib(b float64) string { return fmt.Sprintf("%.1fG", b/(1<<30)) }
