package decide

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const gb = int64(1) << 30

var t0 = time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)

var (
	classS = Class{Name: "linux-s", CPUs: 1, MemBytes: 1 * gb, MaxRunners: 10}
	classL = Class{Name: "linux-l", CPUs: 2, MemBytes: 3 * gb, MaxRunners: 10}
)

func server() Host {
	return Host{Name: "server1", Kind: "server", Up: true, CPUs: 48, MemBytes: 128 * gb, BudgetCPUs: 4, BudgetMem: 6 * gb, NonRunnerCPU: 27, NonRunnerMem: 115 * gb}
}

func desktop() Host {
	return Host{Name: "desk1", Kind: "desktop", Up: true, CPUs: 20, MemBytes: 28 * gb, BudgetCPUs: 6, BudgetMem: 9 * gb, NonRunnerCPU: 1, NonRunnerMem: 2 * gb}
}

func demand(class string, n int) []Demand {
	var ds []Demand
	for i := range n {
		ds = append(ds, Demand{ID: fmt.Sprintf("%s-%d", class, i), Class: class, FirstSeen: t0.Add(time.Duration(i) * time.Second)})
	}
	return ds
}

func snap(hosts []Host, d []Demand, rs []Runner) Snapshot {
	return Snapshot{
		Now: t0.Add(time.Minute), Hosts: hosts, Classes: []Class{classS, classL}, Demand: d, Runners: rs,
		PSICPUThreshold: 40, ProvisionTimeout: 3 * time.Minute, IdleTimeout: 5 * time.Minute, HostLostAfter: 2 * time.Minute,
	}
}

func starts(p Plan) []string {
	var out []string
	for _, s := range p.Start {
		out = append(out, s.Class+"@"+s.Host)
	}
	return out
}

func defers(p Plan) []string {
	var out []string
	for _, d := range p.Defer {
		out = append(out, fmt.Sprintf("%s x%d", d.Class, d.Count))
	}
	return out
}

