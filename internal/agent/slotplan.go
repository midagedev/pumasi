package agent

import (
	"fmt"
	"math"
	"sort"

	"github.com/midagedev/pumasi/internal/api"
)

// With registered runners there is no count of waiting jobs to place: GitHub
// hands a job to whichever runner listens. What the host decides is how many
// listen. planSlots is that rule, a pure function like decide.Decide:
//
//   - What runs (jobs of slots, and the scale set runners on the host) is
//     taken from the budget; the rest is the room.
//   - Every class may listen with as many runners as fit into that room,
//     each class counted by itself. Two classes so listen for the same room,
//     and when one takes a job the idle listeners that no longer fit are
//     stopped.
//   - A listener is only ever stopped while it is idle. One that has taken a
//     job in the meantime stays (the caller finds that out when it stops it),
//     and the host is over its budget until that job ends.
//   - A class with reserve: N keeps room for N of its runners from the others.

// slotIn is one slot as the host sees it now.
type slotIn struct {
	Key   string
	Class string
	N     int
	// off | starting | listening | busy. busy: it has taken a job, or is
	// taking one.
	State string
	// Ready: an off slot that may be started now (it is not waiting to retry).
	Ready bool
	// Stoppable: its idle listener can be stopped without the risk of losing a job.
	Stoppable bool
	// Release: the operator wants the slot given up. It is not started again.
	Release bool
}

type planIn struct {
	Classes []api.SlotClass
	// Closed: why nothing new may listen on this host ("" = open).
	Closed string
	// The budget (0 = none) and what the host's other runners hold of it.
	BudgetCPUs, HeldCPUs float64
	BudgetMem, HeldMem   float64
	// What the machine has free beyond what runs now. Only starting a
	// listener looks at it: a busy moment of the machine closes no listener.
	// Negative = not known.
	MachineCPUs, MachineMem float64
	Slots                   []slotIn
}

type planOut struct {
	Start, Stop []string
	// Why an off slot stays off.
	Why map[string]string
	// Over: what runs is more than the budget ("" = it is not).
	Over string
}

