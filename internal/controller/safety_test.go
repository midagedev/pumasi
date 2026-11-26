package controller

import (
	"context"
	"errors"
	"fmt"
	"github.com/midagedev/pumasi/internal/size"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/cgroup"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
)

// fakeGitHub is the scale set service as far as runners go: registrations, and
// which of them holds a job. Like GitHub it refuses to drop one that does.
type fakeGitHub struct {
	mu      sync.Mutex
	nextID  int
	ids     map[string]int64
	holds   map[string]bool
	down    bool     // every call fails
	dropped []string // registrations removed, by runner name
}

func (g *fakeGitHub) register(name string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ids == nil {
		g.ids, g.holds = map[string]int64{}, map[string]bool{}
	}
	g.nextID++
	g.ids[name] = int64(g.nextID)
	return int64(g.nextID)
}

func (g *fakeGitHub) assign(name string, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holds[name] = held
}

func (g *fakeGitHub) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	return &scaleset.RunnerScaleSet{ID: 1}, nil
}
func (g *fakeGitHub) CreateRunnerScaleSet(_ context.Context, s *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return s, nil
}
func (g *fakeGitHub) DeleteRunnerScaleSet(context.Context, int) error { return nil }
func (g *fakeGitHub) MessageSessionClient(context.Context, int, string, ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error) {
	return nil, errors.New("no sessions in tests")
}
func (g *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if g.down {
		return nil, errors.New("github is down")
	}
	id := g.register(s.Name)
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: int(id), Name: s.Name}, EncodedJITConfig: "jit"}, nil
}
func (g *fakeGitHub) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return nil, errors.New("github is down")
	}
	if id, ok := g.ids[name]; ok {
		return &scaleset.RunnerReference{ID: int(id), Name: name}, nil
	}
	return nil, nil
}
func (g *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return errors.New("github is down")
	}
	for name, have := range g.ids {
		if have != id {
			continue
		}
		if g.holds[name] {
			return fmt.Errorf("delete runner: %w", scaleset.JobStillRunningError)
		}
		delete(g.ids, name)
		g.dropped = append(g.dropped, name)
		return nil
	}
	return fmt.Errorf("delete runner: %w", scaleset.RunnerNotFoundError)
}

// fakeDriver stands in for docker on a host and remembers what was removed.
type fakeDriver struct {
	mu        sync.Mutex
	cs        map[string]agent.Container
	ev        map[string]agent.LogEvents
	removed   []string
	removeErr error
	specs     map[string]agent.Spec
}

func (d *fakeDriver) Start(_ context.Context, s agent.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.specs == nil {
		d.specs = map[string]agent.Spec{}
	}
	d.specs[s.Name] = s
	d.cs[s.Name] = agent.Container{Name: s.Name, Class: s.Class, CPUs: s.CPUs, MemBytes: s.MemBytes, State: "running"}
	return nil
}
func (d *fakeDriver) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.removeErr != nil {
		return d.removeErr
	}
	delete(d.cs, name)
	d.removed = append(d.removed, name)
	return nil
}
func (d *fakeDriver) List(context.Context) ([]agent.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []agent.Container
	for _, c := range d.cs {
		out = append(out, c)
	}
	return out, nil
}
func (d *fakeDriver) LogEvents(_ context.Context, name string, _ time.Time) (agent.LogEvents, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ev[name], nil
}
func (d *fakeDriver) LogTail(context.Context, string, int) string { return "" }
func (d *fakeDriver) DataDir(context.Context) string              { return "" }
func (d *fakeDriver) Check(context.Context, string) error         { return nil }

func (d *fakeDriver) wasRemoved(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.removed, name)
}