func TestDecidePlacement(t *testing.T) {
	busyHost := server()
	busyHost.PSICPU = 55
	crowded := server()
	crowded.NonRunnerCPU = 46.5 // only 1.5 cpus left on the machine, below the budget of 4
	lowMem := desktop()
	lowMem.NonRunnerMem = 26 * gb
	down := server()
	down.Up = false
	down.DownFor = 30 * time.Second
	onlyServerL := classL
	onlyServerL.Hosts = []string{"server1"}
	capped := classS
	capped.MaxRunners = 1

	tests := []struct {
		name       string
		snap       Snapshot
		wantStarts []string
		wantDefers []string
	}{
		{"no demand", snap([]Host{server(), desktop()}, nil, nil), nil, nil},
		{"server before desktop", snap([]Host{server(), desktop()}, demand("linux-s", 1), nil),
			[]string{"linux-s@server1"}, nil},
		{"server budget full goes to desktop", snap([]Host{server(), desktop()}, demand("linux-l", 1), []Runner{
			{Name: "a", Class: "linux-l", Host: "server1", State: "busy", Container: "running", JobKnown: true},
			{Name: "b", Class: "linux-l", Host: "server1", State: "busy", Container: "running", JobKnown: true},
		}), []string{"linux-l@desk1"}, nil},
		// Acceptance mix: 4 linux-l + 6 linux-s against server 4cpu/6G and desktop 6cpu/9G.
		{"acceptance mix", snap([]Host{server(), desktop()}, append(demand("linux-l", 4), demand("linux-s", 6)...), nil),
			// Jobs alternate by arrival; the second linux-l no longer fits the server (1cpu/2G left) and goes to
			// the desktop, while the next linux-s still fills the server's last cpu.
			[]string{"linux-l@server1", "linux-s@server1", "linux-l@desk1", "linux-s@server1", "linux-l@desk1", "linux-s@desk1", "linux-s@desk1"},
			[]string{"linux-l x1", "linux-s x2"}},
		{"psi over threshold", snap([]Host{busyHost}, demand("linux-s", 1), nil), nil, []string{"linux-s x1"}},
		{"machine use below budget limits", snap([]Host{crowded}, append(demand("linux-l", 1), demand("linux-s", 2)...), nil),
			[]string{"linux-s@server1"}, []string{"linux-l x1", "linux-s x1"}},
		{"low memory desktop", snap([]Host{lowMem}, demand("linux-l", 1), nil), nil, []string{"linux-l x1"}},
		{"host down is skipped", snap([]Host{down, desktop()}, demand("linux-s", 1), nil), []string{"linux-s@desk1"}, nil},
		{"starting runner covers demand", snap([]Host{server()}, demand("linux-s", 2), []Runner{
			{Name: "a", Class: "linux-s", Host: "server1", State: "starting", Container: "running", CreatedAt: t0},
		}), []string{"linux-s@server1"}, nil},
		{"busy runner with a known job does not cover", snap([]Host{server()}, demand("linux-s", 1), []Runner{
			{Name: "a", Class: "linux-s", Host: "server1", State: "busy", Container: "running", JobKnown: true},
		}), []string{"linux-s@server1"}, nil},
		// The runner printed "Running job" but GitHub's JobStarted (30-50s late) has not closed the demand.
		{"busy runner awaiting JobStarted still covers", snap([]Host{server()}, demand("linux-s", 1), []Runner{
			{Name: "a", Class: "linux-s", Host: "server1", State: "busy", Container: "running"},
		}), nil, nil},
		{"exited runner frees capacity", snap([]Host{server()}, demand("linux-l", 1), []Runner{
			{Name: "a", Class: "linux-l", Host: "server1", State: "busy", Container: "exited", JobKnown: true},
			{Name: "b", Class: "linux-l", Host: "server1", State: "busy", Container: "running", JobKnown: true},
		}), []string{"linux-l@server1"}, nil},
		{"class host allow list", Snapshot{Now: t0, Hosts: []Host{desktop()}, Classes: []Class{onlyServerL}, Demand: demand("linux-l", 1)},
			nil, []string{"linux-l x1"}},
		{"max runners", Snapshot{Now: t0, Hosts: []Host{desktop()}, Classes: []Class{capped}, Demand: demand("linux-s", 2)},
			[]string{"linux-s@desk1"}, []string{"linux-s x1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Decide(tt.snap)
			if got := starts(p); !reflect.DeepEqual(got, tt.wantStarts) {
				t.Errorf("starts %v, want %v", got, tt.wantStarts)
			}
			if got := defers(p); !reflect.DeepEqual(got, tt.wantDefers) {
				t.Errorf("defers %v, want %v", got, tt.wantDefers)
			}
			checkWithinBudget(t, tt.snap, p)
			if again := Decide(tt.snap); !reflect.DeepEqual(again, p) {
				t.Error("not deterministic")
			}
		})
	}
}

// checkWithinBudget is acceptance criterion 1 as an invariant: after the plan,
// no host holds more class capacity than its budget.
func checkWithinBudget(t *testing.T, s Snapshot, p Plan) {
	t.Helper()
	size := map[string]Class{}
	for _, c := range s.Classes {
		size[c.Name] = c
	}
	stopped := map[string]bool{}
	for _, st := range p.Stop {
		stopped[st.Runner] = true
	}
	cpu, mem := map[string]float64{}, map[string]int64{}
	for _, r := range s.Runners {
		if !stopped[r.Name] && r.Container != "exited" {
			cpu[r.Host] += size[r.Class].CPUs
			mem[r.Host] += size[r.Class].MemBytes
		}
	}
	for _, st := range p.Start {
		cpu[st.Host] += size[st.Class].CPUs
		mem[st.Host] += size[st.Class].MemBytes
	}
	for _, h := range s.Hosts {
		if h.BudgetCPUs > 0 && cpu[h.Name] > h.BudgetCPUs {
			t.Errorf("%s: %.1f cpus over budget %.1f", h.Name, cpu[h.Name], h.BudgetCPUs)
		}
		if h.BudgetMem > 0 && mem[h.Name] > h.BudgetMem {
			t.Errorf("%s: %d mem over budget %d", h.Name, mem[h.Name], h.BudgetMem)
		}
	}
}

