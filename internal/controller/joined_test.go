package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/cgroup"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/join"
	"github.com/midagedev/pumasi/internal/ledger"
)

// memDriver stands in for docker on the joined host.
type memDriver struct {
	mu sync.Mutex
	cs map[string]agent.Container
}

func (d *memDriver) Start(_ context.Context, s agent.Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cs[s.Name] = agent.Container{Name: s.Name, Class: s.Class, CPUs: s.CPUs, MemBytes: s.MemBytes, State: "running"}
	return nil
}
func (d *memDriver) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.cs, name)
	return nil
}
func (d *memDriver) List(context.Context) ([]agent.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []agent.Container
	for _, c := range d.cs {
		out = append(out, c)
	}
	return out, nil
}
func (d *memDriver) LogEvents(context.Context, string, time.Time) (agent.LogEvents, error) {
	return agent.LogEvents{}, nil
}
func (d *memDriver) LogTail(context.Context, string, int) string { return "" }
func (d *memDriver) DataDir(context.Context) string              { return "" }
func (d *memDriver) Check(context.Context, string) error         { return nil }

// awayTold is what each test host was last told with PUT /v1/away.
var awayTold sync.Map

// joinedHost registers a host with the controller's join server and keeps its
// agent connected, the way `pumasi join` and `pumasi agent -join` do.
func joinedHost(t *testing.T, c *Controller, addr, name string) (stop func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	id, err := join.LoadIdentity(c.cfg.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	token, _ := join.NewToken(c.cfg.StateDir(), "", 1, time.Hour)
	hc := join.HostConfig{Controller: addr, Pin: id.Pin, Name: name, Kind: "desktop", CPUs: 2, Mem: 4 << 30}
	hc.ID, hc.Secret = join.NewHostID()
	if err := hc.Register(context.Background(), token, false); err != nil {
		t.Fatal(err)
	}
	cg := cgroup.Reader{CgroupRoot: filepath.Join("..", "cgroup", "testdata", "cg"),
		ProcRoot: filepath.Join("..", "cgroup", "testdata", "proc"), SysRoot: filepath.Join("..", "cgroup", "testdata", "sys")}
	a := agent.New(agent.Config{Host: name, Kind: hc.Kind, Version: "abc123", BudgetCPUs: hc.CPUs, BudgetMem: hc.Mem},
		&memDriver{cs: map[string]agent.Container{}}, &agent.CgroupMeter{R: cg}, log)
	a.Sample(context.Background())
	mux := http.NewServeMux()
	mux.Handle("/", a.Handler())
	mux.HandleFunc("POST /v1/leave", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"containers": 3})
	})
	mux.HandleFunc("PUT /v1/away", func(w http.ResponseWriter, r *http.Request) {
		var in api.Away
		_ = json.NewDecoder(r.Body).Decode(&in)
		awayTold.Store(name, in)
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&join.Host{Config: hc, Version: "abc123", Arch: "amd64", Handler: mux, Log: log}).Run(ctx)
		close(done)
	}()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func joinController(t *testing.T) (*Controller, string, func() []string) {
	t.Helper()
	c, _, sent := testBook(t)
	dir := t.TempDir()
	c.cfg.Ledger = filepath.Join(dir, "pumasi.db")
	c.cfg.Listen = "127.0.0.1:0"
	c.cfg.HostLostAfter = time.Minute
	c.cfg.Hosts = []config.Host{{Name: "server1", SSH: "server1"}}
	c.kick = make(chan struct{}, 1)
	c.join = join.NewServer(dir, t.TempDir(), c.log)
	c.join.Changed = func() {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
	addr, err := c.join.Listen(c.cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.join.Close)
	return c, addr.String(), sent
}

func TestJoinedHostIsAHostLikeAnyOther(t *testing.T) {
	c, addr, sent := joinController(t)
	ctx := context.Background()
	stop := joinedHost(t, c, addr, "desk1")

	select {
	case <-c.kick: // joining wakes the evaluation
	case <-time.After(3 * time.Second):
		t.Fatal("no kick after a host joined")
	}
	// It becomes a host with its first connection.
	for end := time.Now().Add(3 * time.Second); len(c.hosts) == 0 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		c.syncJoined(true)
	}
	if len(c.hosts) != 1 || c.hosts[0].cfg.Name != "desk1" || c.hosts[0].cfg.Budget.CPUs != 2 {
		t.Fatalf("hosts %+v", c.hosts)
	}
	h := c.hosts[0]
	if err := h.wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// The agent API, unchanged, over the host's own connection.
	s, err := h.agent.Snapshot(ctx)
	if err != nil || s.Host != "desk1" || s.Kind != "desktop" || s.Version != "abc123" || s.BudgetCPUs != 2 {
		t.Fatalf("snapshot %+v %v", s, err)
	}
	r, err := h.agent.Start(ctx, api.StartRequest{Name: "linux-s-desk1-1", Class: "linux-s", CPUs: 1, MemBytes: 1 << 30, Image: "img", JITConfig: "jit"})
	if err != nil || r.State != "created" {
		t.Fatalf("start %+v %v", r, err)
	}
	if _, err := h.agent.Start(ctx, api.StartRequest{Name: "linux-l-desk1-2", Class: "linux-l", CPUs: 2, MemBytes: 1 << 30, Image: "img", JITConfig: "jit"}); err == nil ||
		!strings.Contains(err.Error(), "409") {
		t.Fatalf("over the host's own budget: %v", err)
	}
	if problem, err := h.agent.Check(ctx, "img"); err != nil || problem != "" {
		t.Fatalf("check %q %v", problem, err)
	}
	if got, err := h.agent.Delete(ctx, "linux-s-desk1-1"); err != nil || got.Name != "linux-s-desk1-1" {
		t.Fatalf("delete %+v %v", got, err)
	}

	// status and the dashboard list it next to the config's hosts.
	now := time.Now()
	c.refreshHosts(ctx, now)
	c.setStatus(decide.Snapshot{}, decide.Plan{})
	st, err := CollectState(c.cfg, c.ledger, c.liveStatus(), true, now)
	if err != nil || len(st.Hosts) != 2 {
		t.Fatalf("state %+v %v", st.Hosts, err)
	}
	if hs := st.Hosts[1]; hs.Name != "desk1" || hs.Kind != "desktop" || hs.BudgetCPUs != 2 ||
		hs.Reachable == nil || !*hs.Reachable || hs.SeenSeconds == nil || hs.Agent != "abc123" {
		t.Fatalf("host state %+v", hs)
	}

	// The host drops: the incident says why, and ends when it is back.
	stop()
	for end := time.Now().Add(3 * time.Second); h.why() == "" && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	c.refreshHosts(ctx, now)
	if h.snap != nil || h.downSince.IsZero() {
		t.Fatal("still up after the drop")
	}
	c.hostIncidents(now.Add(2*time.Minute), []ledger.Runner{{Name: "r1", Host: "desk1", State: "busy"}})
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], "desk1 unreachable for 2m0s (agent exited) — 1 jobs still running there") {
		t.Fatalf("notices %q", got)
	}
}

