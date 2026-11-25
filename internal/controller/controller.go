// Package controller runs one listener per class (scale set), keeps the ledger,
// and periodically turns agent snapshots and waiting jobs into a Decide plan.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

type Controller struct {
	cfg    *config.Config
	gh     *scaleset.Client
	token  string
	ledger *ledger.Ledger
	log    *slog.Logger
	// ListenerLog receives the scale set listener's own lines (one per long poll).
	ListenerLog *slog.Logger
	ev          *EventLog
	now         func() time.Time

	hosts   []*host
	classes []*class
	kick    chan struct{}

	mu        sync.Mutex      // guards class.stats and inflight
	inflight  map[string]bool // runners whose start request has not returned yet
	starts    sync.WaitGroup
	lastDefer map[string]deferRec
	runCheck  map[int64]time.Time
	status    Status // last round, for `pumasi status`
}

type host struct {
	cfg       config.Host
	started   bool // this controller started its agent
	tr        Transport
	agent     *api.Client
	snap      *api.Snapshot
	downSince time.Time
}

type class struct {
	cfg   config.Class
	setID int
	stats *scaleset.RunnerScaleSetStatistic
}

type deferRec struct {
	count int
	at    time.Time
}

func New(cfg *config.Config, token string, l *ledger.Ledger, ev *EventLog, log *slog.Logger) (*Controller, error) {
	gh, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     cfg.URL,
		PersonalAccessToken: token,
		SystemInfo:          scaleset.SystemInfo{System: "pumasi", Subsystem: "controller", CommitSHA: "NA", Version: "0.1.0"},
	})
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	c := &Controller{cfg: cfg, gh: gh, token: token, ledger: l, log: log, ListenerLog: log, ev: ev, now: time.Now,
		kick: make(chan struct{}, 1), inflight: map[string]bool{}, lastDefer: map[string]deferRec{}, runCheck: map[int64]time.Time{}}
	for _, hc := range cfg.Hosts {
		tr := &SSHTunnel{Dest: hc.SSH, RemotePort: hc.AgentPort, Log: log}
		c.hosts = append(c.hosts, &host{cfg: hc, tr: tr,
			agent: &api.Client{BaseURL: tr.BaseURL, HTTP: &http.Client{Timeout: 90 * time.Second}}})
	}
	for _, cc := range cfg.Classes {
		c.classes = append(c.classes, &class{cfg: cc})
	}
	return c, nil
}

func (c *Controller) Close() {
	for _, h := range c.hosts {
		h.tr.Close()
	}
}

// EnsureScaleSets finds or creates the scale set of every class.
func (c *Controller) EnsureScaleSets(ctx context.Context) error {
	for _, cl := range c.classes {
		set, err := c.gh.GetRunnerScaleSet(ctx, c.cfg.RunnerGroupID, cl.cfg.ScaleSet)
		if err != nil || set == nil {
			labels := make([]scaleset.Label, 0, len(cl.cfg.Labels))
			for _, n := range cl.cfg.Labels {
				labels = append(labels, scaleset.Label{Name: n})
			}
			set, err = c.gh.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
				Name: cl.cfg.ScaleSet, RunnerGroupID: c.cfg.RunnerGroupID, Labels: labels,
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
			})
			if err != nil {
				return fmt.Errorf("create scale set %s: %w", cl.cfg.ScaleSet, err)
			}
			c.ev.Write("scale-set-created", set)
		}
		cl.setID = set.ID
		c.log.Info(fmt.Sprintf("class %s  runs-on: [%s]", cl.cfg.Name, strings.Join(cl.cfg.Labels, ", ")))
	}
	return nil
}

// Run starts the agents, listens on every class and re-evaluates until ctx
// ends. Then it removes its runners and stops the agents; scale sets stay.
// distDir holds the linux builds of this binary (pumasi-linux-<arch>).
func (c *Controller) Run(ctx context.Context, distDir string) error {
	if err := c.EnsureScaleSets(ctx); err != nil {
		return err
	}
	defer c.shutdown()
	if err := c.startAgents(ctx, distDir); err != nil {
		return err
	}
	closeStatus, err := c.serveStatus()
	if err != nil {
		return err
	}
	defer closeStatus()
	c.log.Info("listening for jobs (Ctrl-C to stop)")
	var wg sync.WaitGroup
	for _, cl := range c.classes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.listen(ctx, cl)
		}()
	}
	t := time.NewTicker(c.cfg.EvaluateEvery)
	defer t.Stop()
	for {
		c.Evaluate(ctx)
		select {
		case <-ctx.Done():
			wg.Wait()
			c.starts.Wait()
			return nil
		case <-t.C:
		case <-c.kick:
		}
	}
}

// listen keeps a message session and listener alive for one class.
func (c *Controller) listen(ctx context.Context, cl *class) {
	hostname, _ := os.Hostname()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		err := func() error {
			session, err := c.gh.MessageSessionClient(ctx, cl.setID, "pumasi-"+hostname)
			if err != nil {
				return fmt.Errorf("session: %w", err)
			}
			defer session.Close(context.WithoutCancel(ctx))
			l, err := listener.New(session, listener.Config{ScaleSetID: cl.setID, MaxRunners: cl.cfg.MaxRunners,
				Logger: c.ListenerLog.With("class", cl.cfg.Name)})
			if err != nil {
				return err
			}
			return l.Run(ctx, &scaler{c: c, cl: cl, session: session})
		}()
		if ctx.Err() != nil {
			return
		}
		c.log.Error("listener stopped, restarting", "class", cl.cfg.Name, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Minute)
	}
}

type scaler struct {
	c       *Controller
	cl      *class
	session *scaleset.MessageSessionClient
}

// Scale records demand and job events in the ledger and asks for a re-evaluation.
// Starting runners happens in Evaluate, not here, so deferred demand is served
// even when GitHub has stopped sending messages.
func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	c, cls := s.c, s.cl.cfg.Name
	c.ev.Write("message", map[string]any{"class": cls, "msg": msg})
	now := c.now()

	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquire: %w", err)
		}
		for _, j := range msg.JobAvailableMessages {
			if err := c.ledger.UpsertDemand(job(cls, j.JobMessageBase), now); err != nil {
				return err
			}
		}
	}
	// Withdrawn assignments first: GitHub puts the cancel and the reassignment
	// (under a new job id) in the same message.
	for _, j := range msg.JobCompletedMessages {
		if j.RunnerName != "" {
			continue
		}
		// No runner took it within 5 minutes: GitHub cancels this assignment and, up to
		// two more times, assigns the job again under a new job id (M0, M1). A job canceled
		// by a person looks the same and is simply not assigned again.
		c.log.Info(fmt.Sprintf("unassigned %s  %s: no runner took it, GitHub withdrew the assignment (%s)", cls, j.JobDisplayName, j.Result))
		if err := c.ledger.CloseDemand(j.JobID, "unassigned-"+j.Result, orNow(j.FinishTime, now)); err != nil {
			return err
		}
	}
	for _, j := range msg.JobAssignedMessages {
		if err := c.ledger.UpsertDemand(job(cls, j.JobMessageBase), now); err != nil {
			return err
		}
	}
	for _, j := range msg.JobStartedMessages {
		// JobStarted arrives 30-50s late (the long poll does not wake for it); keep GitHub's time.
		ours, err := c.ledger.JobStarted(j.RunnerName, job(cls, j.JobMessageBase), orNow(j.RunnerAssignTime, now))
		if err != nil {
			return err
		}
		if !ours {
			c.log.Warn("job started on a runner not in the ledger", "class", cls, "runner", j.RunnerName)
		}
		c.log.Info(fmt.Sprintf("job %s on %s", j.JobDisplayName, j.RunnerName))
	}
	for _, j := range msg.JobCompletedMessages {
		if j.RunnerName == "" {
			continue
		}
		if err := c.ledger.JobCompleted(j.RunnerName, job(cls, j.JobMessageBase), j.Result, orNow(j.FinishTime, now)); err != nil {
			return err
		}
		c.log.Info(fmt.Sprintf("done %s on %s  %s", j.JobDisplayName, j.RunnerName, j.Result))
	}
	if msg.Statistics != nil {
		c.mu.Lock()
		s.cl.stats = msg.Statistics
		c.mu.Unlock()
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return nil
}

