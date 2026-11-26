// Package controller runs one listener per class (scale set), keeps the ledger,
// and periodically turns agent snapshots and waiting jobs into a Decide plan.
package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/join"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

type Controller struct {
	noMarker map[string]time.Time // runners warned about in markerCheck, by GitHub's job start; Evaluate's goroutine only
	cfg      *config.Config
	gh       github
	token    string
	ledger   *ledger.Ledger
	log      *slog.Logger
	// ListenerLog receives the scale set listener's own lines (one per long poll).
	ListenerLog *slog.Logger
	ev          *EventLog
	now         func() time.Time

	// hosts changes only on the evaluating goroutine (a host joins or leaves);
	// hostsMu covers the readers on other goroutines.
	hosts   []*host
	hostsMu sync.Mutex
	join    *join.Server // the listener every host dials
	// listenAddr is the address that listener bound.
	listenAddr string
	classes    []*class
	kick       chan struct{}

	mu sync.Mutex // guards class.stats and inflight
	// Runners whose start request the agent has not answered yet (zero), or
	// answered at that time: a snapshot asked for before then cannot show them.
	inflight map[string]time.Time
	// cfgRead: the config file's modification time when its priority rules were last read.
	cfgRead time.Time
	// sized: what each waiting job was taken to need in the last decision (under mu).
	sized       map[string]size.Estimate
	starts      sync.WaitGroup
	lastDefer   map[string]deferRec
	runCheck    map[int64]time.Time
	status      Status // last round, for `pumasi status`
	distDir     string
	adopted     bool     // the first round has reported what it found on the hosts
	genericTold sync.Map // class/labels of jobs that asked only for generic labels: told once
	warmRounds  int      // rounds the adoption report waited for an agent's first sample
	book        *incidentBook
	lastAlive   time.Time // when an earlier controller last ran (zero = never)
	// Runners closed in this round because their host rebooted, per host.
	rebootLost map[string][]ledger.Runner
	rerunCheck map[int64]time.Time
	// Dead runners whose job's run GitHub has not named yet -> why they died. Guarded by mu.
	rerunWanted map[string]string
	// LedgerMoved names the damaged ledger that was set aside at startup ("" = none).
	LedgerMoved string
}

// github is what the controller asks of GitHub's scale set service.
type github interface {
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
	MessageSessionClient(ctx context.Context, id int, owner string, options ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error)
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, id int64) error
}

type host struct {
	cfg config.Host
	// The host's connection: the member it joined as ("" = it has not joined)
	// and that member's session. id changes on the evaluating goroutine only.
	id        string
	peer      atomic.Pointer[join.Peer]
	http      *http.Client // over the host's connection
	ensuring  atomic.Bool  // a bootstrap over ssh is in flight
	reviveBad atomic.Int32
	quiet     bool      // unreachable only because the agent has not sampled yet
	ensuredAt time.Time // last restart attempt
	// Guarded by Controller.mu: runners in a row that failed to start here, and
	// since when the host is closed for new runners because of it.
	closed      string // why the last round placed nothing here ("" = open); Evaluate's goroutine only
	provFails   int
	quarantined time.Time
	probedAt    time.Time
	probing     atomic.Bool
	agent       *api.Client
	snap        *api.Snapshot
	downSince   time.Time
	seenAt      time.Time     // the last snapshot that arrived
	askedAt     time.Time     // when that snapshot was asked for (this process's clock)
	rtt         time.Duration // how long that snapshot took, there and back
	version     string        // the agent's build, as that snapshot named it
	// What this host was last told about speaking up when the controller is
	// gone (away.go), and in which session. Evaluate's goroutine only.
	awayRank  int
	awaySent  string
	awaySince time.Time
	awayBusy  atomic.Bool
	awayDone  atomic.Pointer[awayPushed]
	os        string // as the agent last said ("" = not heard yet)
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
		kick: make(chan struct{}, 1), inflight: map[string]time.Time{}, rebootLost: map[string][]ledger.Runner{}, rerunCheck: map[int64]time.Time{}, rerunWanted: map[string]string{}, lastDefer: map[string]deferRec{}, runCheck: map[int64]time.Time{}}
	for _, hc := range cfg.Hosts {
		c.hosts = append(c.hosts, newHost(hc))
	}
	for _, cc := range cfg.Classes {
		c.classes = append(c.classes, &class{cfg: cc})
	}
	c.book = newIncidentBook(c)
	c.join = join.NewServer(cfg.StateDir(), "", log)
	c.join.Reserved = func(name string) bool {
		return slices.ContainsFunc(cfg.Hosts, func(h config.Host) bool { return h.Name == name })
	}
	c.join.Changed = func() {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
	c.syncJoined(false)
	return c, nil
}

func (c *Controller) Close() {
	if c.join != nil {
		c.join.Close()
	}
}

// runnerGroup is the repository's (or organization's) default runner group.
const runnerGroup = 1

// EnsureScaleSets finds or creates the scale set of every class.
func (c *Controller) EnsureScaleSets(ctx context.Context) error {
	for _, cl := range c.classes {
		name := cl.cfg.ScaleSet()
		var labels []scaleset.Label
		for _, l := range cl.cfg.RunsOn() {
			labels = append(labels, scaleset.Label{Name: l})
		}
		set, err := c.gh.GetRunnerScaleSet(ctx, runnerGroup, name)
		if err != nil || set == nil {
			set, err = c.gh.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
				Name: name, RunnerGroupID: runnerGroup, Labels: labels,
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
			})
			if err != nil {
				return fmt.Errorf("create scale set %s: %w", name, err)
			}
			c.ev.Write("scale-set-created", set)
		}
		cl.setID = set.ID
		// GitHub keeps the labels a scale set was made with.
		var has []string
		for _, l := range set.Labels {
			has = append(has, l.Name)
		}
		if want := cl.cfg.RunsOn(); len(has) > 0 && !sameLabels(has, want) {
			return fmt.Errorf("scale set %s exists with the labels [%s], the config asks for [%s]: labels cannot be changed, run `pumasi down` first (or rename the class)",
				name, strings.Join(has, ", "), strings.Join(want, ", "))
		}
		held := "held only to what protects its host"
		if l := cl.cfg.Limit; l.CPUs > 0 || l.Memory > 0 {
			held = "held to limit:" + cond(l.CPUs > 0, fmt.Sprintf(" %gcpu", l.CPUs), "") + cond(l.Memory > 0, " "+gib(int64(l.Memory)), "")
		}
		// cpus and memory were a limit once; say what they are now on every start.
		c.log.Info(fmt.Sprintf("class %s  runs-on: [%s]  first estimate %gcpu/%s (for a job never measured; not a limit), %s",
			cl.cfg.Name, strings.Join(cl.cfg.RunsOn(), ", "), cl.cfg.CPUs, gib(int64(cl.cfg.Memory)), held))
		if generic := genericOnly(cl.cfg.RunsOn()); len(cl.cfg.Labels) > 0 && len(generic) > 0 {
			// GitHub sends a job here when all of its labels are among the class's.
			c.log.Warn(fmt.Sprintf("class %s also takes jobs that ask for less than all of its labels, like runs-on: [%s]; a job that takes that way is named when it comes",
				cl.cfg.Name, strings.Join(generic, ", ")))
		}
	}
	return nil
}