func TestDecideStops(t *testing.T) {
	longDown := server()
	longDown.Up = false
	longDown.DownFor = 3 * time.Minute
	shortDown := longDown
	shortDown.DownFor = time.Minute
	r := func(state, container string) Runner {
		return Runner{Name: "r", Class: "linux-s", Host: "server1", State: state, Container: container, CreatedAt: t0}
	}
	listening := r("idle", "running")
	listening.ListeningAt = t0.Add(10 * time.Second)

	tests := []struct {
		name   string
		host   Host
		runner Runner
		now    time.Duration // since t0
		want   string        // "state|reason prefix", "" = no stop
	}{
		{"starting within timeout", server(), r("starting", "running"), 2 * time.Minute, ""},
		{"provision timeout", server(), r("starting", "running"), 4 * time.Minute, "failed|provision timeout"},
		{"exited before job", server(), r("starting", "exited"), time.Minute, "failed|exited before a job"},
		{"exited during job keeps state", server(), r("busy", "exited"), time.Minute, "|runner exited during or after its job"},
		{"busy runs on", server(), r("busy", "running"), time.Hour, ""},
		{"job completed", server(), r("done", "running"), time.Minute, "|job completed"},
		{"orphan container", server(), r("", "running"), time.Minute, "|not in the ledger"},
		{"container missing", server(), r("idle", ""), time.Minute, "failed|container missing"},
		{"idle within timeout", server(), listening, 4 * time.Minute, ""},
		{"idle timeout", server(), listening, 6 * time.Minute, "failed|idle timeout"},
		{"host briefly down", shortDown, r("busy", ""), time.Minute, ""},
		{"host lost: idle runner written off", longDown, r("idle", ""), time.Minute, "lost|host server1 unreachable"},
		{"host lost: starting runner written off", longDown, r("starting", ""), time.Minute, "lost|host server1 unreachable"},
		{"host lost: busy runner kept", longDown, r("busy", ""), time.Minute, ""},
		{"host lost: finished runner waits for the agent", longDown, r("done", ""), time.Minute, ""},
		{"host removed from config", Host{}, r("busy", ""), time.Minute, "lost|host server1 is not configured"},
		{"orphan running a job is kept", server(), working(r("", "running")), time.Minute, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := snap([]Host{tt.host}, nil, []Runner{tt.runner})
			s.Now = t0.Add(tt.now)
			p := Decide(s)
			got := ""
			if len(p.Stop) == 1 {
				got = p.Stop[0].State + "|" + p.Stop[0].Reason
			}
			if tt.want == "" && got != "" || tt.want != "" && !strings.HasPrefix(got, tt.want) {
				t.Errorf("stop %q, want prefix %q", got, tt.want)
			}
		})
	}
}

func working(r Runner) Runner {
	r.Working = true
	return r
}

// A host whose tunnel is down gets nothing new, and its busy runners still hold their capacity.
func TestUnreachableHostKeepsBusyRunners(t *testing.T) {
	down := server()
	down.Up = false
	down.DownFor = time.Hour
	busy := Runner{Name: "r", Class: "linux-l", Host: "server1", State: "busy", JobKnown: true, CreatedAt: t0}
	p := Decide(snap([]Host{down}, demand("linux-s", 1), []Runner{busy}))
	if len(p.Stop) != 0 || len(p.Start) != 0 || len(p.Defer) != 1 {
		t.Fatalf("plan %+v", p)
	}
	// Back up: the runner is still there and counts against the 4 cpu budget (2 used, 2 free).
	busy.Container = "running"
	p = Decide(snap([]Host{server()}, demand("linux-l", 2), []Runner{busy}))
	if len(p.Stop) != 0 || len(p.Start) != 1 || len(p.Defer) != 1 {
		t.Fatalf("plan after reconnect %+v", p)
	}
}

func TestDeferReasonNamesEachHost(t *testing.T) {
	full := server()
	full.BudgetCPUs = 1
	p := Decide(snap([]Host{full, desktop()}, demand("linux-l", 4), nil))
	if len(p.Defer) != 1 {
		t.Fatalf("defers %+v", p.Defer)
	}
	for _, want := range []string{"no host with 2.0cpu/3.0G free", "server1 1.0cpu/", "desk1 0.0cpu/"} {
		if !strings.Contains(p.Defer[0].Reason, want) {
			t.Errorf("reason %q lacks %q", p.Defer[0].Reason, want)
		}
	}
}