// rig is a controller with one host whose agent is the real one over a fake
// driver, and a fake GitHub. Nothing in it sleeps: the test moves the clock.
type rig struct {
	*Controller
	t   *testing.T
	now *time.Time
	gh  *fakeGitHub
	drv *fakeDriver
	ag  *agent.Agent
	// answer decides how the host answers one agent request (nil = the agent does).
	answer atomic.Pointer[func(w http.ResponseWriter, r *http.Request) bool]
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c, now, _ := testBook(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{Controller: c, t: t, now: now, gh: &fakeGitHub{ids: map[string]int64{}, holds: map[string]bool{}},
		drv: &fakeDriver{cs: map[string]agent.Container{}, ev: map[string]agent.LogEvents{}}}
	cg := cgroup.Reader{CgroupRoot: filepath.Join("..", "cgroup", "testdata", "cg"),
		ProcRoot: filepath.Join("..", "cgroup", "testdata", "proc"), SysRoot: filepath.Join("..", "cgroup", "testdata", "sys")}
	r.ag = agent.New(agent.Config{Host: "h1", Version: "test"}, r.drv, &agent.CgroupMeter{R: cg}, log)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if f := r.answer.Load(); f != nil && (*f)(w, q) {
			return
		}
		r.ag.Handler().ServeHTTP(w, q)
	}))
	t.Cleanup(srv.Close)

	c.cfg.Ledger = filepath.Join(t.TempDir(), "pumasi.db")
	c.cfg.ProvisionTimeout, c.cfg.IdleTimeout, c.cfg.HostLostAfter = 3*time.Minute, 5*time.Minute, 2*time.Minute
	c.cfg.DemandTTL, c.cfg.WaitAlert, c.cfg.DiskMinFree = 24*time.Hour, 10*time.Minute, 10
	c.cfg.Image = "runner:1"
	c.cfg.Classes = []config.Class{{Name: "small", OS: "linux", Driver: "docker", CPUs: 1, Memory: 1 << 30, MaxDuration: time.Hour}}
	c.gh = r.gh
	c.kick = make(chan struct{}, 1)
	c.inflight, c.lastDefer, c.runCheck = map[string]time.Time{}, map[string]deferRec{}, map[int64]time.Time{}
	c.rebootLost, c.rerunCheck, c.rerunWanted = nil, map[int64]time.Time{}, map[string]string{}
	c.classes = []*class{{cfg: c.cfg.Classes[0], setID: 1}}
	hc := config.Host{Name: "h1"}
	hc.Budget.CPUs, hc.Budget.Memory = 4, 8<<30
	c.hosts = []*host{{cfg: hc,
		agent: &api.Client{BaseURL: func(context.Context) (string, error) { return srv.URL, nil }, HTTP: srv.Client()}}}
	return r
}

// runner places a runner the way start and launch do and lets the agent see it.
func (r *rig) runner(name string) {
	r.t.Helper()
	if err := r.ledger.InsertRunner(name, "small", "linux/docker", "h1", *r.now, size.Estimate{}); err != nil {
		r.t.Fatal(err)
	}
	if !r.launch(context.Background(), name, decide.Start{Class: "small", Host: "h1"}, *r.now, 0, 0) {
		r.t.Fatal("launch did not finish")
	}
	r.ag.Sample(context.Background())
}

func (r *rig) listening(name string) {
	r.drv.mu.Lock()
	r.drv.ev[name] = agent.LogEvents{Listening: *r.now}
	r.drv.mu.Unlock()
}

// round is one evaluation after d has passed.
func (r *rig) round(d time.Duration) {
	*r.now = r.now.Add(d)
	r.ag.Sample(context.Background())
	r.Evaluate(context.Background())
}

func (r *rig) state(name string) string {
	lr, err := r.ledger.Runner(name)
	if err != nil {
		return "absent"
	}
	if !lr.RemovedAt.IsZero() {
		return lr.State + "+removed"
	}
	return lr.State
}