// genericOnly is the labels among ls that every self-hosted runner of that
// machine type has: a job asking only for these did not ask for this class.
func genericOnly(ls []string) []string {
	var out []string
	for _, l := range ls {
		switch strings.ToLower(l) {
		case "self-hosted", "linux", "macos", "windows", "x64", "arm64", "arm":
			out = append(out, l)
		}
	}
	return out
}

// sameLabels compares label sets the way GitHub matches them: without case or order.
func sameLabels(a, b []string) bool {
	norm := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ToLower(s)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(norm(a), norm(b))
}

// Run opens the listener the hosts dial, makes the hosts of the config join,
// listens on every class and re-evaluates until ctx ends. Then it removes the
// runners without a job; agents and scale sets stay.
// distDir holds the builds of this binary that hosts fetch (pumasi-<os>-<arch>).
func (c *Controller) Run(ctx context.Context, distDir string) error {
	if err := c.EnsureScaleSets(ctx); err != nil {
		return err
	}
	c.distDir = distDir
	if fi, err := os.Stat(c.aliveFile()); err == nil {
		c.lastAlive = fi.ModTime()
	}
	go c.book.deliver(ctx)
	// Quarantine lives in this process; one left open by an earlier controller is over.
	if open, err := c.ledger.OpenIncidents(); err == nil {
		for _, in := range open {
			if in.Kind == "host-quarantined" {
				c.book.resolve(in.Key)
			}
		}
	}
	if c.LedgerMoved != "" {
		c.book.report("problem", "ledger-rebuilt/"+filepath.Base(c.LedgerMoved), "ledger-rebuilt", "ledger", fmt.Sprintf(
			"the ledger was damaged and set aside as %s; started from an empty one, history is lost", filepath.Base(c.LedgerMoved)),
			"runners are adopted from the hosts, waiting jobs from GitHub")
	}
	defer func() { // last notifications, on a deadline of their own
		fctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.book.flush(fctx)
	}()
	defer c.shutdown()
	if err := c.listenForHosts(distDir); err != nil {
		return err
	}
	if err := c.startAgents(ctx); err != nil {
		return err
	}
	closeStatus, err := c.serveStatus()
	if err != nil {
		return err
	}
	defer closeStatus()
	if n := c.cfg.Notify; n.Command != "" && n.Webhook == "" {
		c.log.Info("notify.command runs on this machine: nobody hears it when this controller is gone. With notify.webhook two hosts say so after 5m")
	}
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
	conflict := false
	var failingSince time.Time
	for ctx.Err() == nil {
		err := func() error {
			session, err := c.gh.MessageSessionClient(ctx, cl.setID, "pumasi-"+hostname)
			if err != nil {
				return fmt.Errorf("session: %w", err)
			}
			if !failingSince.IsZero() {
				c.log.Info(fmt.Sprintf("class %s: message session open again after %s", cl.cfg.Name, c.now().Sub(failingSince).Round(time.Second)))
			}
			failingSince, conflict = time.Time{}, false
			c.book.resolve("github/" + cl.cfg.Name)
			defer session.Close(context.WithoutCancel(ctx))
			l, err := listener.New(session, listener.Config{ScaleSetID: cl.setID, MaxRunners: math.MaxInt32, // the hosts' budgets are the limit
				Logger: c.ListenerLog.With("class", cl.cfg.Name)})
			if err != nil {
				return err
			}
			return l.Run(ctx, &scaler{c: c, cl: cl, session: session})
		}()
		if ctx.Err() != nil {
			return
		}
		if failingSince.IsZero() {
			failingSince = c.now()
		}
		if errors.Is(err, scaleset.UnauthorizedError) || strings.Contains(err.Error(), "403 Forbidden") {
			// Retrying cannot help and hammers the API with a bad token.
			c.book.open("github-auth", "github-auth", "GitHub", "GitHub rejects the token (401/403): no jobs are served until `pumasi up` is restarted with a valid PUMASI_GITHUB_TOKEN; running jobs go on")
			return
		}
		if d := c.now().Sub(failingSince); d > 2*time.Minute {
			c.book.open("github/"+cl.cfg.Name, "github-unreachable", cl.cfg.Name,
				fmt.Sprintf("no message session for class %s for %s, new jobs of this class are not seen: %s", cl.cfg.Name, d.Round(time.Second), firstLine(err.Error())))
		}
		if errors.Is(err, scaleset.ConflictError) {
			// Another machine's controller, or one that died without closing its
			// session. Keep trying quietly: a dead session expires on GitHub's side.
			if !conflict {
				c.log.Warn(fmt.Sprintf("class %s: GitHub already has a message session for scale set %s (another controller, or one that just died); no jobs for this class until it ends, retrying", cl.cfg.Name, cl.cfg.ScaleSet()))
			}
			conflict = true
		} else {
			conflict = false
			c.log.Error("listener stopped, restarting", "class", cl.cfg.Name, "err", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Minute)
		if conflict {
			// The session of a controller that died: this one is its restart and
			// serves nothing until GitHub lets go of it.
			backoff = min(backoff, 15*time.Second)
		}
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
		c.log.Info(fmt.Sprintf("unassigned %s  %s of run %d: no runner took it, GitHub withdrew the assignment (%s)", cls, j.JobDisplayName, j.WorkflowRunID, j.Result))
		if err := c.ledger.CloseDemand(j.JobID, "unassigned-"+j.Result, orNow(j.FinishTime, now)); err != nil {
			return err
		}
	}
	for _, j := range msg.JobAssignedMessages {
		if err := c.ledger.UpsertDemand(job(cls, j.JobMessageBase), now); err != nil {
			return err
		}
		if len(s.cl.cfg.Labels) > 0 && len(j.RequestLabels) > 0 && len(genericOnly(j.RequestLabels)) == len(j.RequestLabels) {
			key := strings.ToLower(strings.Join(j.RequestLabels, ","))
			if _, told := c.genericTold.LoadOrStore(cls+"/"+key, true); !told {
				c.log.Warn(fmt.Sprintf("job %s of %s asks only for runs-on: [%s] and came to class %s, whose labels include them",
					j.JobDisplayName, j.RepositoryName, strings.Join(j.RequestLabels, ", "), cls))
			}
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
		} else if err := c.ledger.SetEstimate(j.RunnerName, c.estimate(s.cl.cfg, ledger.KeyOf(j.OwnerName+"/"+j.RepositoryName, j.JobWorkflowRef, j.JobDisplayName))); err != nil {
			// The runner was started for the oldest waiting job; GitHub may have given it another.
			return err
		}
		c.log.Info(fmt.Sprintf("job %s on %s", j.JobDisplayName, j.RunnerName))
		c.rerunNamed(j.RunnerName, job(cls, j.JobMessageBase))
	}
	for _, j := range msg.JobCompletedMessages {
		if j.RunnerName == "" {
			continue
		}
		if err := c.ledger.JobCompleted(j.RunnerName, job(cls, j.JobMessageBase), j.Result, orNow(j.FinishTime, now)); err != nil {
			return err
		}
		c.log.Info(fmt.Sprintf("done %s on %s  %s", j.JobDisplayName, j.RunnerName, j.Result))
		c.rerunNamed(j.RunnerName, job(cls, j.JobMessageBase))
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
		Workflow: j.JobWorkflowRef, Name: j.JobDisplayName, RunID: j.WorkflowRunID, Event: j.EventName,
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
	c.reloadPriority()
	c.syncJoined(true)
	c.refreshHosts(ctx, now)
	c.syncAway(ctx)

	live, err := c.ledger.LiveRunners()
	if err != nil {
		c.log.Error("ledger runners", "err", err)
		return
	}
	runners := c.syncRunners(ctx, live)
	// An agent that restarted together with the controller (a new build) has not
	// sampled yet and lists no runners: what was adopted is told once it has.
	warming := slices.ContainsFunc(c.hosts, func(h *host) bool { return h.quiet && !h.downSince.IsZero() })
	if warming && c.warmRounds < 6 {
		c.warmRounds++
	} else if !c.adopted {
		c.adopted = true
		n, busy := 0, 0
		for _, r := range runners {
			if r.State != "" && r.Container != "" && r.Container != "starting" {
				n++
				if r.State == "busy" {
					busy++
				}
			}
		}
		if n > 0 {
			c.log.Info(fmt.Sprintf("adopted %d runners left on the hosts (%d running jobs)", n, busy))
		}
		finished := 0
		for _, r := range runners {
			if r.State == "busy" && r.Container == "exited" {
				finished++
			}
		}
		if down := now.Sub(c.lastAlive); !c.lastAlive.IsZero() && (n > 0 || down > 2*time.Minute) {
			c.book.report("restarted", "controller-restart", "controller-restart", "controller", fmt.Sprintf(
				"controller is back: was down %s, adopted %d runners, %d jobs finished meanwhile", down.Round(time.Second), n, finished), "")
		}
	}
	_ = os.WriteFile(c.aliveFile(), nil, 0o600)
	c.hostIncidents(now, live)
	snap := c.decideSnapshot(now, runners)
	plan := decide.Decide(snap)

	for _, st := range plan.Stop {
		c.stop(ctx, st, now)
	}
	c.reportReboots(now)
	// Starts run in the background: a slow `docker run` on one host must not hold
	// up placement elsewhere. The ledger row exists before Evaluate returns.
	for _, s := range plan.Start {
		c.start(ctx, s, now)
	}
	c.recordDefers(plan.Defer, now)
	c.waitIncidents(plan.Defer, now)
	c.setStatus(plan.Defer)
	c.unregister(ctx)
	c.checkStaleDemand(ctx, now)
	c.probeQuarantined(ctx, now)
	c.checkReruns(ctx, now)
}

func (c *Controller) refreshHosts(ctx context.Context, now time.Time) {
	var wg sync.WaitGroup
	for _, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			asked := time.Now()
			s, err := h.agent.Snapshot(cctx, c.cfg.MountPaths()...)
			if err == nil {
				h.seenAt, h.askedAt, h.rtt, h.version = now, asked, time.Since(asked), s.Version
			}
			warming := err == nil && s.SampledAt.IsZero()
			if warming {
				// Before its first sample the agent lists no runners; acting on that
				// would read every runner on the host as missing.
				err = errors.New("agent has not sampled yet")
			}
			if err != nil {
				if h.downSince.IsZero() {
					h.downSince = now
				}
				h.snap = nil
				if h.downSince.Equal(now) && !warming {
					c.log.Warn(fmt.Sprintf("host %s unreachable; its running jobs are left alone", h.cfg.Name), "err", err)
				}
				h.quiet = warming
				c.reviveAgent(ctx, h, now)
				return
			}
			if !h.downSince.IsZero() && !h.quiet {
				c.log.Info(fmt.Sprintf("host %s reachable again", h.cfg.Name))
			}
			h.quiet = false
			h.snap, h.downSince = &s, time.Time{}
		}()
	}
	wg.Wait()
}

