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

func TestReserveKeepsRoomForItsClass(t *testing.T) {
	deploy := Class{Name: "deploy", CPUs: 2, MemBytes: 2 * gb, MaxRunners: 10, Reserve: 1}
	// The desktop's budget (6 cpus) holds six linux-s, but two cpus are deploy's.
	s := snap([]Host{desktop()}, demand("linux-s", 6), nil)
	s.Classes = []Class{classS, deploy}
	p := Decide(s)
	if len(p.Start) != 4 || len(p.Defer) != 1 || p.Defer[0].Count != 2 || !strings.Contains(p.Defer[0].Reason, "desk1 reserved for deploy") {
		t.Fatalf("plan %+v, want four linux-s and two waiting behind deploy's reserve", p)
	}
	// The deploy job itself takes the room at once.
	s.Demand = append(s.Demand, demand("deploy", 1)...)
	if got := starts(Decide(s)); len(got) != 5 || !strings.Contains(strings.Join(got, " "), "deploy@desk1") {
		t.Errorf("starts %v, want the deploy runner among them", got)
	}
	// While a deploy runner is up the reserve is in use: the others may fill the rest.
	s.Demand = demand("linux-s", 6)
	s.Runners = []Runner{{Name: "deploy-1", Class: "deploy", Host: "desk1", State: "busy", JobKnown: true, Container: "running", Working: true, CreatedAt: t0}}
	if p := Decide(s); len(p.Start) != 4 || strings.Contains(p.Defer[0].Reason, "reserved") {
		t.Errorf("plan %+v, want four linux-s beside the running deploy and no word of a reserve", p)
	}
}

func TestReserveOnBoundHostLeavesOthersFree(t *testing.T) {
	deploy := Class{Name: "deploy", CPUs: 2, MemBytes: 2 * gb, MaxRunners: 10, Reserve: 2, Hosts: []string{"server1"}}
	// The server's budget is 4 cpus: all of it is deploy's. linux-s goes to the desktop only.
	s := snap([]Host{server(), desktop()}, demand("linux-s", 7), nil)
	s.Classes = []Class{classS, deploy}
	p := Decide(s)
	for _, st := range p.Start {
		if st.Host != "desk1" {
			t.Errorf("start %+v on the reserved server", st)
		}
	}
	if len(p.Start) != 6 || !strings.Contains(p.Defer[0].Reason, "server1 reserved for deploy") {
		t.Errorf("plan %+v", p)
	}
	// A reserve nobody can honour anyway (its host is down) holds nobody back.
	down := server()
	down.Up = false
	s.Hosts = []Host{down, desktop()}
	if p := Decide(s); len(p.Start) != 6 || strings.Contains(p.Defer[0].Reason, "reserved") {
		t.Errorf("plan %+v", p)
	}
}

// Placement by what jobs are expected to use, and what hosts are measured to
// have: nobody's hand-set number decides it.
func TestPlacementBySize(t *testing.T) {
	// A machine of 16 cpus and 32G with no budget; 3G is kept free.
	open := func() Host {
		return Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 32 * gb, KeepMem: 3 * gb}
	}
	job := func(id string, cpus float64, mem int64) Demand {
		return Demand{ID: id, Class: "linux-s", FirstSeen: t0, CPUs: cpus, MemBytes: mem, Sized: "for build (learned from 10 runs, the largest 3.0cpu/16.0G)"}
	}
	busy := func(name string, cpus float64, mem int64) Runner {
		return Runner{Name: name, Class: "linux-s", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true,
			CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: cpus, MemBytes: mem}
	}
	tests := []struct {
		name    string
		host    func(*Host)
		demand  []Demand
		runners []Runner
		starts  int
		reason  string // of the first start or defer
	}{
		{name: "a job never seen takes the class's first estimate",
			demand: demand("linux-s", 1), starts: 1, reason: "takes 1.0cpu/1.0G of the free 16.0cpu/29.0G"},
		{name: "a job that learned to be big takes that much, whatever the class says",
			demand: []Demand{job("a", 4, 20*gb)}, starts: 1, reason: "takes 4.0cpu/20.0G of the free 16.0cpu/29.0G for build (learned from 10 runs, the largest 3.0cpu/16.0G)"},
		{name: "two big jobs do not fit together: the second waits and says its size",
			demand: []Demand{job("a", 4, 20*gb), job("b", 4, 20*gb)}, starts: 1, reason: "takes 4.0cpu/20.0G"},
		{name: "small jobs pack by their own size, not the class's",
			demand: []Demand{job("a", 0.5, gb/2), job("b", 0.5, gb/2), job("c", 0.5, gb/2), job("d", 0.5, gb/2)}, starts: 4},
		{name: "a runner over its estimate holds what it uses: the next job waits",
			runners: []Runner{busy("r1", 2, 27*gb)}, demand: []Demand{job("a", 1, 4*gb)}, starts: 0,
			reason: "no host with 1.0cpu/4.0G free (h1 14.0cpu/2.0G)"},
		{name: "load from outside pumasi takes the room away",
			host: func(h *Host) { h.NonRunnerMem = 27 * gb }, demand: []Demand{job("a", 1, 4*gb)}, starts: 0,
			reason: "no host with 1.0cpu/4.0G free (h1 16.0cpu/2.0G)"},
		{name: "the same job goes when the outside load is gone",
			demand: []Demand{job("a", 1, 4*gb)}, starts: 1},
		{name: "a host short of memory right now gets no new runner",
			host: func(h *Host) { h.PSIMem = 25 }, demand: []Demand{job("a", 1, 4*gb)}, starts: 0,
			reason: "h1 memory pressure 25"},
		{name: "a budget is a cap on top of what is measured",
			host: func(h *Host) { h.BudgetMem = 6 * gb }, demand: []Demand{job("a", 1, 4*gb), job("b", 1, 4*gb)}, starts: 1},
		{name: "the memory kept free is not lent",
			demand: []Demand{job("a", 1, 30*gb)}, starts: 0, reason: "no host with 1.0cpu/30.0G free (h1 16.0cpu/29.0G)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := open()
			if tt.host != nil {
				tt.host(&h)
			}
			s := snap([]Host{h}, tt.demand, tt.runners)
			s.PSIMemThreshold = 10
			p := Decide(s)
			if len(p.Start) != tt.starts || len(p.Stop) != 0 {
				t.Fatalf("starts %v stops %v defers %v, want %d starts and no stop", p.Start, p.Stop, p.Defer, tt.starts)
			}
			got := ""
			switch {
			case tt.reason == "":
				return
			case tt.starts > 0:
				got = p.Start[0].Reason
			case len(p.Defer) > 0:
				got = p.Defer[0].Reason
			}
			if !strings.Contains(got, tt.reason) {
				t.Errorf("reason %q, want %q in it", got, tt.reason)
			}
		})
	}
}

