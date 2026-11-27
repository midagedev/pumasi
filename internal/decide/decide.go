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
	OS string
	// What a job of the class is taken to need while nothing is known of it:
	// the first estimate. A job with finished runs comes with its own size.
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
	// Overflow: the plan says how many more jobs of the class could be placed
	// right now (Plan.Room), each taken to be NextCPUs/NextMem large (0 = the
	// first estimate).
	Overflow bool
	NextCPUs float64
	NextMem  int64
	// MayBeHigh: some priority rule can make a job of the class high. While
	// only high jobs are placed (Snapshot.HighOnly), an overflow class offers
	// room only then, or for an expedited job of its own: a job it took
	// otherwise would only wait.
	MayBeHigh bool
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
	// KeepMem is left free on the machine whatever the budget says: memory
	// does not stretch, and what the others use is a sample a few seconds old.
	KeepMem int64
	PSICPU  float64
	PSIMem  float64
	PSIIO   float64
	// Pressed names what the host was over its threshold for at the last look
	// ("cpu", "memory", "io"): it opens again only well under, see Over.
	Pressed []string
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
	Name  string
	Class string
	Host  string
	// What the runner holds of its host's room: what its job is expected to
	// use, or what it uses now when that is more (0 = the class's first estimate).
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
	// Watched: its job was placed blind and has not yet run long enough to
	// show its size. Its charge already holds the extra room.
	Watched bool
}

// Demand is one job waiting for a runner of its class.
type Demand struct {
	ID        string
	Class     string
	FirstSeen time.Time
	// What the job is expected to use (0 = the class's first estimate), and
	// where that comes from, for the reason of its start.
	CPUs     float64
	MemBytes int64
	Sized    string
	// Name is the job's name for people ("" = not known).
	Name string
	// Level is the job's place in line while room is short: 1 high, 0 normal,
	// -1 low. Ranked says which rule gave it ("" = none).
	Level  int
	Ranked string
	// Blind: nothing is measured of the job or of its workflow; its size is a guess.
	Blind bool
	// Expedite: an operator put the job before every other one: 1 is the
	// first of those, 2 the next (0 = not expedited). It is a place, not a
	// level: it does not age, and it ends when the job starts.
	Expedite int
	// Pending: an expedited job its class has not been given yet: GitHub
	// still holds it (an overflow class takes only what fits). It keeps the
	// hosts it could go to as a waiting job does, and no runner is started
	// for it: a runner is handed only a job its scale set holds.
	Pending bool
}

type Snapshot struct {
	Now             time.Time
	Hosts           []Host
	Classes         []Class
	Demand          []Demand
	Runners         []Runner
	PSICPUThreshold float64
	PSIMemThreshold float64
	// PSIIOThreshold: a host whose disk makes its work wait this much gets no
	// new runner: a job's first minutes are its checkout and install.
	PSIIOThreshold float64
	// MaxStarting: at most this many runners come up on one host at a time
	// (0 = any number). Jobs that waited together would otherwise all start
	// in the tick the room came, and the disk shows it only after.
	MaxStarting int
	// HeadStart: a job of level L is placed as if it had come L*HeadStart earlier.
	HeadStart time.Duration
	// BlindHold: a blind job holds this many times its memory while it is
	// watched, and where a watched runner already is the next blind job needs
	// that much free (0 or 1 = blind jobs are placed like any other).
	BlindHold float64
	// HighOnly: while set, only expedited and high jobs (and the jobs ahead
	// of them in their class) get new runners; the others wait with it as
	// the reason. Running jobs go on.
	HighOnly         string
	ProvisionTimeout time.Duration
	IdleTimeout      time.Duration
	HostLostAfter    time.Duration
}

type Start struct {
	Class, Host, Reason string
	// The job the runner is started for and the room taken for it.
	Job      string
	CPUs     float64
	MemBytes int64
}

