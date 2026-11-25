// Package agent is the per-host side of pumasi: it starts and removes runner
// containers, samples their cgroups and reports host capacity over HTTP.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/cgroup"
)

type Config struct {
	Host       string
	Kind       string  // server | desktop, when the host itself says ("" = the controller knows)
	Version    string  // this build, as shown to people
	BudgetCPUs float64 // 0 = no limit from the agent side
	BudgetMem  int64
	Interval   time.Duration
	StateFile  string // runner records survive an agent restart here ("" = keep nothing)
	BootID     string // /proc/sys/kernel/random/boot_id; a change means the host rebooted
}

type Agent struct {
	cfg Config
	drv Driver
	cg  cgroup.Reader
	log *slog.Logger
	now func() time.Time

	// opMu serializes sampling with start/delete so a sample cannot resurrect a
	// runner that was just deleted. dataMu guards the fields below for readers.
	opMu   sync.Mutex
	dataMu sync.Mutex
	recs   map[string]*rec
	host   hostState

	prevRootUsec uint64
	prevAt       time.Time
	dataDir      string // docker's data directory, asked once
}

type rec struct {
	api.Runner
	receivedAt time.Time
	cgDir      string
	lastUsec   uint64
	logsSince  time.Time // container output before this was already searched
	rebooted   bool      // loaded from a state file written before the last reboot
}

type hostState struct {
	cpus         float64
	memBytes     int64
	psi          map[string]float64
	nonRunnerCPU float64
	nonRunnerMem int64
	sampledAt    time.Time
	diskFree     int64
	diskTotal    int64
}

func New(cfg Config, drv Driver, cg cgroup.Reader, log *slog.Logger) *Agent {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	a := &Agent{cfg: cfg, drv: drv, cg: cg, log: log, now: time.Now, recs: map[string]*rec{}}
	a.load()
	return a
}

// Run samples every Interval until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	for {
		a.Sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sample refreshes containers, their cgroup counters and host state.
func (a *Agent) Sample(ctx context.Context) {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	cs, err := a.drv.List(ctx)
	if err != nil {
		a.log.Error("list containers", "err", err)
		return
	}
	now := a.now()
	seen := map[string]bool{}
	var runnerUsecDelta uint64
	var runnerMem int64
	for _, c := range cs {
		seen[c.Name] = true
		a.dataMu.Lock()
		r, ok := a.recs[c.Name]
		if !ok {
			// A container from before this agent started.
			r = &rec{Runner: api.Runner{Name: c.Name}, receivedAt: c.StartedAt}
			a.recs[c.Name] = r
		}
		a.dataMu.Unlock()

		var st *cgroup.Stats
		if c.State == "running" && c.Pid > 0 {
			if r.cgDir == "" {
				r.cgDir, err = a.cg.PathOfPid(c.Pid)
				if err != nil {
					a.log.Warn("cgroup path", "runner", c.Name, "err", err)
				}
			}
			if r.cgDir != "" {
				s, err := a.cg.Read(r.cgDir)
				if err != nil {
					a.log.Warn("cgroup read", "runner", c.Name, "err", err)
				} else {
					st = &s
					runnerUsecDelta += s.CPUUsec - min(r.lastUsec, s.CPUUsec)
					runnerMem += int64(s.MemCurrent)
				}
			}
		}
		var ev LogEvents
		if c.State == "running" && (r.ListeningAt == nil || r.JobStartedAt == nil) {
			checked := a.now().Add(-time.Second) // overlap: docker log timestamps vs. our clock
			if ev, err = a.drv.LogEvents(ctx, c.Name, r.logsSince); err != nil {
				a.log.Warn("runner logs", "runner", c.Name, "err", err)
			} else {
				r.logsSince = checked
			}
		}

		a.dataMu.Lock()
		r.Class, r.CPUs, r.MemBytes, r.State = c.Class, c.CPUs, c.MemBytes, c.State
		if !c.StartedAt.IsZero() {
			r.StartedAt = ptr(c.StartedAt)
		}
		if c.State == "exited" {
			r.ExitedAt = ptr(c.FinishedAt)
			r.ExitCode = ptr(c.ExitCode)
			r.MemCurrent = 0
			if c.OOMKilled && r.OOMKills == 0 {
				r.OOMKills = 1 // the cgroup is gone with the container; docker still knows
			}
			if r.rebooted && c.ExitCode != 0 {
				r.Lost = lostRebooted
			}
		}
		r.rebooted = false // either it survived (not a real reboot of its world) or it is marked now
		if st != nil {
			applyStats(r, *st)
		}
		if !ev.Listening.IsZero() && r.ListeningAt == nil {
			r.ListeningAt = ptr(ev.Listening)
			if !r.receivedAt.IsZero() {
				r.ProvisionMs = ev.Listening.Sub(r.receivedAt).Milliseconds()
			}
		}
		if !ev.JobStarted.IsZero() && r.JobStartedAt == nil {
			r.JobStartedAt = ptr(ev.JobStarted)
		}
		a.dataMu.Unlock()
	}

	a.dataMu.Lock()
	for name, r := range a.recs {
		// Gone from docker without DELETE (removed by hand). Keep runners that are
		// still being started: they are not listed until `docker run` creates them.
		if seen[name] || r.State == "starting" {
			continue
		}
		if r.rebooted || r.Lost != "" {
			// The reboot took the container; keep the record until the controller collects it.
			r.rebooted, r.Lost, r.State, r.MemCurrent = false, lostRebooted, "gone", 0
			continue
		}
		delete(a.recs, name)
	}
	a.dataMu.Unlock()
	a.save()

	a.sampleHost(ctx, now, runnerUsecDelta, runnerMem)
}

