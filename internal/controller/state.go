package controller

import (
	"cmp"
	"path"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

type State struct {
	Controller bool              `json:"controller"` // a `pumasi up` is running and answered
	Incidents  []ledger.Incident `json:"incidents"`  // open problems
	Hosts      []HostState       `json:"hosts"`
	Runners    []RunnerState     `json:"runners"`
	Waiting    []WaitingState    `json:"waiting"`
	// Sizes: what pumasi has learned of the jobs it ran, the most recent first.
	Sizes []JobSize `json:"sizes"`
}

// JobSize is a job's size as placement uses it for its next run.
type JobSize struct {
	Repo     string  `json:"repo"`
	Workflow string  `json:"workflow"` // the workflow file's name
	Job      string  `json:"job"`
	EstCPUs  float64 `json:"estCpus"`
	EstMem   int64   `json:"estMem"`
	LastCPUs float64 `json:"lastCpus"` // its newest measured run
	LastMem  int64   `json:"lastMem"`
	TopCPUs  float64 `json:"topCpus"` // the largest of the runs that count
	TopMem   int64   `json:"topMem"`
	Runs     int     `json:"runs"`
}

type HostState struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	OS         string  `json:"os"`         // linux | macos | windows; "" until the agent has answered
	UsedCPUs   float64 `json:"usedCpus"`   // what the runners here hold: each its estimate, or its use when that is more
	BudgetCPUs float64 `json:"budgetCpus"` // the most pumasi lends here; 0 = whatever is free
	UsedMem    int64   `json:"usedMem"`
	BudgetMem  int64   `json:"budgetMem"`
	// The machine, and what a new runner could take right now (measured: the
	// machine less the runners, everyone else's use and the memory kept free).
	// Zero when the agent did not answer.
	CPUs     float64 `json:"cpus"`
	MemBytes int64   `json:"memBytes"`
	FreeCPUs float64 `json:"freeCpus"`
	FreeMem  int64   `json:"freeMem"`
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
	CPUNow   float64 `json:"cpuNow"`   // cores in use now
	MemBytes int64   `json:"memBytes"` // memory in use now, page cache left out where the OS tells them apart
	MemLimit int64   `json:"memLimit"` // what the OS holds it to (0 = nothing)
	// What placement counts on for its job, and from how many measured runs (0 = the class's first estimate).
	EstCPUs float64 `json:"estCpus"`
	EstMem  int64   `json:"estMem"`
	EstRuns int     `json:"estRuns"`
	// The runner's number among its class's runners on the host ({slot}); 0 = not known.
	Slot int `json:"slot"`
}

type WaitingState struct {
	Class  string `json:"class"`
	Jobs   int    `json:"jobs"`
	Reason string `json:"reason"`
	// Order: the class's place in the line for room (1 = next), and why when
	// a priority rule put it there ("" = by its age). 0 = no controller to say.
	Order    int    `json:"order"`
	Priority string `json:"priority"`
	// How long the oldest open job of the class has waited.
	OldestSeconds int64 `json:"oldestSeconds"`
}

// CollectState joins the ledger with what a running controller knows (live, ok).
// Without a controller it is the ledger alone.
func CollectState(cfg *config.Config, l *ledger.Ledger, live Status, ok bool, now time.Time) (State, error) {
	out := State{Hosts: []HostState{}, Runners: []RunnerState{}, Waiting: []WaitingState{}, Sizes: []JobSize{}}
	out.Controller = ok
	var err error

	if out.Incidents, err = l.OpenIncidents(); err != nil {
		return out, err
	}
	runners, err := l.LiveRunners()
	if err != nil {
		return out, err
	}
	classes := map[string]config.Class{}
	for _, c := range cfg.Classes {
		classes[c.Name] = c
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
			Job: r.Job, AgeSeconds: int64(now.Sub(r.CreatedAt).Seconds()),
			EstCPUs: cmp.Or(r.EstCPUs, classes[r.Class].CPUs), EstMem: cmp.Or(r.EstMem, int64(classes[r.Class].Memory)), EstRuns: r.EstRuns}
		if a, ok := sampled[r.Name]; ok {
			rs.CPUSec, rs.CPUNow, rs.MemBytes, rs.MemLimit, rs.Slot = a.CPUSec, a.CPUNow, cmp.Or(a.MemWork, a.MemCurrent), a.LimitMem, a.Slot
		}
		out.Runners = append(out.Runners, rs)
		if i, ok := hosts[r.Host]; ok {
			h := &out.Hosts[i]
			h.Runners++
			cpus, mem := size.Charge(size.Estimate{CPUs: rs.EstCPUs, MemBytes: rs.EstMem}, rs.CPUNow, rs.MemBytes)
			h.UsedCPUs += cpus
			h.UsedMem += mem
		}
	}
	for i := range out.Hosts {
		h := &out.Hosts[i]
		if snap := live.Hosts[h.Name]; ok && snap != nil {
			h.CPUs, h.MemBytes = snap.CPUs, snap.MemBytes
			h.BudgetCPUs = minPositive(h.BudgetCPUs, snap.BudgetCPUs)
			h.BudgetMem = int64(minPositive(float64(h.BudgetMem), float64(snap.BudgetMem)))
			cpu, mem := decide.Free(decide.Host{CPUs: snap.CPUs, MemBytes: snap.MemBytes, BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem,
				NonRunnerCPU: snap.NonRunnerCPU, NonRunnerMem: snap.NonRunnerMem, KeepMem: keepMem(snap.MemBytes)}, h.UsedCPUs, float64(h.UsedMem))
			h.FreeCPUs, h.FreeMem = cpu, int64(mem)
		}
	}
	keys, err := l.MeasuredJobs(50)
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		runs, err := l.JobRuns(k, size.Window)
		if err != nil || len(runs) == 0 {
			continue
		}
		e := size.Learn(runs, size.Estimate{})
		out.Sizes = append(out.Sizes, JobSize{Repo: k.Repo, Workflow: path.Base(k.Workflow), Job: k.Job, EstCPUs: e.CPUs, EstMem: e.MemBytes,
			LastCPUs: runs[0].CPUs, LastMem: runs[0].MemBytes, TopCPUs: e.TopCPUs, TopMem: e.TopMem, Runs: e.Runs})
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
		for i, w := range live.Waiting {
			out.Waiting = append(out.Waiting, WaitingState{Class: w.Class, Jobs: w.Jobs, Reason: w.Reason, Order: i + 1, Priority: w.Priority, OldestSeconds: oldest(w.Class)})
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