// syncRunners copies agent observations into the ledger and joins both views.
func (c *Controller) syncRunners(ctx context.Context, live []ledger.Runner) []decide.Runner {
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
			working := ar.JobStartedAt != nil && ar.State == "running"
			if !ok && working {
				// Written off while its host was unreachable, yet it took a job.
				if revived, err := c.ledger.Revive(ar.Name); err != nil {
					c.log.Error("ledger revive", "err", err)
				} else if revived {
					if lr, err = c.ledger.Runner(ar.Name); err == nil {
						ok = true
						c.log.Info(fmt.Sprintf("adopt %s  written off earlier, found running a job", ar.Name))
					}
				}
			}
			if !ok && ar.Lost == "" {
				lr, ok = c.adopt(ctx, h, ar)
			}
			if ok {
				c.recordUsage(ar)
				if ar.OOMKills > 0 && lr.OOMKills == 0 {
					c.reportOOM(lr, ar)
				}
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
					c.provisionOK(h)
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
				c.markerCheck(h, lr, ar)
			}
			// What it holds of the host: its estimate, or its use now when that is more.
			est := size.Estimate{CPUs: cmp.Or(lr.EstCPUs, ar.CPUs), MemBytes: cmp.Or(lr.EstMem, ar.MemBytes)}
			cpus, mem := size.Charge(est, ar.CPUNow, cmp.Or(ar.MemWork, ar.MemCurrent))
			dr := decide.Runner{Name: ar.Name, Class: ar.Class, Host: h.cfg.Name, CPUs: cpus, MemBytes: mem,
				State: lr.State, JobKnown: lr.JobID != "", CreatedAt: lr.CreatedAt, ListeningAt: lr.ListeningAt, Container: ar.State, Working: working, Lost: ar.Lost, JobStartedAt: lr.StartedAt}
			if ar.JobStartedAt != nil && ok {
				dr.JobStartedAt = *ar.JobStartedAt
			}
			if ar.ExitCode != nil {
				dr.ExitCode = *ar.ExitCode
			}
			out = append(out, dr)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, lr := range live {
		if seen[lr.Name] {
			delete(c.inflight, lr.Name)
			continue
		}
		dr := decide.Runner{Name: lr.Name, Class: lr.Class, Host: lr.Host, State: lr.State, JobKnown: lr.JobID != "",
			CreatedAt: lr.CreatedAt, ListeningAt: lr.ListeningAt, CPUs: lr.EstCPUs, MemBytes: lr.EstMem}
		if answered, ok := c.inflight[lr.Name]; ok {
			// Missing from a snapshot that was asked for before the agent answered
			// the start says nothing: the runner was not there to be listed yet.
			h := c.hostOf(lr.Host)
			if answered.IsZero() || h == nil || h.snap == nil || !h.askedAt.After(answered) {
				dr.Container = "starting"
			} else {
				delete(c.inflight, lr.Name)
			}
		}
		out = append(out, dr)
	}
	return out
}

