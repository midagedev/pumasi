package watch

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// First come, first served on a pool of cores: a job waits while the one
// before it waits, and for the cores it needs.
func TestFIFO(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	w := fifo([]job{
		{at: at(0), sec: 600, cores: 3},
		{at: at(1), sec: 60, cores: 2}, // 1 free: waits for the first to end at 10m
		{at: at(2), sec: 60, cores: 1}, // fits, but the one before it waits: it waits too
		{at: at(20), sec: 60, cores: 4},
	}, 4)
	want := []float64{0, 540, 480, 0}
	for i := range want {
		if math.Abs(w[i]-want[i]) > 1e-6 {
			t.Fatalf("waits %v, want %v", w, want)
		}
	}
}

// Weeks of a CI that grows 10% a week: the growth is read back, the waits
// grow as the cores fill, and a week the ledger does not reach is left out.
func TestForecastReadsGrowth(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	var runs []ledger.JobRun
	id := int64(0)
	for d := 0; d < 7*7; d++ { // seven weeks; the forecast asks for eight
		day := now.AddDate(0, 0, -49+d)
		n := int(math.Round(20 * math.Pow(1.1, float64(d)/7)))
		for k := range n {
			id++
			start := day.Add(9*time.Hour + time.Duration(k)*8*time.Hour/time.Duration(n))
			for j := range 4 { // a run brings four jobs at once
				runs = append(runs, ledger.JobRun{Runner: "r", RunID: id, Job: string(rune('a' + j)), QueuedAt: start, StartedAt: start,
					FinishedAt: start.Add(10 * time.Minute), RunSec: 600, CPUSec: 1200, Result: "succeeded"})
			}
		}
	}
	f := Forecast(runs, 0, now, 8)
	if !f.Weeks[0].Partial || f.Weeks[1].Partial || f.Fitted != 7 || math.Abs(f.Growth-0.10) > 0.02 || f.GrowthLow > f.Growth || f.GrowthHigh < f.Growth {
		t.Fatalf("growth %+.3f (%+.3f..%+.3f) of %d weeks, partial %v", f.Growth, f.GrowthLow, f.GrowthHigh, f.Fitted, f.Weeks[0].Partial)
	}
	if f.FullIn != -1 || strings.Contains(f.Words, "cores") {
		t.Fatalf("no cores, but: %s", f.Words)
	}
	// The busiest hours keep about 2 cores × 4 jobs × 3 runs an hour /
	// (60/10) busy; on 24 cores the waits come as runs grow.
	f = Forecast(runs, 24, now, 8)
	if f.Load <= 0.2 || f.Load >= 1 || f.FullIn <= 0 || f.FullSoon > f.FullIn || (f.FullLate >= 0 && f.FullLate < f.FullIn) {
		t.Fatalf("load %.2f full in %.1f (%.1f..%.1f)", f.Load, f.FullIn, f.FullSoon, f.FullLate)
	}
	if f.WaitTenthIn < 0 || !strings.Contains(f.Words, "need every core in ~") {
		t.Fatalf("%s", f.Words)
	}
	// Six cores: a run wants eight at once, so its last job waits.
	f = Forecast(runs, 6, now, 8)
	if f.WaitShare < 0.1 || f.WaitTenthIn != 0 {
		t.Fatalf("6 cores: share %.2f, tenth in %.1f: %s", f.WaitShare, f.WaitTenthIn, f.Words)
	}
}