func TestRebootLostAndMaxDuration(t *testing.T) {
	s := snap([]Host{server()}, nil, []Runner{
		{Name: "gone", Class: "linux-l", Host: "server1", State: "busy", JobKnown: true, CreatedAt: t0, Lost: "host rebooted"},
		{Name: "long", Class: "linux-l", Host: "server1", State: "busy", JobKnown: true, CreatedAt: t0, Container: "running", Working: true, JobStartedAt: t0},
		{Name: "short", Class: "linux-s", Host: "server1", State: "busy", JobKnown: true, CreatedAt: t0, Container: "running", Working: true, JobStartedAt: t0},
	})
	for i := range s.Classes {
		if s.Classes[i].Name == "linux-l" {
			s.Classes[i].MaxDuration = time.Minute
		}
	}
	s.Now = t0.Add(2 * time.Minute)
	p := Decide(s)
	if len(p.Stop) != 2 {
		t.Fatalf("stops %+v", p.Stop)
	}
	if st := p.Stop[0]; st.Runner != "gone" || st.State != "lost" || st.Reason != "host rebooted" {
		t.Errorf("reboot stop %+v", st)
	}
	if st := p.Stop[1]; st.Runner != "long" || st.State != "failed" || !strings.HasPrefix(st.Reason, OverMaxDuration) {
		t.Errorf("maxDuration stop %+v", st)
	}
}

func TestClosedHostTakesNoRunners(t *testing.T) {
	closed := server()
	closed.Closed = "quarantined"
	p := Decide(snap([]Host{closed}, demand("linux-s", 1), nil))
	if len(p.Start) != 0 || len(p.Defer) != 1 || !strings.Contains(p.Defer[0].Reason, "server1 quarantined") {
		t.Fatalf("plan %+v", p)
	}
	p = Decide(snap([]Host{closed, desktop()}, demand("linux-s", 1), nil))
	if len(p.Start) != 1 || p.Start[0].Host != "desk1" {
		t.Fatalf("plan with an open host %+v", p)
	}
}

