package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// slotMgr runs the host's registered runners (classes with slots:). The
// controller only hands it a plan; the manager follows it by itself, every
// second, also while the controller is away:
//
//   - a slot that should listen gets a fresh copy of the runner with the
//     slot's credentials and runs it for one job (run.sh --once);
//   - when that run ends, the copy is removed with everything the job left
//     and the slot may listen again;
//   - an idle listener that no longer fits (another class took the room, a
//     person sat down at the desktop) is stopped, but only after its own
//     trace says no job is on its way to it.
type slotMgr struct {
	a     *Agent
	nat   *Native
	store SlotStore
	keep  string // where registrations that stay on GitHub are put when a slot is given up

	regMu   sync.Mutex  // one registration at a time
	leaving atomic.Bool // the host is leaving: the manager does nothing more

	mu       sync.Mutex
	plan     *api.SlotPlan
	st       map[string]*slotState
	view     []api.Slot
	jobs     []api.SlotJob // ended runs that had a job, oldest first
	psiShut  bool
	lastOver string
	// Only the manager's loop reads and writes it.
	lastClosed string
	lastWhy    map[string]string
	// When a runner of the class last took a job: the class that has waited
	// longest starts listening first.
	lastTook map[string]time.Time
}

// slotState is what the manager remembers of a slot between its looks.
type slotState struct {
	fails   int
	retryAt time.Time
	why     string // why the last run ended before a job

	// Of the current run:
	run       string
	job       string
	taking    bool // a job is on its way to it, or it runs one
	diagFile  string
	diagOff   int64
	savedBack bool
	listenAt  time.Time // when it was first seen listening
	stopAt    time.Time // when its stop was first asked
	lastStop  time.Time // and last
	fail      string    // the stop is a failure of the run: why
	stopped   string    // pumasi ended it under its job: why
}

const (
	slotJobsKept   = 100
	slotProvision  = 3 * time.Minute
	slotStopPeriod = 20 * time.Second
	// A job that waits for a class reaches a new listener within a few
	// seconds of "Listening for Jobs" (1 to 3 measured). Until this long
	// after, the other classes do not start listening for the same room.
	slotFreshListen = 8 * time.Second
	slotFreshStart  = time.Minute // a runner that takes this long to listen holds nobody back
)

func newSlotMgr(a *Agent, nat *Native, dir, keep string) *slotMgr {
	m := &slotMgr{a: a, nat: nat, store: SlotStore{Dir: dir}, keep: keep, st: map[string]*slotState{}}
	m.jobs = a.loadedJobs
	a.loadedJobs = nil
	if b, err := os.ReadFile(m.planFile()); err == nil {
		var p api.SlotPlan
		if json.Unmarshal(b, &p) == nil && p.Version != "" {
			m.plan = &p
		}
	}
	return m
}

func (m *slotMgr) planFile() string { return filepath.Join(m.store.Dir, "plan.json") }

// PlanVersion hashes a plan without its Version field.
func PlanVersion(p api.SlotPlan) string {
	p.Version = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// setPlan stores the controller's plan; the host follows it from the next look on.
func (m *slotMgr) setPlan(p api.SlotPlan) error {
	if p.Version == "" {
		p.Version = PlanVersion(p)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.store.Dir, 0o700); err != nil {
		return err
	}
	tmp := m.planFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.planFile()); err != nil {
		return err
	}
	m.mu.Lock()
	m.plan = &p
	m.mu.Unlock()
	return nil
}

func (m *slotMgr) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

// snapshot is the manager's part of the agent's snapshot.
func (m *slotMgr) snapshot() (slots []api.Slot, jobs []api.SlotJob, plan string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plan != nil {
		plan = m.plan.Version
	}
	return append([]api.Slot{}, m.view...), append([]api.SlotJob{}, m.jobs...), plan
}

// slotRun is a slot's runner process as the agent's records show it.
type slotRun struct {
	name      string
	state     string
	received  time.Time
	listening bool
	started   *time.Time
	runner    api.Runner
}