func job(class string, j scaleset.JobMessageBase) ledger.Job {
	queued := j.QueueTime
	if queued.Year() < 2000 { // github.com leaves queueTime empty; scaleSetAssignTime is the closest
		queued = j.ScaleSetAssignTime
	}
	return ledger.Job{
		ID: j.JobID, Class: class, Repo: j.OwnerName + "/" + j.RepositoryName,
		Workflow: j.JobWorkflowRef, Name: j.JobDisplayName, RunID: j.WorkflowRunID,
		Labels: strings.Join(j.RequestLabels, ","), QueueTime: queued,
	}
}

func orNow(t, now time.Time) time.Time {
	if t.Year() < 2000 {
		return now
	}
	return t
}

// Evaluate is one reconciliation round: read agents, update the ledger, decide, act.
func (c *Controller) Evaluate(ctx context.Context) {
	now := c.now()
	c.refreshHosts(ctx, now)

	live, err := c.ledger.LiveRunners()
	if err != nil {
		c.log.Error("ledger runners", "err", err)
		return
	}
	snap := c.decideSnapshot(now, c.syncRunners(live))
	plan := decide.Decide(snap)

	for _, st := range plan.Stop {
		c.stop(ctx, st, now)
	}
	// Starts run in the background: a slow `docker run` on one host must not hold
	// up placement elsewhere. The ledger row exists before Evaluate returns.
	for _, s := range plan.Start {
		c.start(ctx, s, now)
	}
	c.recordDefers(plan.Defer, now)
	c.setStatus(plan.Defer)
	c.unregister(ctx)
	c.checkStaleDemand(ctx, now)
}

func (c *Controller) refreshHosts(ctx context.Context, now time.Time) {
	var wg sync.WaitGroup
	for _, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			s, err := h.agent.Snapshot(cctx)
			if err != nil {
				if h.downSince.IsZero() {
					h.downSince = now
				}
				h.snap = nil
				if h.downSince.Equal(now) {
					c.log.Warn(fmt.Sprintf("host %s unreachable", h.cfg.Name), "err", err)
				}
				return
			}
			if !h.downSince.IsZero() {
				c.log.Info(fmt.Sprintf("host %s reachable again", h.cfg.Name))
			}
			h.snap, h.downSince = &s, time.Time{}
		}()
	}
	wg.Wait()
}

