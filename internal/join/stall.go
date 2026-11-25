package join

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Watchdog notices when this process did not get to run: a host so loaded
// that the agent stalls, or a machine that slept. Either one makes the
// controller's pings go unanswered, and "keepalive timeout" alone sends the
// operator looking at the network.
type Watchdog struct {
	Log *slog.Logger

	mu   sync.Mutex
	what string // "stalled" or "asleep"
	long time.Duration
	at   time.Time
}

const (
	watchEvery = time.Second
	watchSlack = 5 * time.Second  // a gap this long is worth a line
	watchKeep  = 45 * time.Second // how long a gap explains a dropped connection
)

// Run ticks until ctx ends.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		w.saw(last, now)
		last = now
	}
}

// saw compares two readings of the clock one tick apart. The monotonic clock
// stands still while the machine sleeps; the wall clock does not.
func (w *Watchdog) saw(last, now time.Time) {
	w.sawClocks(now.Sub(last), now.Round(0).Sub(last.Round(0)), now)
}

// sawClocks judges one tick: how far each clock moved.
func (w *Watchdog) sawClocks(mono, wall time.Duration, now time.Time) {
	ran := mono - watchEvery
	slept := wall - mono
	what, long := "", time.Duration(0)
	switch {
	case slept >= watchSlack:
		what, long = "asleep", slept
	case ran >= watchSlack:
		what, long = "stalled", ran
	default:
		return
	}
	w.mu.Lock()
	w.what, w.long, w.at = what, long.Round(time.Second), now
	w.mu.Unlock()
	if w.Log == nil {
		return
	}
	if what == "asleep" {
		w.Log.Info("this machine was asleep", "for", long.Round(time.Second))
	} else {
		w.Log.Warn("the agent did not get to run; the host is overloaded (cpu, or memory so short that it swaps)", "for", long.Round(time.Second))
	}
}

// Explain adds what the watchdog saw a moment ago to why a connection ended.
func (w *Watchdog) Explain(why string) string {
	if w == nil || why != WhyKeepalive {
		return why
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.what == "" || time.Since(w.at) > watchKeep {
		return why
	}
	if w.what == "asleep" {
		return fmt.Sprintf("%s (the host was asleep for %s)", why, w.long)
	}
	return fmt.Sprintf("%s (the agent did not get to run for %s: the host is overloaded or was suspended)", why, w.long)
}
