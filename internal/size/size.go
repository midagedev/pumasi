// Package size turns what a job used the last times it ran into what it is
// expected to use the next time. Nobody keeps these numbers by hand: a job
// that grows is seen growing, and the next placement is made with the new size.
package size

import (
	"fmt"
	"math"
	"time"
)

// Sample is one finished run of a job.
type Sample struct {
	// CPUs: cores used on average while the job ran (CPU seconds / seconds).
	CPUs float64
	// MemBytes: the most memory it held that was not page cache.
	MemBytes int64
	// OOM: the run was cut short at a memory limit. What it needed is more
	// than what was seen.
	OOM bool
}

// Estimate is what the next run of a job is expected to use.
type Estimate struct {
	CPUs     float64
	MemBytes int64
	// Runs: how many finished runs it was learned from (0 = none: it is the
	// class's first estimate).
	Runs int
	// The largest of those runs, before the margin.
	TopCPUs float64
	TopMem  int64
	// Borrowed: the job has no run of its own; the size is the largest known
	// of its workflow's other jobs (Top*), never under the class's first estimate.
	Borrowed bool
	// Blind: nothing is measured of the job nor of its workflow. The size is
	// the class's first estimate, which is a guess: see BlindHold.
	Blind bool
}

const (
	// Window: the newest runs that count. A job that shrank is believed after
	// this many smaller runs; one that grew is believed at once.
	Window = 10
	// Margin over the largest run of the window: runs differ, and a sample
	// every few seconds misses short peaks.
	Margin = 1.2
	// A run cut short at its limit says only "more than this".
	OOMMargin = 1.5

	MinCPUs = 0.1
	MinMem  = 256 << 20

	// BlindHold: a runner whose job is blind holds this many times the first
	// estimate of memory while it is watched, so that jobs nobody has measured
	// are not put side by side as if the guess were true.
	BlindHold = 3
	// Settle: how long a blind job is watched. After that its own use is what
	// it holds, and what its workflow's other jobs are sized by.
	Settle = 3 * time.Minute
)

// Borrow is the estimate of a job without a run of its own whose workflow has
// measured jobs: as large as the largest of them, and no smaller than first.
func Borrow(first, known Estimate) Estimate {
	return Estimate{CPUs: math.Max(first.CPUs, known.CPUs), MemBytes: max(first.MemBytes, known.MemBytes),
		TopCPUs: known.TopCPUs, TopMem: known.TopMem, Borrowed: true}
}

// Watched reports whether a runner started with a blind estimate is still
// held at BlindHold: until its job has run for Settle (started zero = no job yet).
func (e Estimate) Watched(started, now time.Time) bool {
	return e.Blind && (started.IsZero() || now.Sub(started) < Settle)
}

// Learn is the estimate from a job's newest runs (any order; at most Window
// are passed). With no runs it is first.
func Learn(runs []Sample, first Estimate) Estimate {
	if len(runs) == 0 {
		first.Runs = 0
		return first
	}
	var e Estimate
	var cpu, mem float64
	for _, r := range runs {
		m := float64(r.MemBytes)
		e.TopCPUs, e.TopMem = math.Max(e.TopCPUs, r.CPUs), max(e.TopMem, r.MemBytes)
		if r.OOM {
			m *= OOMMargin / Margin
		}
		cpu, mem = math.Max(cpu, r.CPUs), math.Max(mem, m)
	}
	e.Runs = len(runs)
	e.CPUs = math.Max(MinCPUs, math.Ceil(cpu*Margin*10)/10)             // to a tenth of a core
	e.MemBytes = max(MinMem, int64(math.Ceil(mem*Margin/(64<<20)))<<26) // to 64M
	return e
}

// Charge is what a runner holds of its host's room: what it is expected to
// use, or what it uses now when that is more. Going over the estimate is not
// an error and stops nothing; it only takes the room away from the next runner.
//
// watched: the estimate is blind and the job has not run long enough to
// show its size; it holds BlindHold times the guess of memory. Not of cpu:
// a busy cpu slows jobs, it breaks nothing.
func Charge(est Estimate, cpuNow float64, memNow int64, watched bool) (cpus float64, mem int64) {
	hold := est.MemBytes
	if watched {
		hold *= BlindHold
	}
	return math.Max(est.CPUs, cpuNow), max(hold, memNow)
}

// Source says where the estimate comes from, for the reason of a start.
func (e Estimate) Source() string {
	switch {
	case e.Borrowed:
		return fmt.Sprintf("no measured run of this job yet; sized like the largest known of its workflow's other jobs, %.1fcpu/%s", e.TopCPUs, gib(e.TopMem))
	case e.Runs == 0 && e.Blind:
		return "the class's first estimate; nothing is measured of this job or its workflow yet"
	}
	switch e.Runs {
	case 0:
		return "the class's first estimate, no measured run of this job yet"
	case 1:
		return fmt.Sprintf("learned from 1 run of %.1fcpu/%s", e.TopCPUs, gib(e.TopMem))
	}
	return fmt.Sprintf("learned from %d runs, the largest %.1fcpu/%s", e.Runs, e.TopCPUs, gib(e.TopMem))
}

// Moved reports whether b differs from a enough to tell people: a quarter in
// memory, or a quarter and half a core in cpu.
func Moved(a, b Estimate) bool {
	far := func(x, y float64) bool { return math.Abs(x-y) > 0.25*math.Max(math.Min(x, y), 1e-9) }
	return far(float64(a.MemBytes), float64(b.MemBytes)) || (far(a.CPUs, b.CPUs) && math.Abs(a.CPUs-b.CPUs) >= 0.5)
}

func gib(b int64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }
