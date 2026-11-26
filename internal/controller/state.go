package controller

import (
	"cmp"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

type State struct {
	Controller bool              `json:"controller"` // a `pumasi up` is running and answered
	Incidents  []ledger.Incident `json:"incidents"`  // open problems
	Hosts      []HostState       `json:"hosts"`
	Runners    []RunnerState     `json:"runners"`
	Waiting    []WaitingState    `json:"waiting"`
}

type HostState struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	OS         string  `json:"os"`       // linux | macos | windows; "" until the agent has answered
	UsedCPUs   float64 `json:"usedCpus"` // sum of the sizes of the runners placed here
	BudgetCPUs float64 `json:"budgetCpus"`
	UsedMem    int64   `json:"usedMem"`
	BudgetMem  int64   `json:"budgetMem"`
	// CPU pressure (PSI some avg10, percent); nil when the agent did not answer.
	CPUPressure *float64 `json:"cpuPressure"`
	Runners     int      `json:"runners"`
	Reachable   *bool    `json:"reachable"` // nil = unknown, no controller running
	// Closed: why the host, though reachable, gets no new runners ("quarantined", "disk 4% free").
	Closed string `json:"closed"`
	// Free share of the disk that holds the runners, in percent; nil when unknown.
	DiskFreePct *float64 `json:"diskFreePct"`
	// The connection: seconds since the last answer (nil = none yet), how long
	// that answer took, the agent's build and why the host was last cut off.
	// Empty without a running controller.
	SeenSeconds *int64  `json:"seenSeconds"`
	RTTMs       float64 `json:"rttMs"`
	Agent       string  `json:"agent"`
	LastDrop    string  `json:"lastDrop"`
}

type RunnerState struct {
	Name       string `json:"name"`
	Class      string `json:"class"`
	Host       string `json:"host"`
	State      string `json:"state"`
	Job        string `json:"job"`
	AgeSeconds int64  `json:"ageSeconds"`
	// From the agent's last sample; zero without a running controller.
	CPUSec   float64 `json:"cpuSec"`
	MemBytes int64   `json:"memBytes"` // memory in use now
	MemLimit int64   `json:"memLimit"` // the class size
	// The runner's number among its class's runners on the host ({slot}); 0 = not known.
	Slot int `json:"slot"`
}

type WaitingState struct {
	Class  string `json:"class"`
	Jobs   int    `json:"jobs"`
	Reason string `json:"reason"`
	// How long the oldest open job of the class has waited.
	OldestSeconds int64 `json:"oldestSeconds"`
}

// CollectState joins the ledger with what a running controller knows (live, ok).
// Without a controller it is the ledger alone.
func CollectState(cfg *config.Config, l *ledger.Ledger, live Status, ok bool, now time.Time) (State, error) {
	out := State{Hosts: []HostState{}, Runners: []RunnerState{}, Waiting: []WaitingState{}}
	out.Controller = ok
	var err error

	if out.Incidents, err = l.OpenIncidents(); err != nil {
		return out, err
	}
	runners, err := l.LiveRunners()
	if err != nil {
		return out, err
	}
	size := map[string]config.Class{}
	for _, c := range cfg.Classes {
		size[c.Name] = c
	}
	hosts := map[string]int{} // name -> index in out.Hosts
	for _, h := range KnownHosts(cfg) {
		hs := HostState{Name: h.Name, Kind: h.Kind(), BudgetCPUs: h.Budget.CPUs, BudgetMem: int64(h.Budget.Memory)}
		if ok {
			snap := live.Hosts[h.Name]
			up := snap != nil
			hs.Reachable = &up
			if ln, ok := live.Links[h.Name]; ok {
				hs.RTTMs, hs.Agent, hs.LastDrop = ln.RTTMs, ln.Agent, ln.LastDrop
				if !ln.SeenAt.IsZero() {
					s := int64(now.Sub(ln.SeenAt).Seconds())
					hs.SeenSeconds = &s
				}
			}
			if up {
				hs.OS = cmp.Or(snap.OS, "linux")
				if p, ok := snap.PSI["cpu"]; ok {
					hs.CPUPressure = &p
				}
				hs.Closed = live.Closed[h.Name]
				if snap.DiskTotal > 0 {
					d := 100 * float64(snap.DiskFree) / float64(snap.DiskTotal)
					hs.DiskFreePct = &d
				}
			}
		}
		out.Hosts = append(out.Hosts, hs)
		hosts[h.Name] = len(out.Hosts) - 1
	}
	sampled := map[string]api.Runner{}
	for _, snap := range live.Hosts {
		if snap != nil {
			for _, r := range snap.Runners {
				sampled[r.Name] = r
			}
		}
	}
	for _, r := range runners {
		rs := RunnerState{Name: r.Name, Class: r.Class, Host: r.Host, State: r.State,
			Job: r.Job, AgeSeconds: int64(now.Sub(r.CreatedAt).Seconds()), MemLimit: int64(size[r.Class].Memory)}
		if a, ok := sampled[r.Name]; ok {
			rs.CPUSec, rs.MemBytes, rs.Slot = a.CPUSec, a.MemCurrent, a.Slot
		}
		out.Runners = append(out.Runners, rs)
		if i, ok := hosts[r.Host]; ok {
			h := &out.Hosts[i]
			h.Runners++
			h.UsedCPUs += size[r.Class].CPUs
			h.UsedMem += int64(size[r.Class].Memory)
		}
	}

	open, err := l.OpenDemand() // oldest first
	if err != nil {
		return out, err
	}
	oldest := func(class string) int64 {
		for _, d := range open {
			if d.Class == class {
				return int64(now.Sub(d.FirstSeen).Seconds())
			}
		}
		return 0
	}
	if ok {
		for _, w := range live.Waiting {
			out.Waiting = append(out.Waiting, WaitingState{Class: w.Class, Jobs: w.Jobs, Reason: w.Reason, OldestSeconds: oldest(w.Class)})
		}
		return out, nil
	}
	// No controller: every open job in the ledger waits, with the last reason recorded for its class.
	idx := map[string]int{}
	for _, d := range open {
		i, seen := idx[d.Class]
		if !seen {
			i = len(out.Waiting)
			idx[d.Class] = i
			out.Waiting = append(out.Waiting, WaitingState{Class: d.Class, Reason: l.LastDefer(d.Class), OldestSeconds: oldest(d.Class)})
		}
		out.Waiting[i].Jobs++
	}
	return out, nil
}