// markerCheck names a runner that has a job according to GitHub while its own
// output, as the agent reads it, has not shown "Running job" for a minute: the
// agent's reading of the runner's log is late or failing (a host too busy to
// run it, a log that cannot be read), and until it shows, that runner's job is
// known from GitHub alone. Once per runner, and once more when the line shows.
func (c *Controller) markerCheck(h *host, lr ledger.Runner, ar api.Runner) {
	const late = time.Minute
	since, warned := c.noMarker[ar.Name]
	switch {
	case ar.JobStartedAt != nil:
		if warned {
			c.log.Info(fmt.Sprintf("runner %s: the agent on %s now sees the job in the runner's output, %s after GitHub assigned it",
				ar.Name, h.cfg.Name, time.Since(since).Round(time.Second)))
			delete(c.noMarker, ar.Name)
		}
	case warned || lr.JobID == "" || lr.StartedAt.IsZero() || ar.State != "running" || time.Since(lr.StartedAt) < late:
	default:
		if c.noMarker == nil {
			c.noMarker = map[string]time.Time{}
		}
		c.noMarker[ar.Name] = lr.StartedAt
		age := "never"
		if h.snap != nil && !h.snap.SampledAt.IsZero() {
			age = time.Since(h.snap.SampledAt).Round(time.Second).String() + " ago"
		}
		c.log.Warn(fmt.Sprintf("runner %s has run %s for %s according to GitHub, but the agent on %s has not seen \"Running job\" in the runner's output (agent sampled %s; its log on the host says why if reading failed)",
			ar.Name, lr.Job, time.Since(lr.StartedAt).Round(time.Second), h.cfg.Name, age))
		c.ev.Write("runner-marker-late", map[string]any{"runner": ar.Name, "host": h.cfg.Name, "jobAssignedAt": lr.StartedAt, "agentSampledAt": h.snap.SampledAt})
	}
}

func (c *Controller) recordUsage(r api.Runner) {
	psi := func(res string) *float64 {
		if v, ok := r.PressureSec[res]; ok {
			return &v
		}
		return nil
	}
	u := ledger.Usage{CPUSec: r.CPUSec, MemPeak: r.MemPeak, MemWorkPeak: r.MemWorkPeak, LimitMem: r.LimitMem, IOBytes: r.IOBytes, ExitCode: r.ExitCode,
		PSICPUSec: psi("cpu"), PSIMemSec: psi("memory"), PSIIOSec: psi("io"), OOMKills: r.OOMKills}
	if err := c.ledger.SetUsage(r.Name, u); err != nil {
		c.log.Error("ledger usage", "runner", r.Name, "err", err)
	}
}

