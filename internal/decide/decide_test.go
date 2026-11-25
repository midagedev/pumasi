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
			{Name: "a", Class: "linux-l", Host: "server1", State: "busy", Container: "running"},
			{Name: "b", Class: "linux-l", Host: "server1", State: "busy", Container: "running"},
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
		{"busy runner does not cover", snap([]Host{server()}, demand("linux-s", 1), []Runner{
			{Name: "a", Class: "linux-s", Host: "server1", State: "busy", Container: "running"},
		}), []string{"linux-s@server1"}, nil},
		{"exited runner frees capacity", snap([]Host{server()}, demand("linux-l", 1), []Runner{
			{Name: "a", Class: "linux-l", Host: "server1", State: "busy", Container: "exited"},
			{Name: "b", Class: "linux-l", Host: "server1", State: "busy", Container: "running"},
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
		{"exited during job keeps state", server(), r("busy", "exited"), time.Minute, "|exited during a job"},
		{"busy runs on", server(), r("busy", "running"), time.Hour, ""},
		{"job completed", server(), r("done", "running"), time.Minute, "|job completed"},
		{"orphan container", server(), r("", "running"), time.Minute, "|not in the ledger"},
		{"container missing", server(), r("idle", ""), time.Minute, "failed|container missing"},
		{"idle within timeout", server(), listening, 4 * time.Minute, ""},
		{"idle timeout", server(), listening, 6 * time.Minute, "failed|idle timeout"},
		{"host briefly down", shortDown, r("busy", ""), time.Minute, ""},
		{"host lost", longDown, r("busy", ""), time.Minute, "lost|host server1 unreachable"},
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

func TestDeferReasonNamesEachHost(t *testing.T) {
	full := server()
	full.BudgetCPUs = 1
	p := Decide(snap([]Host{full, desktop()}, demand("linux-l", 4), nil))
	if len(p.Defer) != 1 {
		t.Fatalf("defers %+v", p.Defer)
	}
	for _, want := range []string{"linux-l: no host with 2.0cpu/3.0G free", "server1: free 1.0cpu/", "desk1: free 0.0cpu/"} {
		if !strings.Contains(p.Defer[0].Reason, want) {
			t.Errorf("reason %q lacks %q", p.Defer[0].Reason, want)
		}
	}
}
