// Package size turns what a job used the last times it ran into what it is
// expected to use the next time. Nobody keeps these numbers by hand: a job
// that grows is seen growing, and the next placement is made with the new size.
package size

import (
	"fmt"
	"math"
	"slices"
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
	// Host is where it ran ("" = not known), for the caller to fill HostMem.
	Host string
	// HostMem: the physical memory of that host (0 = not known). No run holds
	// more than its machine has: a sample above it is a measurement that
	// counted something twice, and Learn leaves it out.
	HostMem int64
	// Secs: how long it ran, seconds (0 = not known).
	Secs float64
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
	// Ignored: runs left out because they held more memory than their host
	// has (see Sample.HostMem).
	Ignored int
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
//
// A run that held more memory than its host has (HostMem known) is left out:
// that is not what the job used but a sum that counted shared memory once per
// process (macOS), and one such run would size the job larger than any host
// for a whole window. A run cut short at its limit is kept, at most at the
// host's memory: it did run out of something.
func Learn(runs []Sample, first Estimate) Estimate {
	ignored := 0
	kept := make([]Sample, 0, len(runs))
	for _, r := range runs {
		if r.HostMem > 0 && r.MemBytes > r.HostMem {
			if !r.OOM {
				ignored++
				continue
			}
			r.MemBytes = r.HostMem
		}
		kept = append(kept, r)
	}
	runs = kept
	if len(runs) == 0 {
		first.Runs, first.Ignored = 0, ignored
		return first
	}
	e := Estimate{Ignored: ignored}
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

// Runtime is how long the next run of a job is expected to take: the median
// of its newest runs' times (any order). ok = false: no run has a time.
// Not the largest, as for its size: a wait told from it should be as often
// early as late.
func Runtime(runs []Sample) (d time.Duration, ok bool) {
	var secs []float64
	for _, r := range runs {
		if r.Secs > 0 {
			secs = append(secs, r.Secs)
		}
	}
	if len(secs) == 0 {
		return 0, false
	}
	slices.Sort(secs)
	m := secs[len(secs)/2]
	if len(secs)%2 == 0 {
		m = (secs[len(secs)/2-1] + m) / 2
	}
	return time.Duration(m * float64(time.Second)), true
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
	s := e.source()
	switch {
	case e.Ignored == 1:
		s += "; 1 run that held more than its host's memory left out"
	case e.Ignored > 1:
		s += fmt.Sprintf("; %d runs that held more than their host's memory left out", e.Ignored)
	}
	return s
}

func (e Estimate) source() string {
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