func (c *Controller) decideSnapshot(now time.Time, runners []decide.Runner) decide.Snapshot {
	s := decide.Snapshot{Now: now, Runners: runners, PSICPUThreshold: c.cfg.PSICPUThreshold, PSIMemThreshold: c.cfg.PSIMemThreshold,
		HeadStart:        c.cfg.PriorityHeadStart,
		ProvisionTimeout: c.cfg.ProvisionTimeout, IdleTimeout: c.cfg.IdleTimeout, HostLostAfter: c.cfg.HostLostAfter}
	for _, h := range c.hosts {
		if h.snap != nil {
			h.os = cmp.Or(h.snap.OS, "linux")
		}
		dh := decide.Host{Name: h.cfg.Name, Kind: h.cfg.Kind(), OS: h.os, Up: h.snap != nil,
			BudgetCPUs: h.cfg.Budget.CPUs, BudgetMem: int64(h.cfg.Budget.Memory)}
		if h.snap == nil {
			dh.DownFor = now.Sub(h.downSince)
		} else {
			a := h.snap
			dh.CPUs, dh.MemBytes, dh.NonRunnerCPU, dh.NonRunnerMem, dh.PSICPU = a.CPUs, a.MemBytes, a.NonRunnerCPU, a.NonRunnerMem, a.PSI["cpu"]
			dh.PSIMem, dh.KeepMem = a.PSI["memory"], keepMem(a.MemBytes)
			dh.BudgetCPUs = minPositive(dh.BudgetCPUs, a.BudgetCPUs)
			dh.BudgetMem = int64(minPositive(float64(dh.BudgetMem), float64(a.BudgetMem)))
			if a.Kind != "" {
				dh.Kind = a.Kind // a host that joined says itself what it is
			}
			h.closed = c.closedReason(h)
			dh.Closed, dh.NoNative, dh.Paths = h.closed, a.NoNative, a.Paths
		}
		s.Hosts = append(s.Hosts, dh)
	}

	open, err := c.ledger.OpenDemand()
	if err != nil {
		c.log.Error("ledger demand", "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	perClass := map[string]int{}
	c.sized = map[string]size.Estimate{}
	for _, d := range open {
		dd := decide.Demand{ID: d.JobID, Class: d.Class, FirstSeen: d.FirstSeen, Name: d.Job}
		dd.Level, dd.Ranked = c.cfg.LevelOf(config.WaitingJob{Class: d.Class, Repo: d.Repo, WorkflowRef: d.Workflow, Name: d.Job, Event: d.Event})
		if cl := c.classByName(d.Class); cl != nil {
			est := c.estimate(cl.cfg, ledger.KeyOf(d.Repo, d.Workflow, d.Job))
			c.sized[d.JobID] = est
			dd.CPUs, dd.MemBytes, dd.Sized = est.CPUs, est.MemBytes, "("+est.Source()+")"
			if d.Job != "" {
				dd.Sized = "for " + d.Job + " " + dd.Sized
			}
		}
		s.Demand = append(s.Demand, dd)
		perClass[d.Class]++
	}
	for _, cl := range c.classes {
		s.Classes = append(s.Classes, decide.Class{Name: cl.cfg.Name, OS: cl.cfg.OS, CPUs: cl.cfg.CPUs, MemBytes: int64(cl.cfg.Memory),
			Hosts: cl.cfg.Hosts, MaxDuration: cl.cfg.MaxDuration,
			Native: cl.cfg.Driver == "native", Paths: mountPaths(cl.cfg.Mounts), Reserve: cl.cfg.Reserve})
		// Jobs the statistics count but the ledger never saw (for example after a fresh ledger).
		if cl.stats != nil {
			for i := perClass[cl.cfg.Name]; i < cl.stats.TotalAssignedJobs-cl.stats.TotalRunningJobs; i++ {
				s.Demand = append(s.Demand, decide.Demand{ID: fmt.Sprintf("stats-%d", i), Class: cl.cfg.Name, FirstSeen: now})
			}
		}
	}
	return s
}

// reloadPriority takes the config file's priority rules again when the file
// has changed, so the line can be reordered without stopping `up`. Nothing
// else of the file is read again. A file that does not load keeps the rules
// in force and says so once.
func (c *Controller) reloadPriority() {
	fi, err := os.Stat(c.cfg.Path)
	if c.cfg.Path == "" || err != nil || fi.ModTime().Equal(c.cfgRead) {
		return
	}
	first := c.cfgRead.IsZero()
	c.cfgRead = fi.ModTime()
	if first {
		return // the config `up` started with
	}
	fresh, err := config.Load(c.cfg.Path)
	if err != nil {
		c.log.Warn("the config changed but does not load; the priority rules stay as they were", "err", err)
		return
	}
	if reflect.DeepEqual(fresh.Priority, c.cfg.Priority) && fresh.PriorityHeadStart == c.cfg.PriorityHeadStart {
		return
	}
	c.mu.Lock()
	c.cfg.Priority, c.cfg.PriorityHeadStart = fresh.Priority, fresh.PriorityHeadStart
	c.mu.Unlock()
	c.log.Info(fmt.Sprintf("priority: %d rules read again from %s (head start %s); they order the jobs waiting now", len(fresh.Priority), c.cfg.Path, fresh.PriorityHeadStart))
	c.decision(c.now(), "", "", "priority", "", fmt.Sprintf("%d rules, head start %s", len(fresh.Priority), fresh.PriorityHeadStart))
}

// estimate is what a job is expected to use: learned from its last measured
// runs, or the class's first estimate while there are none.
func (c *Controller) estimate(cl config.Class, k ledger.JobKey) size.Estimate {
	first := size.Estimate{CPUs: cl.CPUs, MemBytes: int64(cl.Memory)}
	if k.Job == "" {
		return first
	}
	runs, err := c.ledger.JobRuns(k, size.Window)
	if err != nil {
		c.log.Error("ledger job runs", "err", err)
		return first
	}
	return size.Learn(runs, first)
}

// keepMem is what stays free on a machine whatever else is true: a tenth of
// its memory, between 1G and 4G.
func keepMem(total int64) int64 { return min(max(total/10, 1<<30), 4<<30) }

// limits are what a runner of the class is held to on a host. The class's
// own limit when it has one. Otherwise no cpu limit (a busy cpu slows jobs,
// it breaks nothing) and a memory limit that is not about the job at all: it
// keeps one runaway job from taking the machine down with every other job on
// it. Four fifths of the machine, or the host's budget when that is less.
func limits(cl config.Class, budgetMem, hostMem int64) (cpus float64, mem int64) {
	cpus, mem = cl.Limit.CPUs, int64(cl.Limit.Memory)
	if mem == 0 {
		mem = int64(minPositive(float64(hostMem/5*4), float64(budgetMem)))
	}
	return cpus, mem
}

func mountPaths(mounts []string) []string {
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = config.MountPath(m)
	}
	return out
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

// hostOf is hostByName for the evaluating goroutine, which owns c.hosts.
func (c *Controller) hostOf(name string) *host {
	for _, h := range c.hosts {
		if h.cfg.Name == name {
			return h
		}
	}
	return nil
}

func (c *Controller) hostByName(name string) *host {
	c.hostsMu.Lock()
	hosts := c.hosts
	c.hostsMu.Unlock()
	for _, h := range hosts {
		if h.cfg.Name == name {
			return h
		}
	}
	return nil
}

// stop removes the container (when the agent answers), stores its final values and the new state.
func (c *Controller) stop(ctx context.Context, st decide.Stop, now time.Time) {
	before, _ := c.ledger.Runner(st.Runner)
	h := c.hostByName(st.Host)
	// maxDuration is the one stop that is meant to end a job; a completed job
	// has nothing left to lose.
	if h != nil && before.State != "done" && !strings.HasPrefix(st.Reason, decide.OverMaxDuration) &&
		alive(h.snap, st.Runner) && !c.release(ctx, st.Runner, before, st.Reason) {
		return
	}
	c.log.Info(fmt.Sprintf("stop %s  %s", st.Runner, st.Reason))
	c.decision(now, "", st.Host, "stop", st.Runner, st.Reason)
	c.mu.Lock()
	delete(c.inflight, st.Runner)
	c.mu.Unlock()
	var final *api.Runner
	removed := false
	if h != nil && h.snap != nil {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		f, err := h.agent.Delete(cctx, st.Runner)
		cancel()
		if err != nil {
			c.log.Error("delete runner", "runner", st.Runner, "err", err)
		} else {
			removed = true
			if f.State != "unknown" {
				final = &f
				c.recordUsage(f)
				if f.OOMKills > 0 && before.OOMKills == 0 && before.Name != "" {
					c.reportOOM(before, f)
				}
			}
			if f.LogTail != "" {
				_ = c.ledger.SetLogTail(st.Runner, f.LogTail)
			}
			if f.Leftover != "" {
				c.book.report("problem", "leftover/"+st.Runner, "leftover", st.Runner, fmt.Sprintf(
					"%s on %s is removed, but the host may not delete all of its files: %s", st.Runner, st.Host, f.Leftover),
					"none: delete it on the host (usually files a job wrote as root through a container)")
			}
			// If JobCompleted was missed while the controller was away, the exit code
			// is all there is; a late message still replaces it.
			if f.JobStartedAt != nil && f.ExitCode != nil && f.ExitedAt != nil {
				if err := c.ledger.CloseFromExit(st.Runner, *f.ExitCode, *f.ExitedAt); err != nil {
					c.log.Error("ledger close", "err", err)
				}
			}
		}
	}
	if err := c.ledger.SetState(st.Runner, st.State, st.Reason); err != nil {
		c.log.Error("ledger state", "err", err)
	}
	if h != nil && (strings.HasPrefix(st.Reason, decide.ProvisionTimeout) || strings.HasPrefix(st.Reason, decide.ExitedBeforeJob)) {
		c.provisionFailed(h, st.Reason)
	}
	switch endCause(before, st, final) {
	case endRebooted:
		c.rebootLost[st.Host] = append(c.rebootLost[st.Host], before)
	case endMaxDuration:
		c.book.report("problem", "max-duration/"+st.Runner, "max-duration", st.Runner, fmt.Sprintf(
			"%s on %s ran longer than the %s maxDuration; the runner was stopped and GitHub will fail the job",
			jobName(before), st.Host, before.Class), "runner stopped; not rerun")
	case endDied:
		code := "unknown"
		if final != nil && final.ExitCode != nil {
			code = fmt.Sprint(*final.ExitCode)
		}
		action := "container removed; registration is removed when GitHub lets go"
		if a := c.requestRerun(before, fmt.Sprintf("runner %s died on %s", st.Runner, st.Host)); a != "" {
			action += "; " + a
		}
		c.book.report("problem", "runner-died/"+st.Runner, "runner-died", st.Runner, fmt.Sprintf(
			"runner %s died while running %s on %s (exit code %s); GitHub will fail that job", st.Runner, jobName(before), st.Host, code), action)
	}
	if removed {
		if err := c.ledger.MarkRemoved(st.Runner, now); err != nil {
			c.log.Error("ledger removed", "err", err)
		}
		if final != nil {
			c.learned(before, *final, now) // its last sample is in the ledger now
		}
	}
}

// alive: the host's last snapshot shows the runner up, so it may hold a job.
func alive(s *api.Snapshot, runner string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.Runners {
		if r.Name == runner {
			return r.Lost == "" && r.State != "exited" && r.State != "gone"
		}
	}
	return false
}

// release asks GitHub to drop a runner's registration and reports whether its
// container may be removed. GitHub refuses while the runner holds a job, and
// that refusal is the one word on the matter that cannot be late: the agent
// reads the runner's output every few seconds and JobStarted arrives half a
// minute after the fact, so a runner that looks idle here may have just been
// handed a job. Every removal of a runner that is up asks first; only
// maxDuration does not. When GitHub cannot be asked, the runner stays for
// this round.
func (c *Controller) release(ctx context.Context, name string, lr ledger.Runner, why string) bool {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id := lr.RunnerID
	if id == 0 {
		ref, err := c.gh.GetRunnerByName(cctx, name)
		if err != nil {
			c.log.Warn(fmt.Sprintf("runner %s left alone (%s): GitHub could not be asked whether it holds a job", name, why), "err", err)
			return false
		}
		if ref == nil {
			return true // never registered, or gone already: no job can be on it
		}
		id = int64(ref.ID)
	}
	err := c.gh.RemoveRunner(cctx, id)
	switch {
	case err == nil || errors.Is(err, scaleset.RunnerNotFoundError):
		if lr.Name != "" {
			if err := c.ledger.MarkUnregistered(name); err != nil {
				c.log.Error("ledger unregistered", "err", err)
			}
		}
		return true
	case errors.Is(err, scaleset.JobStillRunningError):
		c.log.Info(fmt.Sprintf("busy %s  GitHub has a job on it; not stopped (%s)", name, why))
		c.ev.Write("runner-busy", map[string]any{"runner": name, "from": "github", "insteadOf": why})
		if lr.Name != "" {
			if err := c.ledger.HoldsJob(name, c.now()); err != nil {
				c.log.Error("ledger busy", "err", err)
			}
		}
		return false
	}
	c.log.Warn(fmt.Sprintf("runner %s left alone (%s): GitHub could not be asked whether it holds a job", name, why), "err", err)
	return false
}

// start records the runner and launches it in the background.
func (c *Controller) start(ctx context.Context, s decide.Start, now time.Time) {
	name := fmt.Sprintf("%s-%s-%s", s.Class, s.Host, uuid.NewString()[:8])
	c.log.Info(fmt.Sprintf("start %s on %s  %s  (%s)", s.Class, s.Host, s.Reason, name))
	c.decision(now, s.Class, s.Host, "start", name, s.Reason)
	c.mu.Lock()
	est, ok := c.sized[s.Job]
	c.mu.Unlock()
	if !ok {
		est = size.Estimate{CPUs: s.CPUs, MemBytes: s.MemBytes}
	}
	if err := c.ledger.InsertRunner(name, s.Class, s.Host, now, est); err != nil {
		c.log.Error("ledger insert", "err", err)
		return
	}
	var limCPUs float64
	var limMem int64
	if h, cl := c.hostOf(s.Host), c.classByName(s.Class); h != nil && cl != nil && h.snap != nil {
		limCPUs, limMem = limits(cl.cfg, int64(minPositive(float64(h.cfg.Budget.Memory), float64(h.snap.BudgetMem))), h.snap.MemBytes)
	}
	c.mu.Lock()
	c.inflight[name] = time.Time{}
	c.mu.Unlock()
	c.starts.Add(1)
	go func() {
		defer c.starts.Done()
		answered := c.launch(ctx, name, s, now, limCPUs, limMem)
		c.mu.Lock()
		if _, ok := c.inflight[name]; ok && answered {
			c.inflight[name] = time.Now()
		}
		c.mu.Unlock()
	}()
}

// launch registers the runner and asks the host's agent to start it. It
// reports whether the outcome is known. When the agent did not answer (a
// timeout, a connection that broke), the runner may still come up: its row
// stays "starting" and the provision timeout closes it if it never shows.
func (c *Controller) launch(ctx context.Context, name string, s decide.Start, now time.Time, limCPUs float64, limMem int64) bool {
	cl := c.classByName(s.Class)
	h := c.hostByName(s.Host)
	fail := func(reason string) bool {
		c.log.Error("start failed", "runner", name, "reason", reason)
		if err := errors.Join(c.ledger.SetState(name, "failed", reason), c.ledger.MarkRemoved(name, c.now())); err != nil {
			c.log.Error("ledger start failed", "runner", name, "err", err)
		}
		return true
	}
	if h == nil || cl == nil {
		return fail(fmt.Sprintf("host %s left before the runner was started", s.Host))
	}
	workDir := cl.cfg.WorkDir(name)
	jit, err := c.gh.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: workDir}, cl.setID)
	if err != nil {
		return fail("jit: " + err.Error())
	}
	posted := c.now()
	if err := c.ledger.SetRegistration(name, int64(jit.Runner.ID), posted); err != nil {
		c.log.Error("ledger registration", "err", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, err = h.agent.Start(cctx, api.StartRequest{Name: name, Class: s.Class, CPUs: cmp.Or(s.CPUs, cl.cfg.CPUs),
		MemBytes: cmp.Or(s.MemBytes, int64(cl.cfg.Memory)), LimitCPUs: limCPUs, LimitMem: limMem, Image: c.cfg.Image, JITConfig: jit.EncodedJITConfig,
		Driver: cl.cfg.Driver, Mounts: cl.cfg.Mounts, WorkDir: workDir, InnerDocker: cl.cfg.Docker == "inside",
		Env: cl.cfg.Env, Path: cl.cfg.Path})
	var se *api.StatusError
	switch {
	case err == nil:
		c.ev.Write("runner-started", map[string]any{"runner": name, "host": s.Host, "class": s.Class,
			"jitMs": posted.Sub(now).Milliseconds(), "postMs": c.now().Sub(posted).Milliseconds()})
	case errors.As(err, &se):
		// The agent answered and could not start it. The registration stays until
		// unregister removes it. Over budget (409) is not the host's doing.
		fail("agent start: " + err.Error())
		if se.Code != http.StatusConflict {
			c.provisionFailed(h, err.Error())
		}
	default:
		c.log.Warn(fmt.Sprintf("start of %s on %s not answered; it counts as starting until it shows or the provision timeout passes", name, s.Host), "err", err)
		return false
	}
	return true
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

// githubREST calls api.github.com from inside a round: an answer that never
// comes must not hold the round.
var githubREST = &http.Client{Timeout: 30 * time.Second}

func (c *Controller) runStatus(ctx context.Context, repo string, runID int64) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d", repo, runID), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubREST.Do(req)
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

// startAgents makes every host of the config join, in parallel: one that is
// a member already is waited for, any other is bootstrapped over ssh. Hosts
// that joined by themselves are waited for and nothing else. A host
// that fails is reported and left out; the run goes on while one host works
// or others have joined by themselves.
func (c *Controller) startAgents(ctx context.Context) error {
	hosts := c.hosts
	bad := make([]string, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.cfg.SSH == "" {
				// A host that joined by itself comes back by itself. Waiting for it
				// here lets the first round see the runners it still has.
				if err := h.wait(ctx); err != nil && ctx.Err() == nil {
					c.log.Warn(fmt.Sprintf("host %s has not connected yet; its running jobs are left alone", h.cfg.Name))
				}
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			did := "connected"
			if h.wait(cctx) != nil || c.needsBootstrap(h) {
				did = "joined over ssh"
				err := c.bootstrap(cctx, h.cfg)
				for try := 0; err != nil && try < 2 && cctx.Err() == nil && !strings.Contains(err.Error(), "cannot reach this controller"); try++ {
					// ssh hops fail now and then (seen: a WSL host timing out once); joining again is harmless.
					c.log.Warn(fmt.Sprintf("host %s: retrying", h.cfg.Name), "err", err)
					time.Sleep(3 * time.Second)
					err = c.bootstrap(cctx, h.cfg)
				}
				if err != nil {
					bad[i] = h.cfg.Name
					c.log.Error(fmt.Sprintf("host %s not usable", h.cfg.Name), "err", err)
					return
				}
			}
			c.log.Info(fmt.Sprintf("host %s  %s  %s", h.cfg.Name, did, lends(h.cfg.Budget.CPUs, int64(h.cfg.Budget.Memory))))
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	bad = slices.DeleteFunc(bad, func(n string) bool { return n == "" })
	c.syncJoined(false)
	if len(bad) == 0 {
		return nil
	}
	if len(bad) == len(c.hosts) {
		return errors.New("no usable host")
	}
	c.log.Warn(fmt.Sprintf("%d of %d hosts usable; left out for now and tried again every minute: %s", len(c.hosts)-len(bad), len(c.hosts), strings.Join(bad, ", ")))
	return nil
}

// reviveAgent runs the invite line again on a host of the config that has not
// answered for a while: its agent is dead, or the host rebooted where nothing
// starts the agent by itself. Joining again never touches runners. A host that
// joined by itself has to come back by itself.
func (c *Controller) reviveAgent(ctx context.Context, h *host, now time.Time) {
	if h.cfg.SSH == "" || now.Sub(h.downSince) < 20*time.Second || now.Sub(h.ensuredAt) < time.Minute || !h.ensuring.CompareAndSwap(false, true) {
		return
	}
	h.ensuredAt = now
	go func() {
		defer h.ensuring.Store(false)
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if h.wait(cctx) == nil {
			return // it dialed in again meanwhile
		}
		if err := c.bootstrap(cctx, h.cfg); err != nil {
			c.log.Warn(fmt.Sprintf("host %s: agent not restarted", h.cfg.Name), "err", err)
			if h.reviveBad.Add(1) == 3 {
				c.book.open("agent-down/"+h.cfg.Name, "agent-down", h.cfg.Name, fmt.Sprintf(
					"the agent on %s could not be restarted 3 times (%s); its running jobs go on, nothing new is placed there", h.cfg.Name, firstLine(err.Error())))
			}
			return
		}
		h.reviveBad.Store(0)
		c.book.report("recovered", "agent-restarted/"+h.cfg.Name, "agent-restarted", h.cfg.Name,
			fmt.Sprintf("the agent on %s had stopped and was started again; runners were not touched", h.cfg.Name), "agent restarted")
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}()
}

// rebootReason is the agent's word for a runner whose host rebooted under it.
const rebootReason = api.LostRebooted

// reportReboots tells of each rebooted host once, with the jobs it took down.
func (c *Controller) reportReboots(now time.Time) {
	for host, rs := range c.rebootLost {
		var jobs []string
		for _, r := range rs {
			if r.State == "busy" {
				jobs = append(jobs, jobName(r))
			}
		}
		detail := fmt.Sprintf("host %s rebooted: %d runners lost, no job was running", host, len(rs))
		if len(jobs) > 0 {
			detail = fmt.Sprintf("host %s rebooted: %d runners lost with %d running jobs (%s); GitHub will fail those jobs",
				host, len(rs), len(jobs), strings.Join(jobs, ", "))
		}
		action := "runners closed as lost, registrations removed"
		for _, r := range rs {
			if r.State == "busy" && r.Result == "" {
				if a := c.requestRerun(r, fmt.Sprintf("host %s rebooted under %s", host, jobName(r))); a != "" && !strings.Contains(action, a) {
					action += "; " + a
				}
			}
		}
		c.book.report("problem", fmt.Sprintf("host-rebooted/%s/%d", host, now.Unix()), "host-rebooted", host, detail, action)
	}
	clear(c.rebootLost)
}

func jobName(r ledger.Runner) string {
	if r.Job == "" {
		return "a job on " + r.Name
	}
	return r.Job
}

func (c *Controller) aliveFile() string { return filepath.Join(c.cfg.StateDir(), "alive") }

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// hostIncidents opens an incident for a host that has been unreachable for
// HostLostAfter and resolves it (and a failed agent restart) when it answers.
func (c *Controller) hostIncidents(now time.Time, live []ledger.Runner) {
	for _, h := range c.hosts {
		name := h.cfg.Name
		if h.snap != nil {
			c.book.resolve("host-unreachable/" + name)
			c.book.resolve("agent-down/" + name)
			continue
		}
		down := now.Sub(h.downSince)
		if h.downSince.IsZero() || down < c.cfg.HostLostAfter {
			continue
		}
		jobs := 0
		for _, r := range live {
			if r.Host == name && r.State == "busy" {
				jobs++
			}
		}
		why := ""
		if w := h.why(); w != "" {
			why = " (" + w + ")"
		}
		c.book.open("host-unreachable/"+name, "host-unreachable", name, fmt.Sprintf(
			"%s unreachable for %s%s — %d jobs still running there, no new runners placed", name, down.Round(time.Second), why, jobs))
	}
}

// waitIncidents opens an incident per class whose oldest unplaced job has
// waited longer than WaitAlert, with the reason it cannot be placed.
func (c *Controller) waitIncidents(defers []decide.Defer, now time.Time) {
	open, err := c.ledger.OpenDemand() // oldest first
	if err != nil {
		return
	}
	late := map[string]bool{}
	for _, d := range defers {
		var ofClass []ledger.Demand
		for _, o := range open {
			if o.Class == d.Class {
				ofClass = append(ofClass, o)
			}
		}
		// The deferred jobs are the newest d.Count of the class.
		if d.Count > len(ofClass) {
			continue
		}
		waited := now.Sub(ofClass[len(ofClass)-d.Count].FirstSeen)
		if waited < c.cfg.WaitAlert {
			continue
		}
		late[d.Class] = true
		c.book.open("waiting/"+d.Class, "job-waiting", d.Class, fmt.Sprintf(
			"%d %s jobs waiting, the oldest for %s — %s", d.Count, d.Class, waited.Round(time.Second), d.Reason))
	}
	for _, cl := range c.classes {
		if !late[cl.cfg.Name] {
			c.book.resolve("waiting/" + cl.cfg.Name)
		}
	}
}

func gib(b int64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }

// shutdown runs after Ctrl-C, on its own deadline. A controller going away must
// never cost a running job: agents and busy runners stay, and the next
// `pumasi up` adopts them. Only runners without a job are removed.
func (c *Controller) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	defer c.Close()
	c.refreshHosts(ctx, c.now())
	live, err := c.ledger.LiveRunners()
	if err != nil {
		// Without the ledger nothing says which runner has a job: all stay.
		c.log.Error("stopped  the ledger could not be read, so no runner was removed; `pumasi up` picks them up again", "err", err)
		return
	}
	removed, running := 0, 0
	byName := map[string]ledger.Runner{}
	for _, r := range live {
		byName[r.Name] = r
	}
	for _, h := range c.hosts {
		if h.snap == nil {
			for _, r := range live {
				if r.Host == h.cfg.Name && r.State == "busy" {
					running++
				}
			}
			continue
		}
		for _, r := range h.snap.Runners {
			lr, known := byName[r.Name]
			if !known {
				lr, _ = c.ledger.Runner(r.Name) // closed earlier: its registration id is still of use
			}
			// A job is known from the runner's own output or from GitHub's JobStarted
			// message, whichever came first; a runner that shows neither is removed
			// only when GitHub lets go of its registration.
			if r.JobStartedAt != nil || lr.State == "busy" || (alive(h.snap, r.Name) && !c.release(ctx, r.Name, lr, "controller stopped")) {
				// Running a job, or finished one whose numbers the next `up` collects.
				if r.State == "running" {
					running++
				}
				continue
			}
			if _, err := h.agent.Delete(ctx, r.Name); err != nil {
				c.log.Error("remove runner", "runner", r.Name, "err", err)
				continue
			}
			if err := errors.Join(c.ledger.SetState(r.Name, "", "controller stopped"), c.ledger.MarkRemoved(r.Name, c.now())); err != nil {
				c.log.Error("ledger removed", "runner", r.Name, "err", err)
			}
			removed++
		}
	}
	c.unregister(ctx)
	if running > 0 {
		c.log.Info(fmt.Sprintf("stopped  %d jobs still running on the hosts; `pumasi up` picks them up again (%d idle runners removed)", running, removed))
		return
	}
	c.log.Info(fmt.Sprintf("stopped  no jobs running, %d idle runners removed; agents stay up, `pumasi down` removes everything", removed))
}

// forgetLiveRunners marks runners whose container we could not see removed as lost.
func (c *Controller) forgetLiveRunners(reason string) {
	live, err := c.ledger.LiveRunners()
	if err != nil {
		c.log.Error("ledger runners", "err", err)
		return
	}
	for _, r := range live {
		if err := c.ledger.SetState(r.Name, "lost", reason); err != nil {
			c.log.Error("ledger state", "runner", r.Name, "err", err)
		}
	}
}

// Down removes everything pumasi left behind: on every host the agent, its
// runners and its files; on GitHub the remaining registrations and the
// classes' scale sets. A connected host is asked to remove itself; a host of
// the config that is not connected is told the same over ssh. Runners that
// are still up may be running jobs, so Down refuses while there are any
// unless force is set.
func (c *Controller) Down(ctx context.Context, force bool) error {
	if err := c.listenForHosts(""); err != nil {
		return err
	}
	return c.down(ctx, force)
}

func (c *Controller) down(ctx context.Context, force bool) error {
	// Hosts that joined dial in again within their backoff.
	var wg sync.WaitGroup
	for _, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.wait(ctx)
		}()
	}
	wg.Wait()
	if !force {
		if err := c.refuseIfRunning(ctx); err != nil {
			return err
		}
	}
	errs := make([]error, len(c.hosts))
	for i, h := range c.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			var did string
			var err error
			switch {
			case h.connected():
				var n string
				if n, err = h.leave(cctx); err == nil {
					did = fmt.Sprintf("agent stopped, %s runners removed, pumasi's files removed", n)
				}
			case h.cfg.SSH != "":
				did, err = leaveOverSSH(cctx, h.cfg, force)
			default:
				err = errors.New("the host is not connected; run `pumasi leave` on that host to remove what is there")
			}
			// Down forgets the host either way; what could not be removed is the host's side.
			if h.id != "" {
				c.join.Forget(h.id)
			}
			if err != nil {
				errs[i] = fmt.Errorf("host %s not cleaned: %w", h.cfg.Name, err)
				c.log.Error(fmt.Sprintf("host %s not cleaned", h.cfg.Name), "err", err)
				return
			}
			c.log.Info(fmt.Sprintf("host %s  %s", h.cfg.Name, did))
		}()
	}
	wg.Wait()
	c.forgetLiveRunners("down")
	c.unregister(ctx)
	// Jobs still waiting have nowhere to go once the scale sets are gone.
	if err := c.ledger.CloseOpenDemand("down", c.now()); err != nil {
		c.log.Error("ledger demand", "err", err)
	}
	for _, cl := range c.cfg.Classes {
		set, err := c.gh.GetRunnerScaleSet(ctx, runnerGroup, cl.ScaleSet())
		if err != nil || set == nil {
			continue
		}
		if err := c.gh.DeleteRunnerScaleSet(ctx, set.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete scale set %s: %w", cl.ScaleSet(), err))
			continue
		}
		c.ev.Write("scale-set-deleted", set.ID)
		c.log.Info(fmt.Sprintf("class %s  scale set %s deleted", cl.Name, cl.ScaleSet()))
	}
	return errors.Join(errs...)
}

