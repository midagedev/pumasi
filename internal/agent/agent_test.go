package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/cgroup"
)

type fakeDriver struct {
	cs        map[string]Container
	listening map[string]time.Time
}

func (f *fakeDriver) Start(_ context.Context, s Spec) error {
	f.cs[s.Name] = Container{Name: s.Name, Class: s.Class, CPUs: s.CPUs, MemBytes: s.MemBytes, State: "running", Pid: 42}
	return nil
}
func (f *fakeDriver) Remove(_ context.Context, name string) error { delete(f.cs, name); return nil }
func (f *fakeDriver) List(context.Context) ([]Container, error) {
	var out []Container
	for _, c := range f.cs {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeDriver) ListeningAt(_ context.Context, name string) (time.Time, bool, error) {
	t, ok := f.listening[name]
	return t, ok, nil
}

func testAgent(drv Driver) *Agent {
	cg := cgroup.Reader{
		CgroupRoot: filepath.Join("..", "cgroup", "testdata", "cg"),
		ProcRoot:   filepath.Join("..", "cgroup", "testdata", "proc"),
		SysRoot:    filepath.Join("..", "cgroup", "testdata", "sys"),
	}
	return New(Config{Host: "h1", BudgetCPUs: 3, BudgetMem: 4 << 30}, drv, cg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestStartSampleExitDelete(t *testing.T) {
	drv := &fakeDriver{cs: map[string]Container{}, listening: map[string]time.Time{}}
	a := testAgent(drv)
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return t0 }
	ctx := context.Background()

	if _, err := a.Start(ctx, api.StartRequest{Name: "r1", Class: "linux-l", CPUs: 2, MemBytes: 3 << 30, Image: "img", JITConfig: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, api.StartRequest{Name: "r2", Class: "linux-l", CPUs: 2, MemBytes: 1 << 30, Image: "img", JITConfig: "x"}); !errors.Is(err, errOverBudget) {
		t.Fatalf("second runner over the 3 cpu budget: err %v", err)
	}

	drv.listening["r1"] = t0.Add(8 * time.Second)
	a.Sample(ctx)
	s := a.Snapshot()
	if len(s.Runners) != 1 {
		t.Fatalf("runners %+v", s.Runners)
	}
	r := s.Runners[0]
	if r.CPUSec != 571.525387 || r.MemPeak != 95039488 || r.ProvisionMs != 8000 || r.State != "running" {
		t.Fatalf("sampled runner %+v", r)
	}
	if s.PSI["cpu"] != 15.61 || s.NonRunnerMem <= 0 {
		t.Fatalf("host %+v", s)
	}

	// The container exits: its cgroup is gone, the last values must stay.
	c := drv.cs["r1"]
	c.State, c.Pid, c.ExitCode = "exited", 0, 137
	drv.cs["r1"] = c
	a.Sample(ctx)
	r = a.Snapshot().Runners[0]
	if r.State != "exited" || r.CPUSec != 571.525387 || r.MemPeak != 95039488 || *r.ExitCode != 137 {
		t.Fatalf("after exit %+v", r)
	}

	got, err := a.Delete(ctx, "r1")
	if err != nil || got.MemPeak != 95039488 {
		t.Fatalf("delete %+v %v", got, err)
	}
	if len(a.Snapshot().Runners) != 0 {
		t.Fatal("runner still listed after delete")
	}
}

func TestParseListening(t *testing.T) {
	logs := "2026-11-25T01:00:01.5Z √ Connected to GitHub\n" +
		"2026-11-25T01:00:09.25Z 2026-11-25 01:00:09Z: Listening for Jobs\n"
	at, ok, err := parseListening(logs)
	if err != nil || !ok || !at.Equal(time.Date(2026, 11, 25, 1, 0, 9, 250e6, time.UTC)) {
		t.Fatalf("%v %v %v", at, ok, err)
	}
	if _, ok, _ := parseListening("2026-11-25T01:00:01Z Connected\n"); ok {
		t.Fatal("no listening line: want false")
	}
}

func TestParseInspect(t *testing.T) {
	js := `[{"Id":"abc","Name":"/r1","State":{"Status":"exited","Pid":0,"ExitCode":0,
	"StartedAt":"2026-11-25T01:00:00.1Z","FinishedAt":"0001-01-01T00:00:00Z"},
	"Config":{"Labels":{"pumasi":"m1","pumasi.class":"linux-s","pumasi.cpus":"1","pumasi.mem":"1073741824"}}}]`
	ins, err := parseInspect([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	c := ins[0].container()
	if c.Name != "r1" || c.Class != "linux-s" || c.CPUs != 1 || c.MemBytes != 1<<30 || c.State != "exited" || !c.FinishedAt.IsZero() {
		t.Fatalf("%+v", c)
	}
}

func TestMetrics(t *testing.T) {
	var b strings.Builder
	WriteMetrics(&b, api.Snapshot{Host: "h1", PSI: map[string]float64{"cpu": 1.5}, Runners: []api.Runner{
		{Name: "r1", Class: "linux-s", CPUSec: 2.5, MemPeak: 100, PressureSec: map[string]float64{"io": 0.25}},
	}})
	out := b.String()
	for _, want := range []string{
		`pumasi_runner_cpu_seconds_total{host="h1",class="linux-s",runner="r1"} 2.5`,
		`pumasi_runner_memory_peak_bytes{host="h1",class="linux-s",runner="r1"} 100`,
		`pumasi_runner_pressure_seconds_total{host="h1",class="linux-s",runner="r1",resource="io"} 0.25`,
		`pumasi_host_pressure_avg10{host="h1",resource="cpu"} 1.5`,
		"# EOF\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
