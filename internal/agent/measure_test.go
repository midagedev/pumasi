package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/midagedev/pumasi/internal/api"
)

// fakeMeter reports one usage for every runner and a host of mem bytes.
type fakeMeter struct {
	u   Usage
	mem int64
}

func (m *fakeMeter) Runner(Container) (Usage, error) { return m.u, nil }
func (m *fakeMeter) Host() (HostUsage, error) {
	return HostUsage{MemBytes: m.mem, MemAvailable: m.mem / 2}, nil
}

// A runner's memory is never reported above the machine's: on macOS the sum
// of a tree's footprints counted shared memory once per process and came out
// at 113.8G on a 32G machine, and the next estimate of the job was larger
// than any host. The cap holds from the very first sample.
func TestRunnerMemoryIsCappedAtTheMachine(t *testing.T) {
	const gb = int64(1) << 30
	drv := &fakeDriver{cs: map[string]Container{}}
	met := &fakeMeter{mem: 32 * gb, u: Usage{CPUUsec: 1e6, MemCurrent: 113 * gb, MemPeak: 120 * gb}}
	a := New(Config{Host: "mac-a"}, drv, met, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, err := a.Start(ctx, api.StartRequest{Name: "r1", Class: "mac", CPUs: 1, MemBytes: gb, Image: "img", JITConfig: "x"}); err != nil {
		t.Fatal(err)
	}
	a.Sample(ctx)
	r := a.Snapshot().Runners[0]
	if r.MemWork != 32*gb || r.MemWorkPeak != 32*gb || r.MemCurrent != 32*gb || r.MemPeak != 32*gb {
		t.Fatalf("first sample: work %d peak %d current %d memPeak %d, want each at most %d", r.MemWork, r.MemWorkPeak, r.MemCurrent, r.MemPeak, 32*gb)
	}
	// Under the machine's memory nothing changes.
	met.u = Usage{CPUUsec: 2e6, MemCurrent: 5 * gb, MemPeak: 6 * gb, MemWork: 4 * gb}
	a.Sample(ctx)
	if r := a.Snapshot().Runners[0]; r.MemWork != 4*gb || r.MemCurrent != 5*gb {
		t.Fatalf("second sample: %+v", r)
	}
	// And the last sample before removal too.
	met.u.MemWork = 200 * gb
	if got, err := a.Delete(ctx, "r1"); err != nil || got.MemWorkPeak != 32*gb {
		t.Fatalf("delete: peak %d, err %v", got.MemWorkPeak, err)
	}
}