// refuseIfRunning asks every host that has joined for its running runners. A
// host of the config that never joined has none of this controller's.
func (c *Controller) refuseIfRunning(ctx context.Context) error {
	lines := make([][]string, len(c.hosts))
	errs := make([]error, len(c.hosts))
	var wg sync.WaitGroup
	for i, h := range c.hosts {
		if h.id == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			names, err := h.running(cctx)
			if err != nil {
				errs[i] = fmt.Errorf("cannot check %s for running jobs: %w", h.cfg.Name, err)
				return
			}
			for _, n := range names {
				what := "state unknown to the ledger"
				if r, err := c.ledger.Runner(n); err == nil {
					what = r.State
					if r.Job != "" {
						what += ", job " + r.Job
					}
				}
				lines[i] = append(lines[i], fmt.Sprintf("  %s on %s (%s)", n, h.cfg.Name, what))
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w\nnothing was removed; `pumasi down --force` skips this check", err)
	}
	all := slices.Concat(lines...)
	if len(all) == 0 {
		return nil
	}
	return fmt.Errorf("%d runners are still up and may be running jobs:\n%s\nnothing was removed; `pumasi down --force` stops them too",
		len(all), strings.Join(all, "\n"))
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

func cond(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// lends says what a host lends: its cap, or what it measures free.
func lends(cpus float64, mem int64) string {
	if cpus <= 0 && mem <= 0 {
		return "lends what it has free"
	}
	return "lends at most " + cond(cpus > 0, fmt.Sprintf("%gcpu", cpus), "any cpu") + "/" + cond(mem > 0, gib(mem), "any memory")
}