func TestClassRunsOnlyOnItsOS(t *testing.T) {
	mac := Host{Name: "mac1", Kind: "desktop", OS: "macos", Up: true, CPUs: 14, MemBytes: 48 * gb, BudgetCPUs: 8, BudgetMem: 16 * gb}
	win := Host{Name: "win1", Kind: "desktop", OS: "windows", Up: true, CPUs: 20, MemBytes: 32 * gb, BudgetCPUs: 8, BudgetMem: 16 * gb}
	old := server() // an agent from before hosts said their OS: linux
	classMac := Class{Name: "macos", OS: "macos", CPUs: 4, MemBytes: 8 * gb, MaxRunners: 10}
	classWin := Class{Name: "windows", OS: "windows", CPUs: 4, MemBytes: 8 * gb, MaxRunners: 10}
	s := Snapshot{Now: t0, Hosts: []Host{mac, win, old}, Classes: []Class{classS, classMac, classWin},
		Demand: append(append(demand("linux-s", 6), demand("macos", 3)...), demand("windows", 1)...)}
	p := Decide(s)
	got := map[string][]string{}
	for _, st := range p.Start {
		got[st.Class] = append(got[st.Class], st.Host)
	}
	want := map[string][]string{
		"linux-s": {"server1", "server1", "server1", "server1"}, // the server's budget; never the idle mac or windows host
		"macos":   {"mac1", "mac1"},
		"windows": {"win1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("starts %v, want %v", got, want)
	}
	for _, d := range p.Defer {
		if d.Class == "linux-s" && (strings.Contains(d.Reason, "mac1") || strings.Contains(d.Reason, "win1")) {
			t.Errorf("a linux class does not wait for a host of another OS: %s", d.Reason)
		}
		if d.Class == "macos" && strings.Contains(d.Reason, "server1") {
			t.Errorf("a macos class does not wait for a linux host: %s", d.Reason)
		}
	}

	// A mac that is down is still a mac: the linux class does not name it, the macos class does.
	mac.Up, mac.DownFor = false, time.Minute
	p = Decide(Snapshot{Now: t0, Hosts: []Host{mac}, Classes: []Class{classS, classMac}, Demand: append(demand("linux-s", 1), demand("macos", 1)...)})
	for _, d := range p.Defer {
		if named := strings.Contains(d.Reason, "mac1 down"); named != (d.Class == "macos") {
			t.Errorf("%s: %s", d.Class, d.Reason)
		}
	}
	// A host that has never answered has no known OS and counts as down for every class.
	if len(p.Start) != 0 || len(p.Defer) != 2 {
		t.Errorf("plan %+v", p)
	}
}

func TestClassBoundToHosts(t *testing.T) {
	bound := Class{Name: "release", CPUs: 2, MemBytes: 2 * gb, MaxRunners: 10, Hosts: []string{"desk1"}}
	s := snap([]Host{server(), desktop()}, demand("release", 4), nil)
	s.Classes = []Class{bound}
	// The desktop's budget holds three. The server has room and still gets none.
	p := Decide(s)
	if got := starts(p); !reflect.DeepEqual(got, []string{"release@desk1", "release@desk1", "release@desk1"}) {
		t.Errorf("starts %v", got)
	}
	if len(p.Defer) != 1 || !strings.Contains(p.Defer[0].Reason, "desk1 0.0cpu") || strings.Contains(p.Defer[0].Reason, "server1") {
		t.Errorf("defer %+v, want the full desktop named and the server left out", p.Defer)
	}
	// The host it is bound to is down, or has never joined: said by name.
	down := desktop()
	down.Up = false
	s.Hosts = []Host{server(), down}
	if p := Decide(s); len(p.Start) != 0 || !strings.Contains(p.Defer[0].Reason, "desk1 down") {
		t.Errorf("plan %+v", p)
	}
	s.Hosts = []Host{server()}
	if p := Decide(s); len(p.Start) != 0 || !strings.Contains(p.Defer[0].Reason, "desk1 has not joined") {
		t.Errorf("plan %+v", p)
	}
}

func TestClassGoesWhereItsPathsAre(t *testing.T) {
	mnt := Class{Name: "mnt", CPUs: 1, MemBytes: gb, MaxRunners: 10, Paths: []string{"/srv/ci-cache", "/srv/keys"}}
	srv, desk := server(), desktop()
	desk.Paths = []string{"/srv/ci-cache", "/srv/keys", "/other"}
	srv.Paths = []string{"/srv/ci-cache"}
	s := snap([]Host{srv, desk}, demand("mnt", 2), nil)
	s.Classes = []Class{mnt}
	if got := starts(Decide(s)); !reflect.DeepEqual(got, []string{"mnt@desk1", "mnt@desk1"}) {
		t.Errorf("starts %v, want the desktop although servers go first", got)
	}
	// Nobody has it: the reason is the path.
	desk.Paths = nil
	s.Hosts = []Host{srv, desk}
	if p := Decide(s); len(p.Start) != 0 || p.Defer[0].Reason != "no host has /srv/keys" || p.Defer[0].Count != 2 {
		t.Errorf("plan %+v", p)
	}
	// The one that has it is full: both are said.
	desk.Paths, desk.BudgetCPUs = []string{"/srv/ci-cache", "/srv/keys"}, 0.5
	s.Hosts = []Host{srv, desk}
	if p := Decide(s); len(p.Start) != 0 || !strings.Contains(p.Defer[0].Reason, "desk1 0.5cpu") || !strings.Contains(p.Defer[0].Reason, "the others lack /srv/keys") {
		t.Errorf("plan %+v", p)
	}
}

func TestNativeClassNeedsAHostThatCan(t *testing.T) {
	build := Class{Name: "build", CPUs: 1, MemBytes: gb, MaxRunners: 10, Native: true}
	srv, desk := server(), desktop()
	srv.NoNative = "no systemd user session for ci"
	s := snap([]Host{srv, desk}, demand("build", 1), nil)
	s.Classes = []Class{build, classS}
	s.Demand = append(s.Demand, demand("linux-s", 1)...)
	// The container class still goes to the server.
	if got := starts(Decide(s)); !reflect.DeepEqual(got, []string{"build@desk1", "linux-s@server1"}) {
		t.Errorf("starts %v", got)
	}
	s.Hosts = []Host{srv}
	if p := Decide(s); len(p.Start) != 1 || len(p.Defer) != 1 || !strings.Contains(p.Defer[0].Reason, "server1: no systemd user session for ci") {
		t.Errorf("plan %+v", p)
	}
}