func applyStats(r *rec, s cgroup.Stats) {
	r.lastUsec = s.CPUUsec
	r.CPUSec = float64(s.CPUUsec) / 1e6
	r.MemCurrent = int64(s.MemCurrent)
	r.MemPeak = max(r.MemPeak, int64(s.MemPeak))
	r.IOBytes = int64(s.IOBytes)
	r.OOMKills = max(r.OOMKills, int(s.OOMKills))
	r.PressureSec = map[string]float64{}
	for k, v := range s.Pressure {
		r.PressureSec[k] = float64(v) / 1e6
	}
}

func (a *Agent) sampleHost(ctx context.Context, now time.Time, runnerUsecDelta uint64, runnerMem int64) {
	h := hostState{cpus: float64(runtime.NumCPU()), sampledAt: now}
	if a.dataDir == "" {
		a.dataDir = a.drv.DataDir(ctx)
	}
	if a.dataDir != "" {
		h.diskFree, h.diskTotal = diskSpace(a.dataDir)
	}
	if psi, err := a.cg.HostPressure(); err == nil {
		h.psi = psi
	} else {
		a.log.Warn("host pressure", "err", err)
	}
	if mi, err := a.cg.Meminfo(); err == nil {
		h.memBytes = int64(mi.Total)
		h.nonRunnerMem = max(0, int64(mi.Total)-int64(mi.Available)-runnerMem)
	} else {
		a.log.Warn("meminfo", "err", err)
	}
	root, err := a.cg.RootCPUUsec()
	if err != nil {
		a.log.Warn("root cpu.stat", "err", err)
	}

	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	h.nonRunnerCPU = a.host.nonRunnerCPU
	if err == nil && a.prevRootUsec > 0 && root > a.prevRootUsec {
		dt := now.Sub(a.prevAt).Seconds()
		other := float64(root-a.prevRootUsec) - float64(runnerUsecDelta)
		h.nonRunnerCPU = max(0, other/1e6/dt)
	}
	a.prevRootUsec, a.prevAt = root, now
	a.host = h
}

// diskSpace is the free and total bytes of the filesystem holding dir (0, 0 = unknown).
func diskSpace(dir string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}

func ptr[T any](v T) *T { return &v }

// Snapshot returns the last sampled state.
func (a *Agent) Snapshot() api.Snapshot {
	a.dataMu.Lock()
	defer a.dataMu.Unlock()
	s := api.Snapshot{
		Host: a.cfg.Host, Kind: a.cfg.Kind, Version: a.cfg.Version, CPUs: a.host.cpus, MemBytes: a.host.memBytes,
		BudgetCPUs: a.cfg.BudgetCPUs, BudgetMem: a.cfg.BudgetMem,
		PSI: a.host.psi, NonRunnerCPU: a.host.nonRunnerCPU, NonRunnerMem: a.host.nonRunnerMem,
		SampledAt: a.host.sampledAt, DiskFree: a.host.diskFree, DiskTotal: a.host.diskTotal, Runners: []api.Runner{},
	}
	for _, r := range a.recs {
		s.Runners = append(s.Runners, r.Runner)
	}
	sort.Slice(s.Runners, func(i, j int) bool { return s.Runners[i].Name < s.Runners[j].Name })
	return s
}