func TestDownAsksAJoinedHostToRemoveItself(t *testing.T) {
	c, addr, _ := joinController(t)
	ctx := context.Background()
	joinedHost(t, c, addr, "desk1")
	c.syncJoined(false)
	h := c.hosts[0]
	if names, err := h.running(ctx); err != nil || len(names) != 0 {
		t.Fatalf("running %v %v", names, err)
	}
	c.gh = &fakeGitHub{}
	if err := c.down(ctx, false); err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(join.ReadMembers(c.cfg.StateDir())) != 0 {
		t.Fatal("still a member after down")
	}
	// It left: its runners in the ledger close as "not configured" on the next round.
	c.syncJoined(true)
	if len(c.hosts) != 0 {
		t.Fatalf("hosts %+v", c.hosts)
	}
}

func TestConfigNamesAreNotGivenToJoiningHosts(t *testing.T) {
	c, addr, _ := joinController(t)
	c.join.Reserved = func(n string) bool { return n == "server1" }
	id, _ := join.LoadIdentity(c.cfg.StateDir())
	token, _ := join.NewToken(c.cfg.StateDir(), "", 1, time.Hour)
	hc := join.HostConfig{Controller: addr, Pin: id.Pin, Name: "server1", Kind: "server", CPUs: 1, Mem: 1 << 30}
	hc.ID, hc.Secret = join.NewHostID()
	if err := hc.Register(context.Background(), token, false); err == nil || !strings.Contains(err.Error(), "pumasi.yaml") {
		t.Fatalf("register: %v", err)
	}
}