// Stop removes a runner. State is the ledger state to set ("" = keep).
type Stop struct {
	Runner, Host, State, Reason string
}

type Defer struct {
	Class  string
	Count  int
	Reason string
	// Priority: why the class's next job stands where it does in line
	// ("" = by its age alone). Defers come in the order of the line.
	Priority string
}

type Plan struct {
	Start []Start
	Stop  []Stop
	Defer []Defer
	// Room: for each overflow class, how many more jobs it could start now,
	// after this plan's starts and with the class's waiting jobs served first.
	Room map[string]int
	// Line: each job that waited for room at the start of the plan, in the
	// order it was given room, and what came of it.
	Line []Lined
}

// Lined is one job in the line and what the plan did for it.
type Lined struct {
	ID, Class, Name string
	// Level and Expedite as the job stands: a job ahead of a higher one in
	// its class has that one's (see Line).
	Level    int
	Expedite int
	Pending  bool
	Ranked   string // why it stands where it does ("" = by its age)
	Host     string // where a runner starts for it ("" = none)
	Reason   string // why it waits ("" = a runner starts for it)
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
	watched := map[string]int{}      // host -> runners of jobs nobody has measured, still watched
	starting := map[string]int{}     // host -> runners coming up
	for _, r := range runners {
		if st, ok := stopRule(s, hosts[r.Host], classes[r.Class], r); ok {
			p.Stop = append(p.Stop, st)
			continue
		}
		if r.Container == "exited" {
			continue
		}
		cpus, mem := r.CPUs, float64(r.MemBytes)
		if c, ok := classes[r.Class]; ok && cpus == 0 && mem == 0 {
			cpus, mem = c.CPUs, float64(c.MemBytes)
		}
		a := alloc[r.Host]
		alloc[r.Host] = [2]float64{a[0] + cpus, a[1] + mem}
		live[r.Class]++
		if !r.JobKnown {
			covering[r.Class]++
		}
		if r.Watched {
			watched[r.Host]++
		}
		if r.State == "starting" {
			starting[r.Host]++
		}
	}

	// 2. Demand not covered by a runner that is starting or idle: the newest jobs of each class.
	open := Line(s.Demand, covering, s.HeadStart)

	// 3. Oldest job first; a smaller class may fill a gap a bigger one cannot use.
	hostNames := make([]string, 0, len(hosts))
	for n := range hosts {
		hostNames = append(hostNames, n)
	}
	sort.Strings(hostNames)
	deferIdx := map[string]int{}
	var d Demand
	// wait records that d gets no runner in this plan. A pending job is not
	// its class's yet, so it is no defer of the class.
	wait := func(reason string) {
		p.Line = append(p.Line, lined(d, "", reason))
		if d.Pending {
			return
		}
		if i, ok := deferIdx[d.Class]; ok {
			p.Defer[i].Count++
			return
		}
		deferIdx[d.Class] = len(p.Defer)
		p.Defer = append(p.Defer, Defer{Class: d.Class, Count: 1, Reason: reason, Priority: d.Ranked})
	}
	// kept: hosts where a job waits for room. No job that stands lower is
	// started there meanwhile: it would take the room the other waits for.
	// Running jobs are never touched for it.
	type keep struct {
		rank  int
		class string
		what  string // the job it is kept for, for the reason of the others
	}
	kept := map[string]keep{}
	// behind: classes with a job that waits for room. GitHub gives a class's
	// jobs to its runners oldest first, whichever job a runner was started
	// for: a runner started for a later, smaller job would be handed the one
	// that waits, on a host that has no room for it.
	behind := map[string]bool{}
	// pendingRoom: classes with an expedited job GitHub still holds and room
	// kept for it: the class takes a job now (Room), and GitHub sends the next
	// of its labels. pendingFor: classes with such a job at all.
	pendingRoom := map[string]bool{}
	pendingFor := map[string]bool{}
	for _, d = range open {
		if d.Pending {
			pendingFor[d.Class] = true
		}
		c, ok := classes[d.Class]
		if !ok {
			wait("unknown class")
			continue
		}
		if behind[d.Class] {
			wait("behind an older job of its class that waits for room")
			continue
		}
		if s.HighOnly != "" && d.Expedite == 0 && d.Level <= 0 {
			wait(s.HighOnly)
			behind[d.Class] = true
			continue
		}
		if c.MaxRunners > 0 && live[c.Name] >= c.MaxRunners {
			wait(fmt.Sprintf("at maxRunners %d", c.MaxRunners))
			continue
		}
		// The job's own size where it is known; the class's first estimate otherwise.
		needCPU, needMem := c.CPUs, float64(c.MemBytes)
		if d.CPUs > 0 && d.MemBytes > 0 {
			needCPU, needMem = d.CPUs, float64(d.MemBytes)
		}
		// A job nobody has measured holds more than its guess while it is watched.
		// A host that watches none takes one on the guess alone, so that such a
		// job always gets measured somewhere; the next one there needs the hold.
		holdMem := needMem
		if d.Blind && s.BlindHold > 1 {
			holdMem = needMem * s.BlindHold
		}
		best, bestSlots := "", -1.0
		var why, short []string
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
			case s.MaxStarting > 0 && starting[hn] >= s.MaxStarting:
				why = append(why, fmt.Sprintf("%s has %d runners coming up; the next when they are", hn, starting[hn]))
				continue
			case freeCPU+1e-9 < needCPU || (freeMem < needMem && (watched[hn] == 0 || holdMem == needMem)):
				why = append(why, fmt.Sprintf("%s %.1fcpu/%s", hn, freeCPU, gib(freeMem)))
				short = append(short, hn)
				continue
			case watched[hn] > 0 && freeMem < holdMem:
				why = append(why, fmt.Sprintf("%s %.1fcpu/%s: it watches %d unmeasured job there and takes the next with %s free or when that one has shown its size",
					hn, freeCPU, gib(freeMem), watched[hn], gib(holdMem)))
				short = append(short, hn)
				continue
			}
			if k, ok := kept[hn]; ok && k.rank > rank(d) {
				why = append(why, hn+" kept for "+k.what)
				continue
			}
			if r := reservedFor(s, c, hn, needCPU, needMem, hosts, alloc, live); r != "" {
				why = append(why, hn+" reserved for "+r)
				continue
			}
			slots := math.Min(freeCPU/needCPU, freeMem/needMem)
			if best == "" || better(h, slots, hosts[best], bestSlots) {
				best, bestSlots = hn, slots
			}
		}
		if best == "" {
			if len(why) == 0 && lacking != "" {
				wait("no host has " + lacking)
				continue
			}
			if lacking != "" {
				why = append(why, "the others lack "+lacking)
			}
			if len(why) == 0 {
				why = append(why, "no allowed host")
			}
			wait(fmt.Sprintf("no host with %.1fcpu/%s free (%s)", needCPU, gib(needMem), strings.Join(why, ", ")))
			behind[c.Name] = true
			for _, hn := range short {
				// A host the job would not fit even with every runner gone is not
				// kept for it: nothing would come of the wait.
				if ec, em := Free(hosts[hn], 0, 0); ec+1e-9 < needCPU || em < needMem {
					continue
				}
				if k, ok := kept[hn]; !ok || rank(d) > k.rank {
					kept[hn] = keep{rank(d), c.Name, keptFor(d)}
				}
			}
			continue
		}
		if d.Pending {
			// GitHub still holds the job. The room is kept, and its class offers
			// it (Room), so that GitHub hands over the next job of its labels.
			kept[best] = keep{rank(d), c.Name, keptFor(d)}
			pendingRoom[c.Name] = true
			p.Line = append(p.Line, lined(d, "", fmt.Sprintf("GitHub still holds it; room kept on %s until GitHub gives class %s the next job of its labels", best, c.Name)))
			continue
		}
		h := hosts[best]
		freeCPU, freeMem := free(h, alloc[best])
		reason := fmt.Sprintf("takes %.1fcpu/%s of the free %.1fcpu/%s", needCPU, gib(needMem), freeCPU, gib(freeMem))
		if d.Sized != "" {
			reason += " " + d.Sized
		}
		if holdMem > needMem {
			reason += fmt.Sprintf("; holds %s while it is watched", gib(holdMem))
		}
		if d.Ranked != "" {
			reason += "; " + d.Ranked
		}
		p.Start = append(p.Start, Start{Class: c.Name, Host: best, Reason: reason, Job: d.ID, CPUs: needCPU, MemBytes: int64(needMem)})
		p.Line = append(p.Line, lined(d, best, ""))
		a := alloc[best]
		alloc[best] = [2]float64{a[0] + needCPU, a[1] + holdMem}
		if holdMem > needMem {
			watched[best]++
		}
		starting[best]++
		live[c.Name]++
	}

	// 4. What is left over for classes that take only what fits.
	waiting := map[string]int{}
	for _, d := range p.Defer {
		waiting[d.Class] += d.Count
	}
	for _, c := range s.Classes {
		if !c.Overflow {
			continue
		}
		if p.Room == nil {
			p.Room = map[string]int{}
		}
		next := c
		if c.NextCPUs > 0 && c.NextMem > 0 {
			next.CPUs, next.MemBytes = c.NextCPUs, c.NextMem
		}
		// A host kept for a job of another class that stands above an ordinary
		// one has no room for this class's next job: it would wait there.
		open := hosts
		for name, k := range kept {
			if k.class != c.Name && k.rank > 0 {
				if len(open) == len(hosts) {
					open = maps.Clone(hosts)
				}
				delete(open, name)
			}
		}
		n := 0
		if s.BlindHold <= 1 {
			n = room(s, next, open, alloc)
		} else {
			// The next job may be one nobody has measured, and such a job is placed
			// on the guess only where none is watched; where one is, it needs the
			// hold free. An overflow class says too little rather than too much:
			// a job it does not take goes to the other runners of its labels, while
			// one it takes and cannot place waits here.
			hold := float64(c.MemBytes) * s.BlindHold
			for name, h := range open {
				if ok, _ := takes(s, next, h); !ok {
					continue
				}
				cpu, mem := free(h, alloc[name])
				var byMem int
				switch {
				case c.NextMem > 0 && watched[name] == 0:
					byMem = int(mem / float64(next.MemBytes))
				case watched[name] == 0 && mem >= float64(c.MemBytes):
					byMem = 1 + max(0, int((mem-hold)/hold))
				default:
					byMem = int(mem / hold)
				}
				n += max(0, min(int((cpu+1e-9)/next.CPUs), byMem))
			}
		}
		// Room kept for an expedited job GitHub still holds: the class takes
		// one, whatever its usual job's size.
		if pendingRoom[c.Name] {
			n = max(n, 1)
		}
		// While only high jobs are placed, a class whose jobs no rule makes
		// high takes none: they would only wait here.
		if s.HighOnly != "" && !c.MayBeHigh && !pendingFor[c.Name] {
			n = 0
		}
		if c.MaxRunners > 0 {
			n = min(n, c.MaxRunners-live[c.Name])
		}
		// A job the class takes now goes behind the ones it already holds, so
		// while one of those waits for room there is none for another, however
		// small the next one is taken to be.
		if waiting[c.Name] > 0 {
			n = 0
		}
		p.Room[c.Name] = max(0, n)
	}
	return p
}