// The invariant: no size is a reason to stop a runner. However far a job goes
// over what it was expected to use, and however little room its host has
// left, the plan never stops it. It only stops getting company.
func TestNoRunnerIsStoppedForItsSize(t *testing.T) {
	for _, state := range []string{"starting", "idle", "busy"} {
		for _, over := range []float64{1, 2, 10, 1000} {
			for _, pressure := range []float64{0, 99} {
				h := Host{Name: "h1", Kind: "server", Up: true, CPUs: 4, MemBytes: 8 * gb, KeepMem: gb,
					BudgetCPUs: 2, BudgetMem: 2 * gb, NonRunnerMem: 7 * gb, NonRunnerCPU: 4, PSICPU: pressure, PSIMem: pressure}
				r := Runner{Name: "r1", Class: "linux-s", Host: "h1", State: state, JobKnown: state == "busy", Container: "running",
					Working: state == "busy", CreatedAt: t0, ListeningAt: t0, CPUs: 1 * over, MemBytes: int64(float64(gb) * over)}
				if state == "busy" {
					r.JobStartedAt = t0
				}
				s := snap([]Host{h}, demand("linux-s", 3), []Runner{r})
				s.PSIMemThreshold = 10
				if p := Decide(s); len(p.Stop) != 0 {
					t.Errorf("%s runner at %gx its estimate, pressure %g: stopped: %+v", state, over, pressure, p.Stop)
				}
			}
		}
	}
}