func (m *slotMgr) tick(ctx context.Context) {
	if m.leaving.Load() {
		return
	}
	metas := m.store.List()
	m.mu.Lock()
	plan := m.plan
	m.mu.Unlock()

	now := m.a.now()
	// 1. What runs, from the agent's records.
	runs := map[string]slotRun{}
	var heldCPU, heldMem float64
	m.a.dataMu.Lock()
	for _, r := range m.a.recs {
		if r.Slot == "" {
			if r.State != "exited" && r.State != "gone" {
				heldCPU += r.CPUs
				heldMem += float64(r.MemBytes)
			}
			continue
		}
		runs[r.Slot] = slotRun{name: r.Name, state: r.State, received: r.receivedAt, listening: r.ListeningAt != nil, started: r.JobStartedAt, runner: r.Runner}
	}
	host := m.a.host
	m.a.dataMu.Unlock()

	// 2. Look at each run's own output: it knows first that a job has come.
	type seen struct {
		ev     slotLog
		exited bool
	}
	looks := map[string]seen{}
	sampled := false
	for key, run := range runs {
		st := m.state(key, run.name)
		dir := m.nat.dir(run.name)
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		ev := parseSlotLog(string(b), now)
		if !st.taking && (!ev.JobStarted.IsZero() || m.jobComing(st, dir)) {
			st.taking = true
			if meta, ok := findSlot(metas, key); ok {
				if m.lastTook == nil {
					m.lastTook = map[string]time.Time{}
				}
				m.lastTook[meta.Class] = now
			}
		}
		if !ev.Listening.IsZero() && st.listenAt.IsZero() {
			st.listenAt = now
		}
		if ev.Job != "" {
			st.job = ev.Job
		}
		_, err := os.Stat(filepath.Join(dir, "exit"))
		exited := err == nil || run.state == "exited"
		if err == nil && run.state != "exited" && !sampled {
			// The agent's own sample takes the run's last numbers before it is removed.
			m.a.Sample(ctx)
			sampled = true
		}
		if !ev.Listening.IsZero() && !st.savedBack {
			if meta, ok := findSlot(metas, key); ok {
				m.store.saveBack(meta, filepath.Join(dir, "runner"))
			}
			if _, err := os.Stat(filepath.Join(dir, "runner", ".runner_migrated")); err == nil || now.Sub(ev.Listening) > time.Minute {
				st.savedBack = true
			}
		}
		m.a.dataMu.Lock()
		if r := m.a.recs[run.name]; r != nil {
			if !ev.Listening.IsZero() && r.ListeningAt == nil {
				r.ListeningAt = ptr(ev.Listening)
				if !r.receivedAt.IsZero() {
					r.ProvisionMs = ev.Listening.Sub(r.receivedAt).Milliseconds()
				}
			}
			if !ev.JobStarted.IsZero() && r.JobStartedAt == nil {
				r.JobStartedAt = ptr(ev.JobStarted)
			}
			r.taking = st.taking
			run.listening, run.started, run.runner = r.ListeningAt != nil, r.JobStartedAt, r.Runner
		}
		m.a.dataMu.Unlock()
		if !ev.Listening.IsZero() {
			st.fails, st.why, st.retryAt = 0, "", time.Time{}
		}
		runs[key] = run
		looks[key] = seen{ev: ev, exited: exited}
	}

	// 3. Runs that are over, or that pumasi has to end.
	classes := map[string]api.SlotClass{}
	provision := slotProvision
	if plan != nil {
		for _, c := range plan.Classes {
			classes[c.Name] = c
		}
		if plan.ProvisionSec > 0 {
			provision = time.Duration(plan.ProvisionSec) * time.Second
		}
	}
	for key, run := range runs {
		st, look := m.state(key, run.name), looks[key]
		c := classes[run.runner.Class]
		switch {
		case look.exited:
			m.finish(ctx, key, run, look.ev)
			delete(runs, key)
		case run.started != nil && c.MaxDurationSec > 0 && now.Sub(*run.started) > time.Duration(c.MaxDurationSec)*time.Second:
			// The one stop of a running job pumasi makes on its own: the class asks for it.
			st.stopped = fmt.Sprintf("job ran longer than maxDuration %s", time.Duration(c.MaxDurationSec)*time.Second)
			m.finish(ctx, key, run, look.ev)
			delete(runs, key)
		case st.taking || !st.stopAt.IsZero():
		case look.ev.Conflict && !run.listening:
			st.fail = "GitHub already has a session for this runner: another process listens with the same registration (its old service?), or a session that was cut off has not expired yet"
			st.stopAt = now
			m.stop(ctx, key, run, st)
		case !run.listening && !run.received.IsZero() && now.Sub(run.received) > provision:
			st.fail = fmt.Sprintf("did not listen within %s: %s", provision, look.ev.lastLine())
			st.stopAt = now
			m.stop(ctx, key, run, st)
		}
	}

	// 4. Slots the operator gave up, once nothing runs from them.
	var kept []SlotMeta
	for _, meta := range metas {
		if _, running := runs[meta.Key()]; !running && m.store.releaseAsked(meta) {
			m.giveUp(meta)
			continue
		}
		kept = append(kept, meta)
	}
	metas = kept

	// 5. Who should listen.
	in := planIn{HeldCPUs: heldCPU, HeldMem: heldMem, MachineCPUs: -1, MachineMem: -1,
		BudgetCPUs: m.a.cfg.BudgetCPUs, BudgetMem: float64(m.a.cfg.BudgetMem)}
	if plan == nil {
		in.Closed = "no plan from a controller yet"
	} else {
		in.Classes = plan.Classes
		in.BudgetCPUs = minPos(in.BudgetCPUs, plan.BudgetCPUs)
		in.BudgetMem = minPos(in.BudgetMem, float64(plan.BudgetMem))
		in.Closed = m.closed(plan, host)
		// The host says so itself: with the controller away nothing else does.
		if in.Closed != m.lastClosed {
			if in.Closed != "" {
				m.a.log.Warn("slots closed: nothing new listens", "why", in.Closed)
			} else {
				m.a.log.Info("slots open again", "was", m.lastClosed)
			}
			m.lastClosed = in.Closed
		}
	}
	if p := m.nat.problem(); p != "" && in.Closed == "" {
		in.Closed = p
	}
	stoppable := map[string]bool{}
	metaOf := map[string]SlotMeta{}
	var busyCPU, busyMem float64
	for _, meta := range metas {
		key := meta.Key()
		metaOf[key] = meta
		s := slotIn{Key: key, Class: meta.Class, N: meta.N, State: "off", Release: m.store.releaseAsked(meta)}
		st := m.peek(key)
		if run, ok := runs[key]; ok {
			switch {
			case st != nil && st.taking:
				s.State = "busy"
				busyCPU += classes[meta.Class].CPUs
				busyMem += float64(classes[meta.Class].MemBytes)
			case run.listening:
				s.State = "listening"
				s.Fresh = st == nil || st.listenAt.IsZero() || now.Sub(st.listenAt) < slotFreshListen
			default:
				s.State = "starting"
				s.Fresh = run.received.IsZero() || now.Sub(run.received) < slotFreshStart
			}
			// A run whose stop is under way is left to it. One that is still
			// starting is stopped once it listens: a runner cut off while it
			// opens its session leaves that session behind at GitHub, and the
			// registration cannot listen again until it expires (minutes).
			s.Stoppable = meta.V2 && run.listening && st != nil && st.stopAt.IsZero()
			stoppable[key] = s.Stoppable
		} else {
			s.Ready = st == nil || !now.Before(st.retryAt)
		}
		in.Slots = append(in.Slots, s)
	}
	if !host.sampledAt.IsZero() && host.cpus > 0 {
		in.MachineCPUs = host.cpus - host.nonRunnerCPU - heldCPU - busyCPU
		if host.memBytes > 0 {
			in.MachineMem = float64(host.memBytes) - float64(host.nonRunnerMem) - heldMem - busyMem
		}
	}
	for _, c := range in.Classes {
		in.Order = append(in.Order, c.Name)
	}
	sort.SliceStable(in.Order, func(i, j int) bool { return m.lastTook[in.Order[i]].Before(m.lastTook[in.Order[j]]) })
	out := planSlots(in)
	// Why a slot stays off is written down when it changes: with the
	// controller away the log is the only place that says it.
	for key, why := range out.Why {
		if why != "" && m.lastWhy[key] != why {
			m.a.log.Info("slot stays off", "slot", key, "why", why)
		}
	}
	m.lastWhy = out.Why

	// 6. Act: stop what no longer fits, then start.
	for _, key := range out.Stop {
		if run, ok := runs[key]; ok {
			st := m.state(key, run.name)
			st.stopAt = now
			m.stop(ctx, key, run, st)
		}
	}
	// A stop that did not end the run (it was not up yet) is asked again.
	for key, run := range runs {
		st := m.state(key, run.name)
		if !st.stopAt.IsZero() && !st.taking && now.Sub(st.lastStop) >= 5*time.Second {
			m.stop(ctx, key, run, st)
		}
	}
	for _, key := range out.Start {
		meta := metaOf[key]
		st := m.state(key, "")
		if err := m.start(ctx, meta, classes[meta.Class], plan.Image); err != nil {
			m.failed(st, key, now, err.Error())
		}
	}

	// 7. What the controller and `pumasi status` see.
	view := make([]api.Slot, 0, len(metas))
	for _, meta := range metas {
		key := meta.Key()
		s := api.Slot{Class: meta.Class, N: meta.N, Name: meta.Name, ID: meta.ID, Origin: meta.Origin, From: meta.From, State: "off", Why: out.Why[key]}
		st := m.peek(key)
		if run, ok := runs[key]; ok {
			r := run.runner
			s.Runner = &r
			switch {
			case st != nil && st.taking:
				s.State, s.Job = "busy", st.job
			case run.listening:
				s.State = "listening"
			default:
				s.State = "starting"
			}
			if st != nil && !st.stopAt.IsZero() && !st.taking {
				s.Why = "stopping"
			}
			if !meta.V2 && s.State != "busy" {
				s.Why = "an idle listener of this runner is never stopped (not GitHub's broker protocol)"
			}
		} else if st != nil && st.fails > 0 {
			s.Why = st.why
			if st.fails >= 3 {
				s.State = "failing"
			} else if now.Before(st.retryAt) {
				s.Why = fmt.Sprintf("%s (again in %s)", st.why, st.retryAt.Sub(now).Round(time.Second))
			}
		}
		view = append(view, s)
	}
	m.mu.Lock()
	m.view = view
	if out.Over != m.lastOver {
		if out.Over != "" {
			m.a.log.Warn("over the budget until a job ends", "now", out.Over)
		}
		m.lastOver = out.Over
	}
	// Forget slots that are gone.
	for key := range m.st {
		if _, ok := metaOf[key]; !ok {
			if _, running := runs[key]; !running {
				delete(m.st, key)
			}
		}
	}
	m.mu.Unlock()
}