func lined(d Demand, host, reason string) Lined {
	return Lined{ID: d.ID, Class: d.Class, Name: d.Name, Level: d.Level, Expedite: d.Expedite, Pending: d.Pending,
		Ranked: d.Ranked, Host: host, Reason: reason}
}

// expedited ranks above every level.
const expedited = 1 << 20

// rank is how a job stands against another that wants the same room: an
// expedited job above every level (the first expedited above the next),
// then by level.
func rank(d Demand) int {
	if d.Expedite > 0 {
		return expedited - d.Expedite
	}
	return d.Level
}

// keptFor names the job a host is kept for, in the reason of the others.
func keptFor(d Demand) string {
	name := d.Name
	if name == "" {
		name = "a job"
	}
	if d.Expedite > 0 {
		return "expedited " + name + " of " + d.Class
	}
	return fmt.Sprintf("a %s job of %s", levelName(d.Level), d.Class)
}

const lacks = "lacks "

// Over reports whether a pressure keeps runners off a host. A host that was
// over (was) stays so until it is a fifth under the threshold: a pressure
// that hovers at the line would otherwise open and close the host every few
// seconds, and an overflow class would take a job only to give it back.
func Over(pressure, threshold float64, was bool) bool {
	if threshold <= 0 {
		return false
	}
	if was {
		return pressure >= threshold*0.8
	}
	return pressure >= threshold
}

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
	case Over(h.PSICPU, s.PSICPUThreshold, slices.Contains(h.Pressed, "cpu")):
		return false, fmt.Sprintf("%s cpu pressure %.0f", h.Name, h.PSICPU)
	case Over(h.PSIMem, s.PSIMemThreshold, slices.Contains(h.Pressed, "memory")):
		return false, fmt.Sprintf("%s memory pressure %.0f", h.Name, h.PSIMem)
	case Over(h.PSIIO, s.PSIIOThreshold, slices.Contains(h.Pressed, "io")):
		return false, fmt.Sprintf("%s disk pressure %.0f", h.Name, h.PSIIO)
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
			n += max(0, int(math.Min((cpu+1e-9)/c.CPUs, mem/float64(c.MemBytes))))
		}
	}
	return n
}