// syncRunners copies agent observations into the ledger and joins both views.
func (c *Controller) syncRunners(live []ledger.Runner) []decide.Runner {
	byName := map[string]ledger.Runner{}
	for _, r := range live {
		byName[r.Name] = r
	}
	seen := map[string]bool{}
	var out []decide.Runner
	for _, h := range c.hosts {
		if h.snap == nil {
			continue
		}
		for _, ar := range h.snap.Runners {
			seen[ar.Name] = true
			lr, ok := byName[ar.Name]
			if ok {
				c.recordUsage(ar)
				if ar.ListeningAt != nil && lr.ListeningAt.IsZero() {
					ms := ar.ProvisionMs
					if !lr.PostedAt.IsZero() {
						ms += lr.PostedAt.Sub(lr.CreatedAt).Milliseconds()
					}
					if err := c.ledger.MarkListening(ar.Name, *ar.ListeningAt, ms); err != nil {
						c.log.Error("ledger listening", "err", err)
					}
					lr.ListeningAt = *ar.ListeningAt
					c.log.Info(fmt.Sprintf("ready %s  %.1fs", ar.Name, float64(ms)/1000))
					if lr.State == "starting" {
						lr.State = "idle"
					}
				}
				if ar.JobStartedAt != nil && (lr.State == "starting" || lr.State == "idle") {
					if err := c.ledger.MarkBusy(ar.Name, *ar.JobStartedAt); err != nil {
						c.log.Error("ledger busy", "err", err)
					}
					lr.State = "busy"
					// Seen in the runner's own output; GitHub's JobStarted message comes later.
					c.log.Info(fmt.Sprintf("busy %s", ar.Name))
					c.ev.Write("runner-busy", map[string]any{"runner": ar.Name, "runningJobAt": ar.JobStartedAt})
				}
			}
			dr := decide.Runner{Name: ar.Name, Class: ar.Class, Host: h.cfg.Name, CPUs: ar.CPUs, MemBytes: ar.MemBytes,
				State: lr.State, JobKnown: lr.JobID != "", CreatedAt: lr.CreatedAt, ListeningAt: lr.ListeningAt, Container: ar.State}
			if ar.ExitCode != nil {
				dr.ExitCode = *ar.ExitCode
			}
			out = append(out, dr)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, lr := range live {
		if !seen[lr.Name] {
			dr := decide.Runner{Name: lr.Name, Class: lr.Class, Host: lr.Host, State: lr.State, JobKnown: lr.JobID != "",
				CreatedAt: lr.CreatedAt, ListeningAt: lr.ListeningAt}
			if c.inflight[lr.Name] {
				dr.Container = "starting" // not at the agent yet
			}
			out = append(out, dr)
		}
	}
	return out
}

func (c *Controller) recordUsage(r api.Runner) {
	u := ledger.Usage{CPUSec: r.CPUSec, MemPeak: r.MemPeak, IOBytes: r.IOBytes, ExitCode: r.ExitCode,
		PSICPUSec: r.PressureSec["cpu"], PSIMemSec: r.PressureSec["memory"], PSIIOSec: r.PressureSec["io"]}
	if err := c.ledger.SetUsage(r.Name, u); err != nil {
		c.log.Error("ledger usage", "runner", r.Name, "err", err)
	}
}

func (c *Controller) decideSnapshot(now time.Time, runners []decide.Runner) decide.Snapshot {
	s := decide.Snapshot{Now: now, Runners: runners, PSICPUThreshold: c.cfg.PSICPUThreshold,
		ProvisionTimeout: c.cfg.ProvisionTimeout, IdleTimeout: c.cfg.IdleTimeout, HostLostAfter: c.cfg.HostLostAfter}
	for _, h := range c.hosts {
		dh := decide.Host{Name: h.cfg.Name, Kind: h.cfg.Kind, Up: h.snap != nil,
			BudgetCPUs: h.cfg.Budget.CPUs, BudgetMem: int64(h.cfg.Budget.Memory)}
		if h.snap == nil {
			dh.DownFor = now.Sub(h.downSince)
		} else {
			a := h.snap
			dh.CPUs, dh.MemBytes, dh.NonRunnerCPU, dh.NonRunnerMem, dh.PSICPU = a.CPUs, a.MemBytes, a.NonRunnerCPU, a.NonRunnerMem, a.PSI["cpu"]
			dh.BudgetCPUs = minPositive(dh.BudgetCPUs, a.BudgetCPUs)
			dh.BudgetMem = int64(minPositive(float64(dh.BudgetMem), float64(a.BudgetMem)))
		}
		s.Hosts = append(s.Hosts, dh)
	}

	open, err := c.ledger.OpenDemand()
	if err != nil {
		c.log.Error("ledger demand", "err", err)
	}
	perClass := map[string]int{}
	for _, d := range open {
		s.Demand = append(s.Demand, decide.Demand{ID: d.JobID, Class: d.Class, FirstSeen: d.FirstSeen})
		perClass[d.Class]++
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cl := range c.classes {
		s.Classes = append(s.Classes, decide.Class{Name: cl.cfg.Name, CPUs: cl.cfg.CPUs, MemBytes: int64(cl.cfg.Memory),
			MaxRunners: cl.cfg.MaxRunners, Hosts: cl.cfg.Hosts})
		// Jobs the statistics count but the ledger never saw (for example after a fresh ledger).
		if cl.stats != nil {
			for i := perClass[cl.cfg.Name]; i < cl.stats.TotalAssignedJobs-cl.stats.TotalRunningJobs; i++ {
				s.Demand = append(s.Demand, decide.Demand{ID: fmt.Sprintf("stats-%d", i), Class: cl.cfg.Name, FirstSeen: now})
			}
		}
	}
	return s
}

func minPositive(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return math.Min(a, b)
}

func (c *Controller) hostByName(name string) *host {
	for _, h := range c.hosts {
		if h.cfg.Name == name {
			return h
		}
	}
	return nil
}

// stop removes the container (when the agent answers), stores its final values and the new state.
func (c *Controller) stop(ctx context.Context, st decide.Stop, now time.Time) {
	c.log.Info(fmt.Sprintf("stop %s  %s", st.Runner, st.Reason))
	c.decision(now, "", st.Host, "stop", st.Runner, st.Reason)
	removed := false
	if h := c.hostByName(st.Host); h != nil && h.snap != nil {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		final, err := h.agent.Delete(cctx, st.Runner)
		cancel()
		if err != nil {
			c.log.Error("delete runner", "runner", st.Runner, "err", err)
		} else {
			removed = true
			if final.State != "unknown" {
				c.recordUsage(final)
			}
		}
	}
	if err := c.ledger.SetState(st.Runner, st.State, st.Reason); err != nil {
		c.log.Error("ledger state", "err", err)
	}
	if removed {
		if err := c.ledger.MarkRemoved(st.Runner, now); err != nil {
			c.log.Error("ledger removed", "err", err)
		}
	}
}

// start records the runner and launches it in the background.
func (c *Controller) start(ctx context.Context, s decide.Start, now time.Time) {
	name := fmt.Sprintf("%s-%s-%s", s.Class, s.Host, uuid.NewString()[:8])
	c.log.Info(fmt.Sprintf("start %s on %s  %s  (%s)", s.Class, s.Host, s.Reason, name))
	c.decision(now, s.Class, s.Host, "start", name, s.Reason)
	if err := c.ledger.InsertRunner(name, s.Class, s.Host, now); err != nil {
		c.log.Error("ledger insert", "err", err)
		return
	}
	c.mu.Lock()
	c.inflight[name] = true
	c.mu.Unlock()
	c.starts.Add(1)
	go func() {
		defer c.starts.Done()
		c.launch(ctx, name, s, now)
		c.mu.Lock()
		delete(c.inflight, name)
		c.mu.Unlock()
	}()
}

func (c *Controller) launch(ctx context.Context, name string, s decide.Start, now time.Time) {
	cl := c.classByName(s.Class)
	h := c.hostByName(s.Host)
	fail := func(reason string) {
		c.log.Error("start failed", "runner", name, "reason", reason)
		_ = c.ledger.SetState(name, "failed", reason)
		_ = c.ledger.MarkRemoved(name, c.now())
	}
	jit, err := c.gh.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, cl.setID)
	if err != nil {
		fail("jit: " + err.Error())
		return
	}
	posted := c.now()
	if err := c.ledger.SetRegistration(name, int64(jit.Runner.ID), posted); err != nil {
		c.log.Error("ledger registration", "err", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, err = h.agent.Start(cctx, api.StartRequest{Name: name, Class: s.Class, CPUs: cl.cfg.CPUs,
		MemBytes: int64(cl.cfg.Memory), Image: c.cfg.Image, JITConfig: jit.EncodedJITConfig})
	if err != nil {
		// The registration stays until unregister removes it.
		fail("agent start: " + err.Error())
		return
	}
	c.ev.Write("runner-started", map[string]any{"runner": name, "host": s.Host, "class": s.Class,
		"jitMs": posted.Sub(now).Milliseconds(), "postMs": c.now().Sub(posted).Milliseconds()})
}

func (c *Controller) classByName(name string) *class {
	for _, cl := range c.classes {
		if cl.cfg.Name == name {
			return cl
		}
	}
	return nil
}

// recordDefers writes a defer when its count changes, or once a minute while it lasts.
func (c *Controller) recordDefers(ds []decide.Defer, now time.Time) {
	active := map[string]bool{}
	for _, d := range ds {
		active[d.Class] = true
		last, ok := c.lastDefer[d.Class]
		if ok && last.count == d.Count && now.Sub(last.at) < time.Minute {
			continue
		}
		c.lastDefer[d.Class] = deferRec{count: d.Count, at: now}
		c.log.Info(fmt.Sprintf("defer %s x%d  %s", d.Class, d.Count, d.Reason))
		c.decision(now, d.Class, "", "defer", "", fmt.Sprintf("%d waiting: %s", d.Count, d.Reason))
	}
	for cls := range c.lastDefer {
		if !active[cls] {
			delete(c.lastDefer, cls)
		}
	}
}

func (c *Controller) decision(at time.Time, class, host, action, runner, reason string) {
	if err := c.ledger.AddDecision(at, class, host, action, runner, reason); err != nil {
		c.log.Error("ledger decision", "err", err)
	}
	c.ev.Write("decision", map[string]any{"class": class, "host": host, "action": action, "runner": runner, "reason": reason})
}

// unregister removes GitHub registrations of runners that are gone. A runner
// that ended normally has removed itself already (RunnerNotFound).
func (c *Controller) unregister(ctx context.Context) {
	rs, err := c.ledger.PendingUnregister()
	if err != nil {
		c.log.Error("ledger pending", "err", err)
		return
	}
	for _, r := range rs {
		err := c.gh.RemoveRunner(ctx, r.RunnerID)
		switch {
		case err == nil:
			c.log.Debug("removed registration", "runner", r.Name, "id", r.RunnerID)
			c.ev.Write("unregistered", map[string]any{"runner": r.Name, "id": r.RunnerID})
		case errors.Is(err, scaleset.RunnerNotFoundError):
		case errors.Is(err, scaleset.JobStillRunningError):
			c.log.Debug("registration busy, retry later", "runner", r.Name)
			continue
		default:
			c.log.Warn("remove registration", "runner", r.Name, "err", err)
			continue
		}
		if err := c.ledger.MarkUnregistered(r.Name); err != nil {
			c.log.Error("ledger unregistered", "err", err)
		}
	}
}

// checkStaleDemand closes demand whose workflow run has finished. After three
// reassignments GitHub sends nothing more about a waiting job (M0), so a job
// canceled while waiting would otherwise stay open until demandTTL.
func (c *Controller) checkStaleDemand(ctx context.Context, now time.Time) {
	open, err := c.ledger.OpenDemand()
	if err != nil {
		return
	}
	for _, d := range open {
		if now.Sub(d.FirstSeen) > c.cfg.DemandTTL {
			_ = c.ledger.CloseDemand(d.JobID, "ttl", now)
			continue
		}
		if d.RunID == 0 || now.Sub(d.LastSeen) < 2*time.Minute || now.Sub(c.runCheck[d.RunID]) < 2*time.Minute {
			continue
		}
		c.runCheck[d.RunID] = now
		status, conclusion, err := c.runStatus(ctx, d.Repo, d.RunID)
		if err != nil {
			c.log.Warn("run status", "run", d.RunID, "err", err)
			continue
		}
		if status == "completed" {
			c.log.Info("closing demand of a finished run", "job", d.JobID, "run", d.RunID, "conclusion", conclusion)
			_ = c.ledger.CloseDemand(d.JobID, "run-"+conclusion, now)
		}
	}
}

func (c *Controller) runStatus(ctx context.Context, repo string, runID int64) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d", repo, runID), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET run: %s", resp.Status)
	}
	var run struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	err = json.NewDecoder(resp.Body).Decode(&run)
	return run.Status, run.Conclusion, err
}

// startAgents puts this build's agent on every host, in parallel. A host that
// fails is reported and left out; the run goes on while one host works.
func (c *Controller) startAgents(ctx context.Context, distDir string) error {
	errs := make([]error, len(c.hosts))
	var wg sync.WaitGroup
	for i, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			did, err := ensureAgent(cctx, h.cfg, distDir)
			if err != nil {
				errs[i] = err
				c.log.Error(fmt.Sprintf("host %s not usable", h.cfg.Name), "err", err)
				return
			}
			h.started = true
			c.log.Info(fmt.Sprintf("host %s  %s  budget %gcpu/%s", h.cfg.Name, did, h.cfg.Budget.CPUs, gib(int64(h.cfg.Budget.Memory))))
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := errors.Join(errs...); err != nil && !slices.ContainsFunc(c.hosts, func(h *host) bool { return h.started }) {
		return errors.New("no usable host")
	}
	return nil
}

func gib(b int64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }

// shutdown runs after Ctrl-C, on its own deadline: remove our runners, stop the
// agents we started, close the tunnels.
func (c *Controller) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.log.Info("stopping: removing runners and agents (scale sets stay; `pumasi down` deletes them)")
	c.refreshHosts(ctx, c.now())
	n := 0
	for _, h := range c.hosts {
		if h.snap == nil {
			continue
		}
		for _, r := range h.snap.Runners {
			if _, err := h.agent.Delete(ctx, r.Name); err != nil {
				c.log.Error("remove runner", "runner", r.Name, "err", err)
				continue
			}
			_ = c.ledger.SetState(r.Name, "", "controller stopped")
			_ = c.ledger.MarkRemoved(r.Name, c.now())
			n++
		}
	}
	c.forgetLiveRunners("controller stopped")
	c.unregister(ctx)
	var wg sync.WaitGroup
	for _, h := range c.hosts {
		if !h.started {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := stopAgent(ctx, h.cfg); err != nil {
				c.log.Error(fmt.Sprintf("host %s: agent not stopped", h.cfg.Name), "err", err)
			}
		}()
	}
	wg.Wait()
	c.Close()
	c.log.Info(fmt.Sprintf("stopped  %d runners removed", n))
}