func planSlots(in planIn) planOut {
	out := planOut{Why: map[string]string{}}
	classes := map[string]api.SlotClass{}
	for _, c := range in.Classes {
		classes[c.Name] = c
	}
	byClass := map[string][]slotIn{}
	busy := map[string]int{}
	idleOrOff := map[string]int{} // slots of a class that could still take a job
	for _, s := range in.Slots {
		byClass[s.Class] = append(byClass[s.Class], s)
		c, known := classes[s.Class]
		switch {
		case s.State == "busy":
			busy[s.Class]++
		case known && s.N <= c.Slots && !s.Release:
			idleOrOff[s.Class]++
		}
	}
	usedCPU, usedMem := in.HeldCPUs, in.HeldMem
	for name, n := range busy {
		if c, ok := classes[name]; ok {
			usedCPU += float64(n) * c.CPUs
			usedMem += float64(n) * float64(c.MemBytes)
		}
	}
	inf := math.Inf(1)
	roomCPU, roomMem := inf, inf
	if in.BudgetCPUs > 0 {
		roomCPU = in.BudgetCPUs - usedCPU
	}
	if in.BudgetMem > 0 {
		roomMem = in.BudgetMem - usedMem
	}
	if roomCPU < -1e-9 || roomMem < 0 {
		out.Over = fmt.Sprintf("running %.1fcpu/%s on a budget of %.1fcpu/%s", usedCPU, gibF(usedMem), in.BudgetCPUs, gibF(in.BudgetMem))
	}
	machineCPU, machineMem := inf, inf
	if in.MachineCPUs >= 0 {
		machineCPU = in.MachineCPUs
	}
	if in.MachineMem >= 0 {
		machineMem = in.MachineMem
	}

	names := make([]string, 0, len(byClass))
	for name := range byClass {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		slots := byClass[name]
		sort.Slice(slots, func(i, j int) bool { return slots[i].N < slots[j].N })
		c, known := classes[name]
		closed := ""
		switch {
		case !known:
			closed = "class " + name + " is not in this host's plan"
		case c.Closed != "":
			closed = c.Closed
		case in.Closed != "":
			closed = in.Closed
		}
		keep, start := 0, 0
		noRoom := ""
		if closed == "" {
			// Room the other classes' reserves keep from this one.
			resCPU, resMem, resFor := 0.0, 0.0, ""
			for _, r := range in.Classes {
				if r.Name == name || r.Closed != "" {
					continue
				}
				if need := min(r.Reserve-busy[r.Name], idleOrOff[r.Name]); need > 0 {
					resCPU += float64(need) * r.CPUs
					resMem += float64(need) * float64(r.MemBytes)
					resFor = r.Name
				}
			}
			fit := func(cpu, mem float64) int {
				n := math.Min((cpu+1e-9)/c.CPUs, mem/float64(c.MemBytes))
				if n > 1e6 {
					return 1 << 20
				}
				return max(0, int(n))
			}
			limit := c.Slots - busy[name]
			keep = max(0, min(limit, fit(roomCPU-resCPU, roomMem-resMem)))
			start = min(keep, fit(math.Min(roomCPU, machineCPU)-resCPU, math.Min(roomMem, machineMem)-resMem))
			free := fmt.Sprintf("%.1fcpu/%s free of the budget", math.Max(roomCPU, 0), gibF(math.Max(roomMem, 0)))
			switch {
			case fit(roomCPU, roomMem) == 0:
				noRoom = fmt.Sprintf("no room: %s, the class needs %.1fcpu/%s", free, c.CPUs, gibF(float64(c.MemBytes)))
			case keep < limit && keep < fit(roomCPU, roomMem):
				noRoom = "the room left is reserved for " + resFor
			case start < keep:
				noRoom = fmt.Sprintf("the machine is busy: %.1fcpu/%s free", math.Max(machineCPU, 0), gibF(math.Max(machineMem, 0)))
			case keep < limit:
				noRoom = "no room for one more: " + free
			}
		}

		var idle []slotIn // listening or starting, lowest number first
		for _, s := range slots {
			over := known && s.N > c.Slots
			switch {
			case s.State == "busy":
				continue
			case s.State == "off":
				switch {
				case s.Release:
					out.Why[s.Key] = "being released"
				case over:
					out.Why[s.Key] = fmt.Sprintf("the class has slots: %d", c.Slots)
				case closed != "":
					out.Why[s.Key] = closed
				}
				continue
			case s.Release || over:
				if s.Stoppable {
					out.Stop = append(out.Stop, s.Key)
				}
				continue
			}
			idle = append(idle, s)
		}
		// Too many listen: stop from the highest number down, those still
		// starting before those that listen.
		if extra := len(idle) - keep; extra > 0 {
			order := append([]slotIn{}, idle...)
			sort.SliceStable(order, func(i, j int) bool {
				if (order[i].State == "starting") != (order[j].State == "starting") {
					return order[i].State == "starting"
				}
				return order[i].N > order[j].N
			})
			for _, s := range order {
				if extra == 0 {
					break
				}
				if s.Stoppable {
					out.Stop = append(out.Stop, s.Key)
					extra--
				}
			}
		}
		want := start - len(idle)
		for _, s := range slots {
			if s.State != "off" || s.Release || (known && s.N > c.Slots) || closed != "" {
				continue
			}
			if want > 0 && s.Ready {
				out.Start = append(out.Start, s.Key)
				want--
				continue
			}
			if out.Why[s.Key] == "" && want <= 0 {
				out.Why[s.Key] = noRoom
			}
		}
	}
	sort.Strings(out.Start)
	sort.Strings(out.Stop)
	return out
}

func gibF(b float64) string {
	if math.IsInf(b, 1) || b >= math.MaxInt64 {
		return "-"
	}
	return fmt.Sprintf("%.1fG", b/(1<<30))
}