// reservedFor names the class whose reserve a runner of c on host would eat
// into ("" = none). A class with reserve: N is promised room for N runners;
// those it already has count. Another class is held back only where its
// runner would take that room away, not when the room is not there anyway.
func reservedFor(s Snapshot, c Class, host string, cpus, mem float64, hosts map[string]Host, alloc map[string][2]float64, live map[string]int) string {
	var with map[string][2]float64
	for _, r := range s.Classes {
		need := r.Reserve - live[r.Name]
		if r.Name == c.Name || need <= 0 {
			continue
		}
		if with == nil {
			with = maps.Clone(alloc)
			a := with[host]
			with[host] = [2]float64{a[0] + cpus, a[1] + mem}
		}
		if before := room(s, r, hosts, alloc); room(s, r, hosts, with) < min(need, before) {
			return r.Name
		}
	}
	return ""
}

// How the reasons of the stops begin that the controller treats differently
// from the rest. A Stop's Reason is a sentence for people; these are its part
// for code.
const (
	OverMaxDuration  = "job ran longer than maxDuration " // the class's maxDuration ended the job
	ProvisionTimeout = "provision timeout"                // never listened
	ExitedBeforeJob  = "exited before a job"
	ContainerMissing = "container missing"
)

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
		return stop(failedUnlessJob(r), "%s on %s", ContainerMissing, r.Host)
	case r.Container == "exited" && r.State == "busy":
		return stop("", "runner exited during or after its job (code %d); result comes with JobCompleted", r.ExitCode)
	case r.Container == "exited":
		return stop("failed", "%s (code %d)", ExitedBeforeJob, r.ExitCode)
	case c.MaxDuration > 0 && !r.JobStartedAt.IsZero() && s.Now.Sub(r.JobStartedAt) > c.MaxDuration:
		// The one stop of a running job pumasi makes on its own: the user asked for it.
		return stop("failed", "%s%s", OverMaxDuration, c.MaxDuration)
	case r.State == "starting" && r.ListeningAt.IsZero() && s.ProvisionTimeout > 0 && s.Now.Sub(r.CreatedAt) > s.ProvisionTimeout:
		return stop("failed", "%s: not listening %s after start", ProvisionTimeout, s.ProvisionTimeout)
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