func findSlot(metas []SlotMeta, key string) (SlotMeta, bool) {
	for _, m := range metas {
		if m.Key() == key {
			return m, true
		}
	}
	return SlotMeta{}, false
}

// state is the slot's memory, started anew for a run it has not seen. An empty
// run asks for the memory as it is.
func (m *slotMgr) state(key, run string) *slotState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.st[key]
	if st == nil {
		st = &slotState{}
		m.st[key] = st
	}
	if run != "" && st.run != run {
		*st = slotState{fails: st.fails, retryAt: st.retryAt, why: st.why, run: run}
	}
	return st
}

func (m *slotMgr) peek(key string) *slotState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st[key]
}

// closed is why nothing new may listen on this host right now ("" = open):
// the controller's word, or what the host sees of itself.
func (m *slotMgr) closed(plan *api.SlotPlan, h hostState) string {
	if plan.Closed != "" {
		return plan.Closed
	}
	if h.closed != "" {
		return h.closed
	}
	if m.a.cfg.Kind == "desktop" && h.inputIdle != nil && *h.inputIdle < api.PersonAway {
		return "in use"
	}
	if psi, ok := h.psi["cpu"]; ok && plan.PSICPUMax > 0 {
		// Open again only well below the mark, or listeners would come and go with every sample.
		m.mu.Lock()
		if psi >= plan.PSICPUMax {
			m.psiShut = true
		} else if psi < plan.PSICPUMax*0.75 {
			m.psiShut = false
		}
		shut := m.psiShut
		m.mu.Unlock()
		if shut {
			return fmt.Sprintf("cpu pressure %.0f", psi)
		}
	}
	if h.diskTotal > 0 && plan.DiskMinFree > 0 {
		if pct := 100 * float64(h.diskFree) / float64(h.diskTotal); pct < plan.DiskMinFree {
			return fmt.Sprintf("disk %.0f%% free", pct)
		}
	}
	return ""
}

