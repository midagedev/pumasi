// Package decide holds the placement rule: a pure function from a snapshot of
// hosts, classes, demand and runners to a plan, with a reason for every action.
package decide

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

type Class struct {
	Name string
	// OS the class's jobs need: linux | macos | windows. Empty = linux.
	OS         string
	CPUs       float64
	MemBytes   int64
	MaxRunners int
	Hosts      []string // empty = every host
	// Reserve: room for this many runners of the class is kept from other classes.
	Reserve int
	// Native: the runner is a process of the host's user, not a container.
	Native bool
	// Paths the host must have (the class's mounts).
	Paths []string
	// MaxDuration stops a runner whose job has run this long (0 = never).
	MaxDuration time.Duration
}

type Host struct {
	Name string
	Kind string // server | desktop
	// OS as the host's agent reports it. Empty = linux, or not known yet while the host is down.
	OS string
	Up bool
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
	// Closed: why the host takes no new runners although it is up
	// ("quarantined", "disk 4% free"). Empty = open.
	Closed string
	// NoNative: why the host cannot run process runners. Empty = it can.
	NoNative string
	// Paths: of the paths classes ask for, those this host has.
	Paths []string
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
	// Working: the agent sees this container running a job right now.
	Working bool
	// JobStartedAt: when the runner took its job (zero = no job yet).
	JobStartedAt time.Time
	// Lost: the agent's reason why the container cannot have finished ("host rebooted").
	Lost string
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
		if st, ok := stopRule(s, hosts[r.Host], classes[r.Class], r); ok {
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
		lacking := "" // a path no host that could otherwise take the class has
		for _, hn := range c.Hosts {
			if _, ok := hosts[hn]; !ok {
				why = append(why, hn+" has not joined")
			}
		}
		for _, hn := range hostNames {
			h := hosts[hn]
			freeCPU, freeMem := free(h, alloc[hn])
			ok, reason := takes(s, c, h)
			switch {
			case !ok && reason == "":
				continue
			case !ok && strings.HasPrefix(reason, lacks):
				lacking = strings.TrimPrefix(reason, lacks)
				continue
			case !ok:
				why = append(why, reason)
				continue
			case freeCPU+1e-9 < c.CPUs || freeMem < float64(c.MemBytes):
				why = append(why, fmt.Sprintf("%s %.1fcpu/%s", hn, freeCPU, gib(freeMem)))
				continue
			}
			if r := reservedFor(s, c, hn, hosts, alloc, live); r != "" {
				why = append(why, hn+" reserved for "+r)
				continue
			}
			slots := math.Min(freeCPU/c.CPUs, freeMem/float64(c.MemBytes))
			if best == "" || better(h, slots, hosts[best], bestSlots) {
				best, bestSlots = hn, slots
			}
		}
		if best == "" {
			if len(why) == 0 && lacking != "" {
				deferred(c.Name, "no host has "+lacking)
				continue
			}
			if lacking != "" {
				why = append(why, "the others lack "+lacking)
			}
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

const lacks = "lacks "

// takes reports whether a host may get runners of a class at all, apart from
// room. When not, reason says why for the people waiting ("" = not worth
// telling: another class's host, another OS).
func takes(s Snapshot, c Class, h Host) (ok bool, reason string) {
	switch {
	case len(c.Hosts) > 0 && !slices.Contains(c.Hosts, h.Name):
		return false, ""
	case (h.Up || h.OS != "") && osOf(h.OS) != osOf(c.OS):
		return false, "" // a job that asks for an OS never runs on another one
	case !h.Up:
		return false, h.Name + " down"
	case h.Closed != "":
		return false, h.Name + " " + h.Closed
	case s.PSICPUThreshold > 0 && h.PSICPU >= s.PSICPUThreshold:
		return false, fmt.Sprintf("%s cpu pressure %.0f", h.Name, h.PSICPU)
	case c.Native && h.NoNative != "":
		return false, h.Name + ": " + h.NoNative
	case missing(c.Paths, h.Paths) != "":
		return false, lacks + missing(c.Paths, h.Paths)
	}
	return true, ""
}

// room is how many more runners of a class fit on the hosts that take it.
func room(s Snapshot, c Class, hosts map[string]Host, alloc map[string][2]float64) int {
	n := 0
	for name, h := range hosts {
		if ok, _ := takes(s, c, h); ok {
			cpu, mem := free(h, alloc[name])
			n += int(math.Min((cpu+1e-9)/c.CPUs, mem/float64(c.MemBytes)))
		}
	}
	return n
}

// reservedFor names the class whose reserve a runner of c on host would eat
// into ("" = none). A class with reserve: N is promised room for N runners;
// those it already has count. Another class is held back only where its
// runner would take that room away, not when the room is not there anyway.
func reservedFor(s Snapshot, c Class, host string, hosts map[string]Host, alloc map[string][2]float64, live map[string]int) string {
	var with map[string][2]float64
	for _, r := range s.Classes {
		need := r.Reserve - live[r.Name]
		if r.Name == c.Name || need <= 0 {
			continue
		}
		if with == nil {
			with = maps.Clone(alloc)
			a := with[host]
			with[host] = [2]float64{a[0] + c.CPUs, a[1] + float64(c.MemBytes)}
		}
		if before := room(s, r, hosts, alloc); room(s, r, hosts, with) < min(need, before) {
			return r.Name
		}
	}
	return ""
}

// OverMaxDuration starts the reason of a stop made because of a class's maxDuration.
const OverMaxDuration = "job ran longer than maxDuration "

// stopRule returns the removal of a runner that no longer serves a purpose.
func stopRule(s Snapshot, h Host, c Class, r Runner) (Stop, bool) {
	stop := func(state, format string, args ...any) (Stop, bool) {
		return Stop{Runner: r.Name, Host: r.Host, State: state, Reason: fmt.Sprintf(format, args...)}, true
	}
	switch {
	case r.Lost != "" && r.State != "":
		return stop("lost", "%s", r.Lost)
	case r.State == "" && r.Working:
		return Stop{}, false // unknown to the ledger, but never kill a running job
	case r.State == "":
		return stop("", "not in the ledger")
	case h.Name == "":
		// The host left the config: nothing can reach the container, only the ledger row goes.
		return stop("lost", "host %s is not configured", r.Host)
	case !h.Up:
		// A tunnel that is down says nothing about the job. Only runners without a
		// job are written off; busy ones wait for the agent's next snapshot.
		if (r.State == "starting" || r.State == "idle") && h.DownFor > s.HostLostAfter {
			return stop("lost", "host %s unreachable for %s", r.Host, h.DownFor.Round(time.Second))
		}
		return Stop{}, false
	case r.State == "done":
		return stop("", "job completed")
	case r.Container == "":
		return stop(failedUnlessJob(r), "container missing on %s", r.Host)
	case r.Container == "exited" && r.State == "busy":
		return stop("", "runner exited during or after its job (code %d); result comes with JobCompleted", r.ExitCode)
	case r.Container == "exited":
		return stop("failed", "exited before a job (code %d)", r.ExitCode)
	case c.MaxDuration > 0 && !r.JobStartedAt.IsZero() && s.Now.Sub(r.JobStartedAt) > c.MaxDuration:
		// The one stop of a running job pumasi makes on its own: the user asked for it.
		return stop("failed", "%s%s", OverMaxDuration, c.MaxDuration)
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

// missing is the first of want that is not in have ("" = all are there).
func missing(want, have []string) string {
	for _, p := range want {
		if !slices.Contains(have, p) {
			return p
		}
	}
	return ""
}

func osOf(os string) string {
	if os == "" {
		return "linux"
	}
	return os
}

func gib(b float64) string { return fmt.Sprintf("%.1fG", b/(1<<30)) }