// forgetLiveRunners marks runners whose container we could not see removed as lost.
func (c *Controller) forgetLiveRunners(reason string) {
	live, _ := c.ledger.LiveRunners()
	for _, r := range live {
		_ = c.ledger.SetState(r.Name, "lost", reason)
	}
}

// Down removes everything pumasi left behind: on every host the agent, the
// containers labelled pumasi and the cache directory; on GitHub the remaining
// registrations and the classes' scale sets. It needs no running agent.
func (c *Controller) Down(ctx context.Context) error {
	errs := make([]error, len(c.hosts))
	var wg sync.WaitGroup
	for i, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			out, err := cleanHost(cctx, h.cfg)
			if err != nil {
				errs[i] = err
				c.log.Error(fmt.Sprintf("host %s not cleaned", h.cfg.Name), "err", err)
				return
			}
			c.log.Info(fmt.Sprintf("host %s  agent stopped, %s containers removed, ~/.cache/pumasi removed", h.cfg.Name, lastLine(out)))
		}()
	}
	wg.Wait()
	c.forgetLiveRunners("down")
	c.unregister(ctx)
	for _, cl := range c.cfg.Classes {
		set, err := c.gh.GetRunnerScaleSet(ctx, c.cfg.RunnerGroupID, cl.ScaleSet)
		if err != nil || set == nil {
			continue
		}
		if err := c.gh.DeleteRunnerScaleSet(ctx, set.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete scale set %s: %w", cl.ScaleSet, err))
			continue
		}
		c.ev.Write("scale-set-deleted", set.ID)
		c.log.Info(fmt.Sprintf("class %s  scale set %s deleted", cl.Name, cl.ScaleSet))
	}
	return errors.Join(errs...)
}

// EventLog appends JSON lines (listener messages, decisions, starts) for later analysis.
type EventLog struct {
	mu sync.Mutex
	f  *os.File
}

func OpenEventLog(path string) (*EventLog, error) {
	if path == "" {
		return &EventLog{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	return &EventLog{f: f}, err
}

func (l *EventLog) Write(kind string, v any) {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(ledger.TimeFormat), "kind": kind, "data": v})
	_, _ = l.f.Write(append(b, '\n'))
}

func (l *EventLog) Close() {
	if l != nil && l.f != nil {
		l.f.Close()
	}
}