func minPos(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}

// start lets a slot listen: a fresh copy of the runner with the slot's
// credentials, run for one job.
func (m *slotMgr) start(ctx context.Context, meta SlotMeta, c api.SlotClass, image string) error {
	if c.Name == "" || c.CPUs <= 0 || c.MemBytes <= 0 {
		return fmt.Errorf("class %s has no size in the plan", meta.Class)
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("%s-%s-s%d-%s", c.Name, m.a.cfg.Host, meta.N, hex.EncodeToString(b[:]))
	spec := Spec{Name: name, Class: c.Name, CPUs: c.CPUs, MemBytes: c.MemBytes, Image: image, Driver: "native",
		Env: c.Env, Path: c.Path, Slot: meta.Key(),
		install: func(runnerDir string) error { return m.store.install(meta, runnerDir, c.Path) }}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := m.a.startSlot(sctx, spec); err != nil {
		return err
	}
	m.a.log.Info("slot listens", "slot", meta.Key(), "runner", meta.Name, "run", name)
	return nil
}

// stop ends an idle listener. It is only done when the listener's own trace
// shows no job on its way; if one is, the run stays and counts as busy.
func (m *slotMgr) stop(ctx context.Context, key string, run slotRun, st *slotState) {
	dir := m.nat.dir(run.name)
	sign := func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		return !parseSlotLog(string(b), time.Time{}).JobStarted.IsZero() || m.jobComing(st, dir)
	}
	// A listener that does not end on the interrupt is ended the hard way,
	// after the same look.
	st.lastStop = m.a.now()
	hard := st.lastStop.Sub(st.stopAt) > slotStopPeriod
	stopped, err := stopIdleListener(filepath.Join(dir, "runner"), sign, hard)
	switch {
	case err != nil:
		m.a.log.Warn("slot listener not stopped", "slot", key, "err", err)
	case !stopped && sign():
		st.taking, st.stopAt, st.fail = true, time.Time{}, ""
		m.a.log.Info("slot listener took a job while it was being stopped; it stays", "slot", key, "run", run.name)
	case stopped && st.fail == "":
		m.a.log.Info("slot stops listening", "slot", key, "run", run.name)
	}
}

