package controller

import (
	"cmp"
	"path"
	"regexp"
	"strings"
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
	Overflow   []Overflow        `json:"overflow"`   // classes that take only what fits: their jobs elsewhere are not in Waiting
	Warnings   []string          `json:"warnings"`   // the setup's own risks (jobs that can read the controller's credentials)
	Hosts      []HostState       `json:"hosts"`
	Runners    []RunnerState     `json:"runners"`
	Waiting    []WaitingState    `json:"waiting"`
	// WaitingJobs: each job that waits here, the oldest first, with its class's reason.
	WaitingJobs []JobWait `json:"waitingJobs"`
	// Mode: the operating switch (normal | high-only); Expedited: the jobs
	// put before every other one.
	Mode      Mode        `json:"mode"`
	Expedited []Expedited `json:"expedited"`
	// TakenBack: jobs GitHub took back before a runner had them, in the last
	// hour: given back for want of room (they run elsewhere) or cancelled.
	TakenBack []JobWait `json:"takenBack"`
	// RecentIncidents: problems resolved in the last day, the oldest first.
	RecentIncidents []ledger.Incident `json:"recentIncidents"`
	// Sizes: what pumasi has learned of the jobs it ran, the most recent first.
	Sizes []JobSize `json:"sizes"`
	// Classes: the config's classes, in its order.
	Classes []ClassState `json:"classes"`
	// Limits: the lines the controller holds hosts and waiting jobs to.
	Limits StateLimits `json:"limits"`
}

// JobExpect is when a waiting job is expected to start: in so many
// seconds, on a host, once a job ends ("" = there is room, or its runner is
// coming up). A forecast from the jobs that run and wait now, with the run
// times their last runs took.
type JobExpect struct {
	InSeconds int64  `json:"inSeconds"`
	Host      string `json:"host"`
	After     string `json:"after,omitempty"`
}