// A runner that GitHub has handed a job is not removed, whatever the
// controller believes about it: the agent has not read "Running job" yet and
// JobStarted has not arrived, so to the ledger it is idle, starting or
// unknown. Each row is one way a runner gets removed; it runs twice, with and
// without a job on the runner, so the row also proves that the path removes.
func TestNoPathRemovesARunnerThatHoldsAJob(t *testing.T) {
	const name = "small-h1-aaaa"
	idle := func(r *rig) {
		r.runner(name)
		r.listening(name)
		r.round(0)
	}
	rows := []struct {
		path  string
		setup func(r *rig)
		act   func(r *rig)
		// always: the outcome does not depend on the job ("kept" or "removed").
		always string
	}{
		{path: "idle timeout", setup: idle, act: func(r *rig) { r.round(6 * time.Minute) }},
		{path: "provision timeout", setup: func(r *rig) { r.runner(name) }, act: func(r *rig) { r.round(4 * time.Minute) }},
		{path: "not in the ledger: closed earlier", setup: func(r *rig) {
			// A start that timed out and was written off, then came up after all.
			idle(r)
			if err := errors.Join(r.ledger.SetState(name, "failed", "agent start: timeout"), r.ledger.MarkRemoved(name, *r.now)); err != nil {
				t.Fatal(err)
			}
		}, act: func(r *rig) { r.round(10 * time.Second) }},
		{path: "not in the ledger: never known", setup: func(r *rig) {
			// Left by a controller whose ledger is gone, of a class this one does not have.
			r.gh.register(name)
			if _, err := r.hosts[0].agent.Start(context.Background(), api.StartRequest{Name: name, Class: "gone", CPUs: 1, MemBytes: 1 << 30, Image: "runner:1", JITConfig: "jit"}); err != nil {
				t.Fatal(err)
			}
		}, act: func(r *rig) { r.round(10 * time.Second) }},
		{path: "shutdown", setup: idle, act: func(r *rig) { r.shutdown() }},
		{path: "shutdown: closed earlier", setup: func(r *rig) {
			idle(r)
			if err := r.ledger.SetState(name, "lost", "host h1 unreachable for 3m"); err != nil {
				t.Fatal(err)
			}
		}, act: func(r *rig) { r.ag.Sample(context.Background()); r.shutdown() }},
		{path: "shutdown: ledger unreadable", setup: idle, act: func(r *rig) { r.ledger.Close(); r.shutdown() }, always: "kept"},
		{path: "idle timeout: GitHub cannot be asked", setup: idle, act: func(r *rig) { r.gh.down = true; r.round(6 * time.Minute) }, always: "kept"},
		{path: "maxDuration", setup: func(r *rig) {
			idle(r)
			r.drv.mu.Lock()
			r.drv.ev[name] = agent.LogEvents{Listening: *r.now, JobStarted: *r.now}
			r.drv.mu.Unlock()
			r.round(time.Second)
		}, act: func(r *rig) { r.round(61 * time.Minute) }, always: "removed"},
	}
	for _, row := range rows {
		for _, held := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/job=%v", row.path, held), func(t *testing.T) {
				r := newRig(t)
				row.setup(r)
				r.gh.assign(name, held)
				row.act(r)
				want := held
				if row.always != "" {
					want = row.always == "kept"
				}
				if kept := !r.drv.wasRemoved(name); kept != want {
					t.Fatalf("kept=%v, want %v (ledger: %s)", kept, want, r.state(name))
				}
				if held && row.always == "" && r.state(name) != "busy" && r.state(name) != "absent" {
					t.Errorf("ledger says %s after GitHub refused; want busy", r.state(name))
				}
			})
		}
	}
}

// GitHub let go of the registration and then the host failed to remove the
// container: the next round removes what was left.
func TestContainerLeftByAFailedRemovalGoesNextRound(t *testing.T) {
	const name = "small-h1-bbbb"
	r := newRig(t)
	r.runner(name)
	r.listening(name)
	r.round(0)
	r.drv.removeErr = errors.New("docker: device busy")
	r.round(6 * time.Minute)
	if r.drv.wasRemoved(name) || !slices.Contains(r.gh.dropped, name) {
		t.Fatalf("after the failed removal: removed=%v dropped=%v", r.drv.removed, r.gh.dropped)
	}
	r.drv.removeErr = nil
	r.round(10 * time.Second)
	if !r.drv.wasRemoved(name) || r.state(name) != "failed+removed" {
		t.Fatalf("container left behind: removed=%v ledger=%s", r.drv.removed, r.state(name))
	}
}

func TestLaunchOnAHostThatLeft(t *testing.T) {
	r := newRig(t)
	if err := r.ledger.InsertRunner("small-h9-cccc", "small", "linux/docker", "h9", *r.now, size.Estimate{}); err != nil {
		t.Fatal(err)
	}
	r.launch(context.Background(), "small-h9-cccc", decide.Start{Class: "small", Host: "h9"}, *r.now, 0, 0)
	if got := r.state("small-h9-cccc"); got != "failed+removed" {
		t.Fatalf("ledger %s", got)
	}
	if len(r.gh.ids) != 0 {
		t.Errorf("a registration was made for a host that is gone: %v", r.gh.ids)
	}
}