// jobComing reads what the listener has traced since the last look: GitHub's
// broker hands a job over in two steps, and the trace shows the first one
// (the request is acknowledged) a second or two before the runner has
// acquired the job and prints "Running job".
func (m *slotMgr) jobComing(st *slotState, dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "runner", "_diag", "Runner_*.log"))
	if len(files) == 0 {
		return false
	}
	sort.Strings(files)
	file := files[len(files)-1]
	if st.diagFile != file {
		st.diagFile, st.diagOff = file, 0
	}
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= st.diagOff {
		return false
	}
	buf := make([]byte, fi.Size()-st.diagOff)
	n, _ := f.ReadAt(buf, st.diagOff)
	buf = buf[:n]
	if jobSignRE.Match(buf) {
		return true
	}
	// Keep the end: a line may still be half written.
	st.diagOff += int64(max(0, n-256))
	return false
}

// jobSignRE matches the listener's trace of a job that is being handed to
// it: the request it acknowledges, the job message it then fetches.
var jobSignRE = regexp.MustCompile(`Acknowledging runner request|Job request \S+ for plan|Running job`)

// finish removes a run that is over (or that pumasi ends) and records what
// it was: a job, a stop, or a runner that could not listen.
func (m *slotMgr) finish(ctx context.Context, key string, run slotRun, ev slotLog) {
	st := m.state(key, run.name)
	dir := m.nat.dir(run.name)
	if meta, ok := findSlot(m.store.List(), key); ok && !st.savedBack {
		m.store.saveBack(meta, filepath.Join(dir, "runner"))
	}
	final, err := m.a.Delete(ctx, run.name)
	if err != nil {
		m.a.log.Warn("slot run not removed; tried again", "slot", key, "run", run.name, "err", err)
		return
	}
	now := m.a.now()
	hadJob := !ev.JobStarted.IsZero() || final.JobStartedAt != nil
	switch {
	case hadJob:
		if final.JobStartedAt == nil {
			final.JobStartedAt = ptr(ev.JobStarted)
		}
		if final.ExitedAt == nil {
			final.ExitedAt = ptr(now)
		}
		m.mu.Lock()
		job := api.SlotJob{Runner: final, Slot: key, Registered: slotName(m.view, key), Job: ev.Job, Result: ev.Result, Stopped: st.stopped}
		m.jobs = append(m.jobs, job)
		if len(m.jobs) > slotJobsKept {
			m.jobs = m.jobs[len(m.jobs)-slotJobsKept:]
		}
		st.fails, st.why, st.retryAt = 0, "", time.Time{}
		m.mu.Unlock()
		m.a.opMu.Lock()
		m.a.save()
		m.a.opMu.Unlock()
		m.a.log.Info("slot job over", "slot", key, "job", ev.Job, "result", ev.Result, "leftover", final.Leftover)
	case st.fail != "":
		m.failed(st, key, now, st.fail)
	case !st.stopAt.IsZero():
		// Stopped on purpose while idle: nothing to tell.
	default:
		m.failed(st, key, now, "ended before a job: "+ev.lastLine())
	}
	if final.Leftover != "" && !hadJob {
		m.a.log.Warn("slot run removed, but not all of its files", "slot", key, "left", final.Leftover)
	}
}