var errOverBudget = errors.New("over budget")

const lostRebooted = "host rebooted"

// Start launches one runner. It refuses when the agent's own budget would be exceeded.
func (a *Agent) Start(ctx context.Context, req api.StartRequest) (api.Runner, error) {
	received := a.now() // before waiting for other starts: queueing here is part of provisioning
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.dataMu.Lock()
	var cpus float64
	var mem int64
	for _, r := range a.recs {
		if r.State != "exited" && r.State != "gone" {
			cpus += r.CPUs
			mem += r.MemBytes
		}
	}
	if _, dup := a.recs[req.Name]; dup {
		a.dataMu.Unlock()
		return api.Runner{}, fmt.Errorf("runner %s exists", req.Name)
	}
	if (a.cfg.BudgetCPUs > 0 && cpus+req.CPUs > a.cfg.BudgetCPUs+1e-9) || (a.cfg.BudgetMem > 0 && mem+req.MemBytes > a.cfg.BudgetMem) {
		a.dataMu.Unlock()
		return api.Runner{}, fmt.Errorf("%w: in use %.1f cpus/%d bytes, budget %.1f/%d, asked %.1f/%d",
			errOverBudget, cpus, mem, a.cfg.BudgetCPUs, a.cfg.BudgetMem, req.CPUs, req.MemBytes)
	}
	r := &rec{Runner: api.Runner{Name: req.Name, Class: req.Class, CPUs: req.CPUs, MemBytes: req.MemBytes, State: "starting"}, receivedAt: received}
	a.recs[req.Name] = r
	a.dataMu.Unlock()

	err := a.drv.Start(ctx, Spec{Name: req.Name, Class: req.Class, CPUs: req.CPUs, MemBytes: req.MemBytes, Image: req.Image, JITConfig: req.JITConfig})

	a.dataMu.Lock()
	if err != nil {
		delete(a.recs, req.Name)
		a.dataMu.Unlock()
		return api.Runner{}, err
	}
	r.State = "created"
	out := r.Runner
	a.dataMu.Unlock()
	a.save()
	return out, nil
}

// Delete removes a runner container and returns its last values. A running
// container is sampled once more first.
func (a *Agent) Delete(ctx context.Context, name string) (api.Runner, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.dataMu.Lock()
	r, ok := a.recs[name]
	a.dataMu.Unlock()
	if ok && r.State == "running" && r.cgDir != "" {
		if st, err := a.cg.Read(r.cgDir); err == nil {
			a.dataMu.Lock()
			applyStats(r, st)
			a.dataMu.Unlock()
		}
	}
	tail := a.drv.LogTail(ctx, name, 40)
	if err := a.drv.Remove(ctx, name); err != nil {
		return api.Runner{}, err
	}
	a.dataMu.Lock()
	delete(a.recs, name)
	a.dataMu.Unlock()
	a.save()
	if !ok {
		return api.Runner{Name: name, State: "unknown"}, nil
	}
	out := r.Runner
	out.LogTail = tail
	return out, nil
}

// Handler serves the agent API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Snapshot())
	})
	mux.HandleFunc("POST /v1/runners", func(w http.ResponseWriter, r *http.Request) {
		var req api.StartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Image == "" || req.JITConfig == "" || req.CPUs <= 0 || req.MemBytes <= 0 {
			http.Error(w, "name, image, jitConfig, cpus and memBytes are required", http.StatusBadRequest)
			return
		}
		out, err := a.Start(r.Context(), req)
		switch {
		case errors.Is(err, errOverBudget):
			http.Error(w, err.Error(), http.StatusConflict)
		case err != nil:
			a.log.Error("start runner", "runner", req.Name, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			a.log.Info("runner started", "runner", req.Name, "class", req.Class)
			writeJSON(w, http.StatusCreated, out)
		}
	})
	mux.HandleFunc("DELETE /v1/runners/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		out, err := a.Delete(r.Context(), name)
		if err != nil {
			a.log.Error("delete runner", "runner", name, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.log.Info("runner deleted", "runner", name, "cpuSec", out.CPUSec, "memPeak", out.MemPeak)
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/check", func(w http.ResponseWriter, r *http.Request) {
		problem := ""
		if err := a.drv.Check(r.Context(), r.URL.Query().Get("image")); err != nil {
			problem = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]string{"problem": problem})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		WriteMetrics(w, a.Snapshot())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