// Priority only orders the line for room that is short. It never stops a
// runner, and nobody waits forever behind it.
func TestPriority(t *testing.T) {
	// Room for exactly one runner of either class.
	tight := func() Host {
		return Host{Name: "h1", Kind: "server", Up: true, CPUs: 2, MemBytes: 4 * gb}
	}
	job := func(id, class string, ago time.Duration, level int) Demand {
		d := Demand{ID: id, Name: id, Class: class, FirstSeen: t0.Add(-ago), Level: level}
		if level != 0 {
			d.Ranked = levelName(level) + " by rule 1"
		}
		return d
	}
	small := Class{Name: "small", CPUs: 2, MemBytes: 3 * gb}
	other := Class{Name: "other", CPUs: 2, MemBytes: 3 * gb}
	big := Class{Name: "big", CPUs: 2, MemBytes: 16 * gb}
	busy := Runner{Name: "r1", Class: "small", Host: "h1", State: "busy", JobKnown: true, Container: "running", Working: true,
		CreatedAt: t0, ListeningAt: t0, JobStartedAt: t0, CPUs: 2, MemBytes: 3 * gb}
	bigBusy := busy
	bigBusy.MemBytes = 10 * gb
	tests := []struct {
		name    string
		host    Host
		demand  []Demand
		runners []Runner
		starts  []string // class of each start, in order
		defers  []string // "class: priority" in the order of the line
		reason  string   // in the first defer's reason
	}{
		{name: "no rules: the oldest job first",
			demand: []Demand{job("new", "other", time.Minute, 0), job("old", "small", 2*time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"other: "}},
		{name: "a high job goes before an older normal one",
			demand: []Demand{job("deploy", "other", time.Minute, 1), job("build", "small", 20*time.Minute, 0)},
			starts: []string{"other"}, defers: []string{"small: "}},
		{name: "but not before one that has waited longer than the head start",
			demand: []Demand{job("deploy", "other", time.Minute, 1), job("build", "small", 40*time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"other: high by rule 1"}},
		{name: "a low job waits behind newer normal ones",
			demand: []Demand{job("nightly", "other", 20*time.Minute, -1), job("build", "small", time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"other: low by rule 1"}},
		{name: "and is not starved: after the head start it is first again",
			demand: []Demand{job("nightly", "other", 40*time.Minute, -1), job("build", "small", time.Minute, 0)},
			starts: []string{"other"}, defers: []string{"small: "}},
		{name: "jobs before a high job in its class move up with it: GitHub serves the class oldest first",
			demand: []Demand{job("lint", "small", 3*time.Minute, 0), job("deploy", "small", time.Minute, 1), job("build", "other", 10*time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"small: high by rule 1", "other: "}},
		{name: "room for everything: levels change nothing",
			host:   Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 64 * gb},
			demand: []Demand{job("nightly", "other", time.Minute, -1), job("deploy", "small", time.Minute, 1), job("build", "small", time.Minute, 0)},
			starts: []string{"small", "small", "other"}},
		{name: "a high job waiting for room keeps that host from lower jobs",
			host:    Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 24 * gb},
			runners: []Runner{bigBusy},
			demand:  []Demand{job("release", "big", time.Minute, 1), job("build", "small", 5*time.Minute, 0)},
			defers:  []string{"big: high by rule 1", "small: "}, reason: "no host with 2.0cpu/16.0G free"},
		{name: "but not a host it would not fit with every runner gone",
			host:   Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 12 * gb},
			demand: []Demand{job("release", "big", time.Minute, 1), job("build", "small", 5*time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"big: high by rule 1"}},
		{name: "jobs of the same level still fill the gap",
			host:   Host{Name: "h1", Kind: "server", Up: true, CPUs: 16, MemBytes: 12 * gb},
			demand: []Demand{job("release", "big", 10*time.Minute, 0), job("build", "small", 5*time.Minute, 0)},
			starts: []string{"small"}, defers: []string{"big: "}},
		{name: "a running job is not touched for a high one",
			demand: []Demand{job("deploy", "other", time.Minute, 1)}, runners: []Runner{busy},
			defers: []string{"other: high by rule 1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.host
			if h.Name == "" {
				h = tight()
			}
			s := snap([]Host{h}, tt.demand, tt.runners)
			s.Now, s.Classes, s.HeadStart = t0, []Class{small, other, big}, 30*time.Minute
			p := Decide(s)
			var gotStarts, gotDefers []string
			for _, st := range p.Start {
				gotStarts = append(gotStarts, st.Class)
			}
			for _, d := range p.Defer {
				gotDefers = append(gotDefers, d.Class+": "+d.Priority)
			}
			if !reflect.DeepEqual(gotStarts, tt.starts) || !reflect.DeepEqual(gotDefers, tt.defers) || len(p.Stop) != 0 {
				t.Fatalf("starts %v defers %v stops %v\nwant starts %v defers %v", gotStarts, gotDefers, p.Stop, tt.starts, tt.defers)
			}
			if tt.reason != "" && !strings.Contains(p.Defer[0].Reason, tt.reason) {
				t.Errorf("reason %q", p.Defer[0].Reason)
			}
		})
	}
}

func TestInheritedLevelSaysWhichJobPulls(t *testing.T) {
	ds := []Demand{{ID: "a", Name: "lint", Class: "c", FirstSeen: t0}, {ID: "b", Name: "test", Class: "c", FirstSeen: t0.Add(time.Second)},
		{ID: "c", Name: "deploy", Class: "c", FirstSeen: t0.Add(2 * time.Second), Level: 1, Ranked: "high by rule 1 (workflow: deploy-*.yml)"},
		{ID: "d", Name: "docs", Class: "c", FirstSeen: t0.Add(3 * time.Second)}}
	inherit(ds)
	want := "ahead of deploy in its class, which is high by rule 1 (workflow: deploy-*.yml)"
	if ds[0].Level != 1 || ds[1].Level != 1 || ds[0].Ranked != want || ds[1].Ranked != want {
		t.Errorf("before the high job: %+v %+v", ds[0], ds[1])
	}
	if ds[3].Level != 0 || ds[3].Ranked != "" {
		t.Errorf("behind the high job: %+v", ds[3])
	}
}