func slotName(view []api.Slot, key string) string {
	for _, s := range view {
		if SlotKey(s.Class, s.N) == key {
			return s.Name
		}
	}
	return ""
}

// failed counts a run that did not come to listen and sets when the slot is
// tried again: soon at first, then every five minutes.
func (m *slotMgr) failed(st *slotState, key string, now time.Time, why string) {
	m.mu.Lock()
	st.fails++
	st.why = why
	wait := []time.Duration{15 * time.Second, time.Minute}
	d := 5 * time.Minute
	if st.fails <= len(wait) {
		d = wait[st.fails-1]
	}
	st.retryAt = now.Add(d)
	m.mu.Unlock()
	m.a.log.Warn("slot cannot listen", "slot", key, "fails", st.fails, "why", why, "again", d)
}

// giveUp frees a slot nothing runs from. An adopted registration is only
// forgotten (its files are where they were copied from); one pumasi made is
// moved to the keep directory, because it is still registered on GitHub.
func (m *slotMgr) giveUp(meta SlotMeta) {
	if meta.Origin == OriginAdopted {
		if err := m.store.Remove(meta); err != nil {
			m.a.log.Warn("slot not released", "slot", meta.Key(), "err", err)
			return
		}
		m.a.log.Info("slot released", "slot", meta.Key(), "runner", meta.Name, "its files are in", meta.From)
		return
	}
	dst, err := m.store.MoveOut(meta, m.keep)
	if err != nil {
		m.a.log.Warn("slot not released", "slot", meta.Key(), "err", err)
		return
	}
	m.a.log.Info("slot released", "slot", meta.Key(), "runner", meta.Name, "its files are in", dst)
}

// register fills the plan's empty slots: for each it runs GitHub's config.sh
// in a scratch copy of the runner with the registration token and keeps the
// credentials that leaves. The token goes through the environment and is
// kept nowhere.
func (m *slotMgr) register(ctx context.Context, req api.RegisterRequest) api.RegisterResult {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	var res api.RegisterResult
	m.mu.Lock()
	plan := m.plan
	m.mu.Unlock()
	if plan == nil {
		res.Problem = "the host has no plan from the controller yet; try again in a moment"
		return res
	}
	have := map[string]bool{}
	for _, meta := range m.store.List() {
		have[meta.Key()] = true
	}
	for _, c := range plan.Classes {
		if req.Class != "" && req.Class != c.Name {
			continue
		}
		for n := 1; n <= c.Slots; n++ {
			if have[SlotKey(c.Name, n)] {
				res.Had++
				continue
			}
			meta, err := m.registerOne(ctx, plan, c, n, req.Token)
			if err != nil {
				res.Problem = fmt.Sprintf("slot %s: %v", SlotKey(c.Name, n), err)
				return res // the next would fail the same way
			}
			res.Registered = append(res.Registered, meta.Name)
			m.a.log.Info("slot registered", "slot", meta.Key(), "runner", meta.Name)
		}
	}
	return res
}