// ClassState is a class as the config gives it, defaults filled in.
type ClassState struct {
	Name     string   `json:"name"`
	Labels   []string `json:"labels"` // what its jobs' runs-on must be among
	OS       string   `json:"os"`
	Driver   string   `json:"driver"`
	Docker   string   `json:"docker,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	Mounts   []string `json:"mounts,omitempty"`
	Overflow bool     `json:"overflow,omitempty"`
	After    string   `json:"after,omitempty"`
	Reserve  int      `json:"reserve,omitempty"`
	// The first estimate: what a job of the class never measured is placed with.
	CPUs      float64 `json:"cpus"`
	Memory    int64   `json:"memory"`
	LimitCPUs float64 `json:"limitCpus,omitempty"`
	LimitMem  int64   `json:"limitMem,omitempty"`
	// MaxDurationSeconds: a job running longer is stopped (0 = never).
	MaxDurationSeconds int64 `json:"maxDurationSeconds,omitempty"`
	// Warm: runners the class keeps up without a job now, learned from how
	// its jobs came in the last 30 minutes (0 = none).
	Warm int `json:"warm,omitempty"`
	// Soon: jobs of the class expected within the time a runner takes to
	// come up, as the jobs they need end; a runner is kept up for each.
	Soon int `json:"soon,omitempty"`
	// WarmMin: of its runners up without a job, how many the class keeps
	// whatever waits: they do not step aside for another class's job.
	WarmMin int `json:"warmMin,omitempty"`
}

// StateLimits: a host over a pressure line or under diskMinFree takes no new
// runners; a job waiting waitAlertSeconds is a problem; a high job counts as
// having come priorityHeadStartSeconds earlier, a low one that much later.
type StateLimits struct {
	PSICPU                   float64 `json:"psiCpu"`
	PSIMemory                float64 `json:"psiMemory"`
	PSIIO                    float64 `json:"psiIo"`
	DiskMinFree              float64 `json:"diskMinFree"`
	WaitAlertSeconds         int64   `json:"waitAlertSeconds"`
	PriorityHeadStartSeconds int64   `json:"priorityHeadStartSeconds"`
}

// JobSize is a job's size as placement uses it for its next run.
type JobSize struct {
	Repo     string  `json:"repo"`
	Workflow string  `json:"workflow"` // the workflow file's name
	Job      string  `json:"job"`
	Platform string  `json:"platform"` // what it ran as: "linux/docker"
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
	// CPUShare: how many times placement counts the cores here (cpuOvercommit
	// on a server whose cores runners share; FreeCPUs counts them so). 0 = once.
	CPUShare float64 `json:"cpuShare,omitempty"`
	// CPU pressure (PSI some avg10, percent); nil when the agent did not answer.
	CPUPressure *float64 `json:"cpuPressure"`
	// Memory and disk pressure the same way; nil where the OS does not tell.
	MemPressure *float64 `json:"memPressure"`
	IOPressure  *float64 `json:"ioPressure"`
	// What everything but the runners uses on the machine, and the memory kept
	// free whatever the budget says. Zero without a running controller.
	OtherCPUs float64 `json:"otherCpus"`
	OtherMem  int64   `json:"otherMem"`
	KeepMem   int64   `json:"keepMem"`
	Runners   int     `json:"runners"`
	Reachable *bool   `json:"reachable"` // nil = unknown, no controller running
	// Closed: why the host, though reachable, gets no new runners ("quarantined", "disk 4% free").
	Closed string `json:"closed"`
	// Free share of the fullest disk runners live on, in percent; nil when unknown.
	DiskFreePct *float64 `json:"diskFreePct"`
	// DiskPath: a path on that disk, as the agent names it ("" = not said).
	DiskPath string `json:"diskPath,omitempty"`
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
	// Watched: placed on a guess (nothing measured of the job or its workflow);
	// it holds size.BlindHold times EstMem until its job has run size.Settle.
	Watched bool `json:"watched,omitempty"`
	// What placement counts the runner as holding of its host: its estimate
	// (times the hold while watched), or what it uses now when that is more.
	HeldCPUs float64 `json:"heldCpus"`
	HeldMem  int64   `json:"heldMem"`
	// The runner's number among its class's runners on the host ({slot}); 0 = not known.
	Slot int `json:"slot"`
	// GivesWay: up without a job, it steps aside for a waiting job of another
	// class that needs its room (one of its own class it simply takes).
	GivesWay bool `json:"givesWay,omitempty"`
	// Ahead: a runner with no job started for a job expected to come, and
	// after what ("package (amd64) expected in ~4s, when release-check ends"):
	// the runner it holds is for that job.
	Ahead string `json:"ahead,omitempty"`
}

// JobWait is one job that waits for a runner, or that waited and left.
type JobWait struct {
	// ID: pumasi's id of the job, what POST /api/expedite takes.
	ID       string `json:"id,omitempty"`
	Class    string `json:"class"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"` // the workflow file's name
	Job      string `json:"job"`
	RunID    int64  `json:"runId"`
	// How long it has waited, or had when it left.
	WaitedSeconds int64 `json:"waitedSeconds"`
	// Reason: why its class's jobs wait ("" when it left, or nobody can say).
	Reason string `json:"reason,omitempty"`
	// LeftAt: when GitHub took it back (zero = it still waits).
	LeftAt time.Time `json:"leftAt,omitzero"`
	// Waiting jobs only: the priority level ("high", "low"; "" = normal;
	// "expedite" for a job expedited by hand or pulled along by one behind it
	// in its class), the rule that gave it or the job it moves up with, and
	// the job's place in the whole line (1 = next; 0 = a runner of its class
	// that is up takes it), as `pumasi priority` works it out.
	Level    string `json:"level,omitempty"`
	Ranked   string `json:"ranked,omitempty"`
	Position int    `json:"position,omitempty"`
	// Expedited by hand (`pumasi expedite`), and GitHub's id of the job when
	// it was named by it or by its URL.
	Expedited   bool  `json:"expedited,omitempty"`
	GithubJobID int64 `json:"githubJobId,omitempty"`
	// Expect: waiting jobs only, when it is expected to start, if anything in
	// sight lets it (see Status.Expect).
	Expect *JobExpect `json:"expect,omitempty"`
	// OverLimit: the memory it would need, when that is past its class's
	// hard limit: it is held at the limit, and will be cut short there again.
	OverLimit int64 `json:"overLimit,omitempty"`
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
	out := State{Hosts: []HostState{}, Runners: []RunnerState{}, Waiting: []WaitingState{}, Sizes: []JobSize{},
		WaitingJobs: []JobWait{}, TakenBack: []JobWait{}, RecentIncidents: []ledger.Incident{}, Expedited: []Expedited{}, Classes: classStates(cfg)}
	out.Limits = StateLimits{PSICPU: cfg.PSICPUThreshold, PSIMemory: cfg.PSIMemThreshold, PSIIO: cfg.PSIIOThreshold, DiskMinFree: cfg.DiskMinFree,
		WaitAlertSeconds: int64(cfg.WaitAlert.Seconds()), PriorityHeadStartSeconds: int64(cfg.PriorityHeadStart.Seconds())}
	out.Controller = ok
	out.Warnings = append([]string{}, live.Warnings...)
	out.Overflow = append([]Overflow{}, live.Overflow...)
	for i := range out.Classes {
		out.Classes[i].Warm, out.Classes[i].Soon = live.Warm[out.Classes[i].Name], live.Soon[out.Classes[i].Name]
	}
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
				psi := func(k string) *float64 {
					if p, ok := snap.PSI[k]; ok {
						return &p
					}
					return nil
				}
				hs.CPUPressure, hs.MemPressure, hs.IOPressure = psi("cpu"), psi("memory"), psi("io")
				hs.OtherCPUs, hs.OtherMem, hs.KeepMem = snap.NonRunnerCPU, snap.NonRunnerMem, keepMem(snap.MemBytes)
				hs.Closed = live.Closed[h.Name]
				if snap.DiskTotal > 0 {
					d := 100 * float64(snap.DiskFree) / float64(snap.DiskTotal)
					hs.DiskFreePct = &d
					hs.DiskPath = snap.DiskPath
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
	giving := map[string]bool{}
	for _, n := range live.GiveWay {
		giving[n] = true
	}
	for _, r := range runners {
		rs := RunnerState{GivesWay: giving[r.Name] && r.Job == "", Name: r.Name, Class: r.Class, Host: r.Host, State: r.State,
			Job: r.Job, AgeSeconds: int64(now.Sub(r.CreatedAt).Seconds()),
			EstCPUs: cmp.Or(r.EstCPUs, classes[r.Class].CPUs), EstMem: cmp.Or(r.EstMem, int64(classes[r.Class].Memory)), EstRuns: r.EstRuns}
		if a, ok := sampled[r.Name]; ok {
			rs.CPUSec, rs.CPUNow, rs.MemBytes, rs.MemLimit, rs.Slot = a.CPUSec, a.CPUNow, cmp.Or(a.MemWork, a.MemCurrent), a.LimitMem, a.Slot
		}
		if r.Job == "" {
			if ds, _, err := l.RunnerHistory(r.Name); err == nil {
				for _, d := range ds {
					if d.Action == "start" {
						rs.Ahead = aheadOf(d.Reason)
						break
					}
				}
			}
		}
		rs.Watched = size.Estimate{Blind: r.EstBlind}.Watched(r.StartedAt, now)
		rs.HeldCPUs, rs.HeldMem = size.Charge(size.Estimate{CPUs: rs.EstCPUs, MemBytes: rs.EstMem}, rs.CPUNow, rs.MemBytes, rs.Watched)
		out.Runners = append(out.Runners, rs)
		if i, ok := hosts[r.Host]; ok {
			h := &out.Hosts[i]
			h.Runners++
			h.UsedCPUs += rs.HeldCPUs
			h.UsedMem += rs.HeldMem
		}
	}
	for i := range out.Hosts {
		h := &out.Hosts[i]
		if snap := live.Hosts[h.Name]; ok && snap != nil {
			h.CPUs, h.MemBytes = snap.CPUs, snap.MemBytes
			h.BudgetCPUs = minPositive(h.BudgetCPUs, snap.BudgetCPUs)
			h.BudgetMem = int64(minPositive(float64(h.BudgetMem), float64(snap.BudgetMem)))
			_, psi := snap.PSI["cpu"]
			dh := decide.Host{Kind: h.Kind, CPUs: snap.CPUs, MemBytes: snap.MemBytes, BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem,
				NonRunnerCPU: snap.NonRunnerCPU, NonRunnerMem: snap.NonRunnerMem, KeepMem: keepMem(snap.MemBytes), CPUPressureKnown: psi}
			h.FreeCPUs, h.FreeMem, h.CPUShare = hostFree(cfg, dh, h.UsedCPUs, h.UsedMem)
		}
	}
	hostMem := map[string]int64{}
	for name, snap := range live.Hosts {
		if snap != nil {
			hostMem[name] = snap.MemBytes
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
		withHostMem(runs, hostMem)
		e := size.Learn(runs, size.Estimate{})
		out.Sizes = append(out.Sizes, JobSize{Repo: k.Repo, Workflow: path.Base(k.Workflow), Job: k.Job, Platform: k.Platform, EstCPUs: e.CPUs, EstMem: e.MemBytes,
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
	wait := func(d ledger.Demand) JobWait {
		return JobWait{ID: d.JobID, Class: d.Class, Repo: d.Repo, Workflow: path.Base(ledger.KeyOf(d.Repo, d.Workflow, "").Workflow), Job: d.Job, RunID: d.RunID}
	}
	reason := map[string]string{}
	for _, w := range live.Waiting {
		reason[w.Class] = w.Reason
	}
	pv, err := CollectPriority(cfg, l, live, ok, now)
	if err != nil {
		return out, err
	}
	out.Mode, out.Expedited = pv.Mode, pv.Expedited
	placed := map[string]LineJob{}
	for _, j := range pv.Line {
		placed[j.Job] = j
	}
	for _, d := range open {
		j := wait(d)
		j.WaitedSeconds, j.Reason = int64(now.Sub(d.FirstSeen).Seconds()), reason[d.Class]
		if lj, ok := placed[d.JobID]; ok {
			j.Level, j.Ranked, j.Position, j.GithubJobID, j.Expedited = lj.Level, lj.ranked, lj.Position, lj.GitHubJobID, lj.expedited
			if j.Level == "normal" {
				j.Level = ""
			}
			if lj.WaitingFor != "" && lj.Position > 0 {
				j.Reason = lj.WaitingFor
			}
		}
		if e, ok := live.Expect[d.JobID]; ok {
			j.Expect = &JobExpect{InSeconds: int64(max(0, e.StartsAt.Sub(now).Seconds())), Host: e.Host, After: e.After}
		}
		j.OverLimit = live.Over[d.JobID]
		out.WaitingJobs = append(out.WaitingJobs, j)
	}
	back, err := l.TakenBack(now.Add(-time.Hour))
	if err != nil {
		return out, err
	}
	for _, d := range back {
		j := wait(d)
		j.WaitedSeconds, j.LeftAt = int64(d.ClosedAt.Sub(d.FirstSeen).Seconds()), d.ClosedAt
		out.TakenBack = append(out.TakenBack, j)
	}
	closed, err := l.ClosedIncidents(now.Add(-24 * time.Hour))
	if err != nil {
		return out, err
	}
	out.RecentIncidents = append(out.RecentIncidents, closed...)
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

// hostFree is what a new runner could take on h as placement counts it, and
// how many times it counts the cores (0 = once).
func hostFree(cfg *config.Config, h decide.Host, cpus float64, mem int64) (float64, int64, float64) {
	s := decide.Snapshot{CPUOvercommit: cfg.CPUOvercommit, PSICPUThreshold: cfg.PSICPUThreshold}
	cpu, m := s.HostFree(h, cpus, float64(mem))
	share := s.CPUShare(h)
	if share == 1 {
		share = 0
	}
	return cpu, int64(m), share
}

func classStates(cfg *config.Config) []ClassState {
	out := []ClassState{}
	for _, cl := range cfg.Classes {
		os, driver, _ := strings.Cut(cl.Platform(), "/")
		out = append(out, ClassState{Name: cl.Name, Labels: cl.RunsOn(), OS: os, Driver: driver, Docker: cl.Docker,
			Hosts: cl.Hosts, Mounts: cl.Mounts, Overflow: cl.Overflow, After: cl.After, Reserve: cl.Reserve,
			CPUs: cmp.Or(cl.CPUs, config.DefaultCPUs), Memory: cmp.Or(int64(cl.Memory), config.DefaultMemory),
			LimitCPUs: cl.Limit.CPUs, LimitMem: int64(cl.Limit.Memory), MaxDurationSeconds: int64(cl.MaxDuration.Seconds()), WarmMin: cl.WarmMin})
	}
	return out
}

// aheadRe reads a start decision's reason for a runner started ahead of a
// job (decide's warmStarts): "ahead: <job> expected in ~Ns, when <job> ends
// (1 of 2 expected), holds …".
var aheadRe = regexp.MustCompile(`^ahead: (.*) \(\d+ of \d+ expected\)`)

// aheadOf is the job a runner was started ahead of, and after what, from
// its start decision's reason ("" = it was not).
func aheadOf(reason string) string {
	if m := aheadRe.FindStringSubmatch(reason); m != nil {
		return m[1]
	}
	return ""
}