// free is what a runner may still take on a host: what the machine has left
// after the runners' charges and everyone else's measured use, and no more
// than the budget leaves when the host has one. Nobody sets the first number:
// it moves with the machine.
func free(h Host, a [2]float64) (cpu, mem float64) { return Free(h, a[0], a[1]) }

// Free is free for a host whose runners hold cpus and mem.
func Free(h Host, cpus, held float64) (cpu, mem float64) {
	a := [2]float64{cpus, held}
	cpu = h.CPUs - a[0] - h.NonRunnerCPU
	mem = float64(h.MemBytes) - a[1] - float64(h.NonRunnerMem) - float64(h.KeepMem)
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

// Line is the waiting jobs in the order they are given room. covering says
// how many runners of each class are up without a job yet: that many of the
// class's oldest jobs are theirs and not in the line. A pending job comes
// last in its class: GitHub gives it to the class after the jobs it holds.
func Line(demand []Demand, covering map[string]int, headStart time.Duration) []Demand {
	byClass := map[string][]Demand{}
	for _, d := range demand {
		byClass[d.Class] = append(byClass[d.Class], d)
	}
	var open []Demand
	for c, ds := range byClass {
		sort.Slice(ds, func(i, j int) bool { return inClass(ds[i], ds[j]) })
		inherit(ds)
		held := 0
		for _, d := range ds {
			if !d.Pending {
				held++
			}
		}
		open = append(open, ds[min(covering[c], held):]...)
	}
	sort.Slice(open, func(i, j int) bool { return before(open[i], open[j], headStart) })
	return open
}

// inClass is the order GitHub gives a class's jobs to its runners: the
// oldest first, and a job it has not given the class yet after all of them.
func inClass(a, b Demand) bool {
	if a.Pending != b.Pending {
		return b.Pending
	}
	return less(a, b)
}

// inherit gives a class's waiting jobs (in class order) the standing of the
// most urgent job behind them: its level, and its place when it is
// expedited. GitHub hands a class's jobs to its runners oldest first,
// whichever job a runner was started for: an urgent job is served only after
// those before it, so they move up with it.
func inherit(ds []Demand) {
	for i := len(ds) - 2; i >= 0; i-- {
		next := ds[i+1]
		if rank(next) <= rank(ds[i]) {
			continue
		}
		if next.Expedite > 0 {
			ds[i].Expedite = next.Expedite
		}
		ds[i].Level = max(ds[i].Level, next.Level)
		name := next.Name
		if name == "" {
			name = "a job"
		}
		ds[i].Ranked = "ahead of " + name + " in its class, which is " + next.Ranked
		if strings.HasPrefix(next.Ranked, "ahead of ") {
			ds[i].Ranked = next.Ranked // the same job further back pulls both
		}
	}
}

// before is the line: expedited jobs first, in the order they were
// expedited; then by age, a job of level L counting as headStart*L older.
// However high a job's level, one that has waited long enough goes first.
func before(a, b Demand, headStart time.Duration) bool {
	if (a.Expedite > 0) != (b.Expedite > 0) {
		return a.Expedite > 0
	}
	if a.Expedite != b.Expedite {
		return a.Expedite < b.Expedite
	}
	if a.Expedite > 0 {
		return inClass(a, b) // one class: its own and the jobs it pulls
	}
	ta, tb := a.FirstSeen.Add(-time.Duration(a.Level)*headStart), b.FirstSeen.Add(-time.Duration(b.Level)*headStart)
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return less(a, b)
}

func levelName(l int) string {
	switch {
	case l > 0:
		return "high"
	case l < 0:
		return "low"
	}
	return "normal"
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