func (m *slotMgr) registerOne(ctx context.Context, plan *api.SlotPlan, c api.SlotClass, n int, token string) (SlotMeta, error) {
	scratch := filepath.Join(m.store.Dir, ".register")
	_ = removeTree(scratch)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return SlotMeta{}, err
	}
	defer removeTree(scratch)
	dir := filepath.Join(scratch, "runner")
	if err := m.nat.cloneDist(ctx, plan.Image, dir); err != nil {
		return SlotMeta{}, err
	}
	name := fmt.Sprintf("%s-%s-%d", c.Name, m.a.cfg.Host, n)
	out, err := runConfig(ctx, dir, token, "--unattended", "--url", plan.URL, "--name", name,
		"--labels", strings.Join(c.Labels, ","), "--no-default-labels", "--work", "_work", "--disableupdate", "--replace")
	if err != nil {
		return SlotMeta{}, fmt.Errorf("config.sh: %s", configProblem(out, err))
	}
	return m.store.Add(c.Name, n, OriginRegistered, dir, false)
}

// runConfig runs the runner's config script in dir. The token is given
// through the environment, so it is in no argument list.
func runConfig(ctx context.Context, dir, token string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, filepath.Join(dir, configScript), args...)
	cmd.Dir = dir
	cmd.Env = append(runnerEnv(os.Environ()), "ACTIONS_RUNNER_INPUT_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// configProblem is the line of config.sh's output that says what went wrong.
func configProblem(out string, err error) string {
	var last string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "√") {
			continue
		}
		last = line
		if strings.Contains(line, "Http response code") || strings.Contains(line, "Failed") {
			break
		}
	}
	if last == "" {
		return err.Error()
	}
	if strings.Contains(last, "NotFound") || strings.Contains(last, "Unauthorized") {
		last += " (is the token a registration token of this URL, and less than an hour old?)"
	}
	return last
}

// slotLog is what a slot's run has printed.
type slotLog struct {
	LogEvents
	Job      string // the display name of the job it took
	Result   string // Succeeded | Failed | Canceled, once the job is over
	Conflict bool   // GitHub refuses a second session for the registration
	lines    []string
}

var (
	runningJobRE = regexp.MustCompile(`^(\S+ \S+Z): Running job: (.*)$`)
	jobResultRE  = regexp.MustCompile(`^\S+ \S+Z: Job (.*) completed with result: (\S+)$`)
)

func parseSlotLog(log string, now time.Time) slotLog {
	ev := slotLog{LogEvents: parseRunnerLog(log, now)}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev.lines = append(ev.lines, line)
		if g := runningJobRE.FindStringSubmatch(line); g != nil && ev.Job == "" {
			ev.Job = g[2]
		} else if g := jobResultRE.FindStringSubmatch(line); g != nil {
			ev.Result = g[2]
		} else if strings.Contains(line, "A session for this runner already exists") {
			ev.Conflict = true
		}
	}
	return ev
}

// lastLine is the last thing the runner said itself, without the lines its
// scripts print on every exit.
func (ev slotLog) lastLine() string {
	for i := len(ev.lines) - 1; i >= 0; i-- {
		l := ev.lines[i]
		if strings.HasPrefix(l, "Exiting") || strings.HasPrefix(l, "Runner listener exit") || strings.HasPrefix(l, "at ") {
			continue
		}
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		return l
	}
	return "it printed nothing"
}

// GiveUpSlots is what `pumasi leave` does with the host's registered runners
// once none of them runs: the registrations pumasi made are removed from
// GitHub when a token is given (a registration or removal token) and
// otherwise their files are kept in keep; adopted ones are only forgotten.
// It returns one line per slot.
func GiveUpSlots(ctx context.Context, cache, keep, token string) []string {
	store := SlotStore{Dir: SlotsDir(cache)}
	nat := &Native{Root: cache}
	var out []string
	for _, meta := range store.List() {
		if meta.Origin == OriginAdopted {
			_ = store.Remove(meta)
			out = append(out, fmt.Sprintf("%s: still registered on GitHub; its files are untouched in %s (start its service there again to use it)", meta.Name, meta.From))
			continue
		}
		why := "no token was given"
		if token != "" {
			err := unregister(ctx, nat, store, meta, token)
			if err == nil {
				_ = store.Remove(meta)
				out = append(out, fmt.Sprintf("%s: registration removed from GitHub", meta.Name))
				continue
			}
			why = err.Error()
		}
		dst, err := store.MoveOut(meta, keep)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: still registered on GitHub, and its files could not be kept (%v)", meta.Name, err))
			continue
		}
		out = append(out, fmt.Sprintf("%s: still registered on GitHub (%s); its files are kept in %s — `pumasi adopt` takes them back, or remove the runner in GitHub's settings", meta.Name, why, dst))
	}
	_ = os.RemoveAll(store.Dir)
	return out
}