// The agent does not answer the start in time, yet starts the runner. The
// runner is not written off and then removed as a stranger: it stays
// "starting" and is taken up when it shows.
func TestStartThatWasNotAnsweredIsNotWrittenOff(t *testing.T) {
	r := newRig(t)
	lost := func(w http.ResponseWriter, q *http.Request) bool {
		if q.Method != http.MethodPost {
			return false
		}
		rec := httptest.NewRecorder()
		r.ag.Handler().ServeHTTP(rec, q) // the host does start it
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // and the answer never arrives
		return true
	}
	r.answer.Store(&lost)
	r.start(context.Background(), decide.Start{Class: "small", Host: "h1"}, *r.now)
	r.starts.Wait()
	live, _ := r.ledger.LiveRunners()
	if len(live) != 1 || live[0].State != "starting" {
		t.Fatalf("live runners %+v", live)
	}
	got := live[0].Name
	r.answer.Store(nil)
	r.listening(got)
	r.round(10 * time.Second)
	if r.drv.wasRemoved(got) || r.state(got) != "idle" {
		t.Fatalf("removed=%v ledger=%s", r.drv.removed, r.state(got))
	}
}

// ...and when it never shows, the provision timeout closes the row.
func TestStartThatWasNotAnsweredAndNeverShows(t *testing.T) {
	r := newRig(t)
	lost := func(w http.ResponseWriter, q *http.Request) bool {
		if q.Method != http.MethodPost {
			return false
		}
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
		return true
	}
	r.answer.Store(&lost)
	r.start(context.Background(), decide.Start{Class: "small", Host: "h1"}, *r.now)
	r.starts.Wait()
	live, _ := r.ledger.LiveRunners()
	name := live[0].Name
	r.answer.Store(nil)
	r.round(time.Minute)
	if got := r.state(name); got != "starting" {
		t.Fatalf("after a minute: %s", got)
	}
	r.round(3 * time.Minute)
	if got := r.state(name); got != "failed+removed" {
		t.Fatalf("after the provision timeout: %s", got)
	}
}

// A snapshot asked for before the agent answered the start cannot list the
// runner; that is not a missing container. One asked for afterwards is.
func TestNewRunnerIsNotMissingFromAnOlderSnapshot(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.refreshHosts(ctx, *r.now) // the snapshot of this round: no runners
	r.start(ctx, decide.Start{Class: "small", Host: "h1"}, *r.now)
	r.starts.Wait()
	live, _ := r.ledger.LiveRunners()
	name := live[0].Name
	container := func() string {
		live, _ := r.ledger.LiveRunners()
		for _, dr := range r.syncRunners(ctx, live) {
			if dr.Name == name {
				return dr.Container
			}
		}
		return "absent"
	}
	if got := container(); got != "starting" {
		t.Fatalf("against the older snapshot: container %q, want starting", got)
	}
	// Removed by hand on the host; the next snapshot is newer than the start.
	r.drv.mu.Lock()
	delete(r.drv.cs, name)
	r.drv.mu.Unlock()
	r.ag.Sample(ctx)
	r.refreshHosts(ctx, *r.now)
	if got := container(); got != "" {
		t.Fatalf("against a newer snapshot without it: container %q, want missing", got)
	}
}

// The 24 hour chart has a line for every host that held runners, not only
// for the hosts of the config.
func TestSeriesHasHostsThatJoined(t *testing.T) {
	r := newRig(t)
	if err := r.ledger.InsertRunner("small-desk1-eeee", "small", "linux/docker", "desk1", r.now.Add(-time.Hour), size.Estimate{}); err != nil {
		t.Fatal(err)
	}
	s, err := r.series(*r.now)
	if err != nil {
		t.Fatal(err)
	}
	line, ok := s.HostCPUs["desk1"]
	if !ok || line[len(line)-1] != 1 {
		t.Fatalf("hosts in the chart: %v", strings.Join(slices.Sorted(func(yield func(string) bool) {
			for k := range s.HostCPUs {
				if !yield(k) {
					return
				}
			}
		}), ","))
	}
}