// unregister removes a slot's registration from GitHub with config.sh remove.
func unregister(ctx context.Context, nat *Native, store SlotStore, meta SlotMeta, token string) error {
	dists, _ := filepath.Glob(filepath.Join(nat.Root, "dist", "actions-runner-*"))
	if len(dists) == 0 {
		return errors.New("no runner is unpacked here to remove it with")
	}
	sort.Strings(dists)
	scratch := filepath.Join(store.Dir, ".register")
	_ = removeTree(scratch)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}
	defer removeTree(scratch)
	dir := filepath.Join(scratch, "runner")
	if err := cloneTree(dists[len(dists)-1], dir); err != nil {
		return err
	}
	if err := store.install(meta, dir, nil); err != nil {
		return err
	}
	if out, err := runConfig(ctx, dir, token, "remove"); err != nil {
		return fmt.Errorf("config.sh remove: %s", configProblem(out, err))
	}
	return nil
}

// ReadSlotPlan is the plan the host last got from its controller (nil = none).
func ReadSlotPlan(cache string) *api.SlotPlan {
	b, err := os.ReadFile(filepath.Join(SlotsDir(cache), "plan.json"))
	var p api.SlotPlan
	if err != nil || json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// slotRunHoldsJob reads a run's output and trace from the start: has a job
// come to it?
func slotRunHoldsJob(runDir string) bool {
	b, _ := os.ReadFile(filepath.Join(runDir, "log"))
	if !parseSlotLog(string(b), time.Time{}).JobStarted.IsZero() {
		return true
	}
	return (&slotMgr{}).jobComing(&slotState{}, runDir)
}

// SlotRuns lists the runs of the host's registered runners that are up:
// those that hold a job and those that only listen.
func SlotRuns(ctx context.Context, cache string) (busy, idle []string) {
	nat := &Native{Root: cache, Runners: RunnerRoot(cache)}
	cs, _ := nat.List(ctx)
	for _, c := range cs {
		if c.Slot == "" || c.State != "running" {
			continue
		}
		if slotRunHoldsJob(nat.dir(c.Name)) {
			busy = append(busy, c.Name)
		} else {
			idle = append(idle, c.Name)
		}
	}
	return busy, idle
}

// StopSlotListeners ends the idle listeners of the host's registered runners
// the careful way (none that has a job on its way) and waits for them to
// close their sessions. It returns the runs that hold a job.
func StopSlotListeners(ctx context.Context, cache string) (busy []string) {
	nat := &Native{Root: cache, Runners: RunnerRoot(cache)}
	_, idle := SlotRuns(ctx, cache)
	var stopping []string
	for _, name := range idle {
		dir := nat.dir(name)
		if stopped, _ := stopIdleListener(filepath.Join(dir, "runner"), func() bool { return slotRunHoldsJob(dir) }, false); stopped {
			stopping = append(stopping, dir)
		}
	}
	for deadline := time.Now().Add(15 * time.Second); len(stopping) > 0 && time.Now().Before(deadline) && ctx.Err() == nil; {
		left := stopping[:0]
		for _, dir := range stopping {
			if _, err := os.Stat(filepath.Join(dir, "exit")); err != nil {
				left = append(left, dir)
			}
		}
		if stopping = left; len(stopping) > 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	busy, _ = SlotRuns(ctx, cache)
	return busy
}

// LeaveSlots is StopSlotListeners for the running agent: its own manager
// starts no listener any more.
func (a *Agent) LeaveSlots(ctx context.Context, cache string) (busy []string) {
	if a.slots == nil {
		return nil
	}
	a.slots.leaving.Store(true)
	a.opMu.Lock() // a look that is under way ends first
	a.opMu.Unlock()
	return StopSlotListeners(ctx, cache)
}
