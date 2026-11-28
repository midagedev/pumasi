package watch

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// Capacity forecast: how fast CI's use of the hosts grows, and at that rate
// when the busiest hours need every core and when waiting for room doubles.
// The waits come from replaying the last week's jobs — their times, sizes and
// the way a workflow run brings its jobs all at once — on the cores, with more
// runs in it as CI grows. (A model of jobs coming one at a time at random,
// Erlang C, said 11% of the busiest hours' jobs waited where a queue
// simulation of made-up CI had 53%: a run brings all its jobs at once.)

// ForecastWeek is one 7-day window, the last one ending now.
type ForecastWeek struct {
	From     time.Time `json:"from"`
	Jobs     int       `json:"jobs"`
	CPUHours float64   `json:"cpuHours"`
	// PeakCores: the busiest hours' cores in use, the 95th percentile of the
	// window's hourly averages (cpu-seconds spread over each job's time).
	PeakCores float64 `json:"peakCores"`
	WaitP90   float64 `json:"waitP90"` // seconds from waiting to running, every job
	// Partial: the ledger begins after the week does; not in the growth.
	Partial bool `json:"partial,omitempty"`
}

// ForecastReport is `pumasi forecast` and /api/forecast.
type ForecastReport struct {
	Now   time.Time      `json:"now"`
	Weeks []ForecastWeek `json:"weeks"` // the oldest first
	// Growth: cpu-hours a week grow by this share a week (Theil–Sen slope on
	// the logs of the weeks; 0 with fewer than 3 weeks of jobs). GrowthLow
	// and GrowthHigh: its 80% interval (Kendall), so as many weeks of a team
	// whose merges come at random say this much and no more.
	Growth float64 `json:"growth"`
	Fitted int     `json:"fitted"` // the weeks the growth is of
	// EndsEarly: the ledger's last job finished more than two days before
	// it was asked; the weeks end at that job (Now).
	EndsEarly  bool    `json:"endsEarly,omitempty"`
	GrowthLow  float64 `json:"growthLow"`
	GrowthHigh float64 `json:"growthHigh"`
	// Cores: the hosts' cores for CI (0 = not known: no forecast), and where
	// that number came from.
	Cores     float64 `json:"cores"`
	CoresFrom string  `json:"coresFrom,omitempty"`
	// Physical: Cores are the hosts' own (each its budget, else its cores),
	// not counted again where runners share them, as the map counts them.
	Physical bool `json:"physical,omitempty"`
	// Load: the last week's PeakCores against Cores.
	Load float64 `json:"load"`
	// FullIn: weeks until the busiest hours need every core, at Growth.
	// WaitNow: the mean wait for room in the replay of the last week, and
	// WaitShare the share of its jobs that waited a minute or more.
	// WaitTenthIn: weeks until one job in ten waits a minute or more;
	// DoubleIn: weeks until the mean wait for room doubles (while it is
	// under 5 seconds, doubling says little: -1). Each is -1 when it does not
	// come within a year (or CI is not growing), 0 when it is already so.
	FullIn      float64 `json:"fullIn"`
	WaitNow     float64 `json:"waitNow"`
	WaitShare   float64 `json:"waitShare"`
	WaitTenthIn float64 `json:"waitTenthIn"`
	DoubleIn    float64 `json:"doubleIn"`
	// FullSoon/FullLate, DoubleSoon/DoubleLate: the same at GrowthHigh and
	// GrowthLow (-1: not within a year at that growth).
	FullSoon   float64 `json:"fullSoon"`
	FullLate   float64 `json:"fullLate"`
	DoubleSoon float64 `json:"doubleSoon"`
	DoubleLate float64 `json:"doubleLate"`
	Words      string  `json:"words"`
	Note       string  `json:"note"`
}

// Forecast reads the last weeks (3 or more) of runs before now. cores is the
// hosts' cores for CI; 0 gives the growth alone.
func Forecast(runs []ledger.JobRun, cores float64, now time.Time, weeks int) ForecastReport {
	weeks = max(weeks, 3)
	// A ledger whose last job is days old (a copy, a controller that was
	// off): the forecast ends at that job.
	var lastEnd time.Time
	for _, r := range runs {
		if r.FinishedAt.After(lastEnd) && !r.FinishedAt.After(now) {
			lastEnd = r.FinishedAt
		}
	}
	endsEarly := !lastEnd.IsZero() && now.Sub(lastEnd) > 48*time.Hour
	if endsEarly {
		now = lastEnd
	}
	rep := ForecastReport{Now: now, Cores: cores, FullIn: -1, WaitTenthIn: -1, DoubleIn: -1, FullSoon: -1, FullLate: -1, DoubleSoon: -1, DoubleLate: -1,
		Note: "growth is the Theil–Sen slope of the weekly cpu-hours' logs; the busiest hours are the 95th percentile of hourly cores in use; " +
			"the waits replay the last week's jobs, first come first served, each taking the cores it used, on all the cores as one pool, " +
			"with runs added at the times runs came as CI grows: hosts of mixed sizes, memory, and pumasi sizing a job by its largest " +
			"recent run make real waits come sooner"}
	start := now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour)
	hours := map[int64]float64{} // hour -> core-seconds
	week := func(t time.Time) int { return int(t.Sub(start) / (7 * 24 * time.Hour)) }
	rep.Weeks = make([]ForecastWeek, weeks)
	waits := make([][]float64, weeks)
	for i := range rep.Weeks {
		rep.Weeks[i].From = start.Add(time.Duration(i) * 7 * 24 * time.Hour)
	}
	for _, r := range runs {
		if r.StartedAt.Before(start) || !r.StartedAt.Before(now) || r.RunSec <= 0 {
			continue
		}
		w := week(r.StartedAt)
		rep.Weeks[w].Jobs++
		rep.Weeks[w].CPUHours += r.CPUSec / 3600
		waits[w] = append(waits[w], r.WaitSec)
		c := r.CPUSec / r.RunSec
		for t := r.StartedAt; t.Before(r.FinishedAt); {
			h := t.Truncate(time.Hour)
			next := h.Add(time.Hour)
			if next.After(r.FinishedAt) {
				next = r.FinishedAt
			}
			hours[h.Unix()] += c * next.Sub(t).Seconds()
			t = next
		}
	}
	perWeek := make([][]float64, weeks)
	for i := range perWeek {
		perWeek[i] = make([]float64, 7*24) // hours with no job count as idle
	}
	for h, cs := range hours {
		t := time.Unix(h, 0)
		if t.Before(start) || !t.Before(now) {
			continue
		}
		w := week(t)
		if k := int(t.Sub(rep.Weeks[w].From) / time.Hour); k < len(perWeek[w]) {
			perWeek[w][k] = cs / 3600
		}
	}
	// A week the ledger does not reach the start of is shown but not fitted.
	first := now
	for _, r := range runs {
		if r.StartedAt.Before(first) {
			first = r.StartedAt
		}
	}
	var logs []float64
	for i := range rep.Weeks {
		rep.Weeks[i].Partial = rep.Weeks[i].From.Before(first.Add(-12 * time.Hour))
		s := slices.Clone(perWeek[i])
		slices.Sort(s)
		rep.Weeks[i].PeakCores = Percentile(s, 0.95)
		if len(waits[i]) > 0 {
			slices.Sort(waits[i])
			rep.Weeks[i].WaitP90 = Percentile(waits[i], 0.9)
		}
		switch {
		case rep.Weeks[i].Partial:
		case rep.Weeks[i].CPUHours > 0:
			logs = append(logs, math.Log(rep.Weeks[i].CPUHours))
		default:
			logs = nil // a week with no jobs: the ledger has a hole, no trend
		}
	}
	rep.Fitted = len(logs)
	rep.EndsEarly = endsEarly
	if len(logs) >= 3 {
		m, lo, hi := theilSenCI(logs, 1.2816)
		rep.Growth, rep.GrowthLow, rep.GrowthHigh = math.Exp(m)-1, math.Exp(lo)-1, math.Exp(hi)-1
	}
	last := rep.Weeks[weeks-1]
	if cores <= 0 || last.PeakCores <= 0 {
		rep.Words = rep.growthWords()
		return rep
	}
	rep.Load = last.PeakCores / cores
	weeksAt := func(g, factor float64) float64 { // weeks until CI is factor times today's at g
		switch {
		case factor <= 1:
			return 0
		case g <= 0 || math.IsInf(factor, 1):
			return -1
		}
		w := math.Log(factor) / math.Log(1+g)
		if w > 52 {
			return -1
		}
		return w
	}
	// Replay the last week with more runs: when its busiest hours need every
	// core (more runs smooth the hours out, so the busiest grow slower than
	// the total), and its waits.
	rp := newReplay(runs, now.Add(-7*24*time.Hour), now, cores)
	full := rp.grow(func(r replayed) bool { return r.peak >= cores })
	if math.IsInf(full, 1) {
		// Past the replay's reach the hours are smooth and the busiest grow
		// with the runs: scale them up from there (on a made-up week at 11% of
		// its cores, within 3% of a replay run on to the full cores).
		if p := rp.at(growMax).peak; p > 0 {
			full = growMax * cores / p
		}
	}
	rep.FullIn, rep.FullSoon, rep.FullLate = weeksAt(rep.Growth, full), weeksAt(rep.GrowthHigh, full), weeksAt(rep.GrowthLow, full)
	now1 := rp.at(1)
	rep.WaitNow, rep.WaitShare = now1.mean, now1.share
	tenth := rp.grow(func(r replayed) bool { return r.share >= 0.10 })
	rep.WaitTenthIn = weeksAt(rep.Growth, tenth)
	if rep.WaitNow >= 5 {
		w0 := rep.WaitNow
		double := rp.grow(func(r replayed) bool { return r.mean >= 2*w0 })
		rep.DoubleIn, rep.DoubleSoon, rep.DoubleLate = weeksAt(rep.Growth, double), weeksAt(rep.GrowthHigh, double), weeksAt(rep.GrowthLow, double)
	}
	rep.Words = rep.growthWords() + " " + rep.capacityWords()
	return rep
}

// SetCores says where Cores came from, and whether they are the hosts'
// physical cores, and words the forecast again.
func (r *ForecastReport) SetCores(from string, physical bool) {
	r.CoresFrom, r.Physical = from, physical
	if r.Cores > 0 && r.Load > 0 {
		r.Words = r.growthWords() + " " + r.capacityWords()
	}
}

func (r ForecastReport) growthWords() string {
	n := len(r.Weeks)
	pre := ""
	if r.EndsEarly {
		pre = fmt.Sprintf("The ledger's last job finished %s; the weeks end there. ", r.Now.Local().Format("Mon Jan 2 15:04"))
	}
	return pre + r.growthSentence(n)
}

func (r ForecastReport) growthSentence(n int) string {
	if r.Fitted < 3 {
		return fmt.Sprintf("CI used %.0f cpu-hours in the last 7 days; %d whole weeks of jobs in the ledger, too few to tell a trend (3 or more).", r.Weeks[n-1].CPUHours, r.Fitted)
	}
	return fmt.Sprintf("CI used %.0f cpu-hours in the last 7 days (%.0f the week before); over the last %d weeks it grew %s a week (%s to %s, 80%% interval).",
		r.Weeks[n-1].CPUHours, r.Weeks[n-2].CPUHours, r.Fitted, PctSigned(r.Growth), PctSigned(r.GrowthLow), PctSigned(r.GrowthHigh))
}

func (r ForecastReport) capacityWords() string {
	cores := "cores"
	if r.Physical {
		cores = "physical cores"
	}
	s := fmt.Sprintf("At the busiest hours it keeps %.0f of the %.0f %s busy (%.0f%%)", r.Weeks[len(r.Weeks)-1].PeakCores, r.Cores, cores, 100*r.Load)
	switch {
	case r.FullIn == 0:
		return s + fmt.Sprintf(": the busiest hours already need every core; %s of jobs wait a minute or more for room.", oddsText(r.WaitShare))
	case r.FullIn < 0:
		return s + fmt.Sprintf(", and at this rate they do not need every core within a year (%s of jobs wait a minute or more for room now).", oddsText(r.WaitShare))
	}
	s += fmt.Sprintf("; at this rate they need every core in %s%s", weeksText(r.FullIn), span(r.FullSoon, r.FullLate))
	if r.DoubleIn > 0 {
		s += fmt.Sprintf(", and the wait for room doubles in %s%s", weeksText(r.DoubleIn), span(r.DoubleSoon, r.DoubleLate))
	}
	switch {
	case r.WaitTenthIn > 0:
		s += fmt.Sprintf("; one job in ten waits a minute or more for room in %s (now %s, %s on average)", weeksText(r.WaitTenthIn), oddsText(r.WaitShare), secsText(r.WaitNow))
	case r.WaitTenthIn == 0:
		s += fmt.Sprintf("; already %s of jobs wait a minute or more for room (%s on average)", oddsText(r.WaitShare), secsText(r.WaitNow))
	}
	return s + "."
}

// span: " (5 to 14 weeks)" for the interval's ends, soon first.
func span(soon, late float64) string {
	if soon < 0 {
		return ""
	}
	if late < 0 || late > 52 {
		return fmt.Sprintf(" (%.0f weeks or more)", soon)
	}
	return fmt.Sprintf(" (%.0f to %.0f weeks)", soon, late)
}

func secsText(sec float64) string {
	return (time.Duration(sec) * time.Second).Round(time.Second).String()
}

func weeksText(w float64) string {
	switch {
	case w < 1:
		return fmt.Sprintf("~%.0f days", math.Max(1, 7*w))
	case w > 52:
		return "more than a year"
	}
	return fmt.Sprintf("~%.0f weeks", w)
}

func oddsText(p float64) string {
	switch {
	case p < 0.001:
		return "almost none"
	case p < 0.01:
		return "under 1%"
	}
	return fmt.Sprintf("%.0f%%", 100*p)
}

// theilSenCI: the Theil–Sen slope of x against its index, and the interval
// around it that Kendall's tau gives at z (1.28 = 80%).
func theilSenCI(x []float64, z float64) (m, lo, hi float64) {
	n := len(x)
	var sl []float64
	for i := range n {
		for j := i + 1; j < n; j++ {
			sl = append(sl, (x[j]-x[i])/float64(j-i))
		}
	}
	if len(sl) == 0 {
		return 0, 0, 0
	}
	slices.Sort(sl)
	m = median(slices.Clone(sl))
	c := z * math.Sqrt(float64(n*(n-1)*(2*n+5))/18)
	N := float64(len(sl))
	l := int(math.Floor((N-c)/2)) - 1
	u := int(math.Ceil((N + c) / 2))
	lo, hi = sl[max(0, min(len(sl)-1, l))], sl[max(0, min(len(sl)-1, u))]
	return m, lo, hi
}

// replay is a week of jobs to run again on a pool of cores, with more runs.
type replay struct {
	cores   float64
	batches [][]job // a workflow run's jobs, in the order they came
	times   []time.Time
	from    time.Time
}

type job struct {
	at    time.Time // came, as an offset from its batch's first
	sec   float64
	cores float64
}

func newReplay(runs []ledger.JobRun, from, to time.Time, cores float64) *replay {
	rp := &replay{cores: cores, from: from}
	idx := map[int64]int{}
	for _, r := range runs {
		came := r.QueuedAt
		if came.IsZero() {
			came = r.StartedAt
		}
		if came.Before(from) || !came.Before(to) || r.RunSec <= 0 {
			continue
		}
		c := math.Min(math.Max(r.CPUSec/r.RunSec, 0.1), cores)
		i, ok := idx[r.RunID]
		if !ok {
			i = len(rp.batches)
			idx[r.RunID] = i
			rp.batches = append(rp.batches, nil)
		}
		rp.batches[i] = append(rp.batches[i], job{at: came, sec: r.RunSec, cores: c})
	}
	for i, b := range rp.batches {
		slices.SortFunc(b, func(x, y job) int { return x.at.Compare(y.at) })
		rp.times = append(rp.times, b[0].at)
		_ = i
	}
	return rp
}

type replayed struct {
	mean, share float64 // the wait for room, mean seconds; the share a minute or more
	peak        float64 // the busiest hours' cores wanted (95th percentile of the hours)
}

// at: the replay with factor times the runs (each run once, and the extra
// ones copies of runs chosen at random, each at the time another run came).
func (rp *replay) at(factor float64) replayed {
	if len(rp.batches) == 0 {
		return replayed{}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	var jobs []job
	add := func(b []job, start time.Time) {
		for _, j := range b {
			jobs = append(jobs, job{at: start.Add(j.at.Sub(b[0].at)), sec: j.sec, cores: j.cores})
		}
	}
	for _, b := range rp.batches {
		add(b, b[0].at)
	}
	extra := int(math.Round((factor - 1) * float64(len(rp.batches))))
	for range extra {
		b := rp.batches[rng.IntN(len(rp.batches))]
		t := rp.times[rng.IntN(len(rp.times))].Add(time.Duration(rng.NormFloat64() * float64(time.Hour)))
		add(b, t)
	}
	slices.SortStableFunc(jobs, func(x, y job) int { return x.at.Compare(y.at) })
	var r replayed
	// The cores wanted each hour, had every job started when it came.
	hours := make([]float64, 7*24)
	for _, j := range jobs {
		end := j.at.Add(time.Duration(j.sec * float64(time.Second)))
		for t := j.at; t.Before(end); {
			h := t.Truncate(time.Hour)
			next := h.Add(time.Hour)
			if next.After(end) {
				next = end
			}
			if k := int(h.Sub(rp.from.Truncate(time.Hour)) / time.Hour); k >= 0 && k < len(hours) {
				hours[k] += j.cores * next.Sub(t).Seconds() / 3600
			}
			t = next
		}
	}
	slices.Sort(hours)
	r.peak = Percentile(hours, 0.95)
	waits := fifo(jobs, rp.cores)
	n := 0
	for _, w := range waits {
		r.mean += w
		if w >= 60 {
			n++
		}
	}
	r.mean /= float64(len(waits))
	r.share = float64(n) / float64(len(waits))
	return r
}

// growMax is the most runs the replay tries, as a factor of the last week's.
const growMax = 8

// grow: the smallest factor (to growMax) at which done holds, or +Inf.
func (rp *replay) grow(done func(replayed) bool) float64 {
	if done(rp.at(1)) {
		return 1
	}
	lo, hi := 1.0, 1.0
	for {
		hi *= 1.25
		if hi > growMax {
			return math.Inf(1)
		}
		if done(rp.at(hi)) {
			break
		}
		lo = hi
	}
	for range 8 {
		mid := (lo + hi) / 2
		if done(rp.at(mid)) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// fifo: each job's wait for room on a pool of cores, first come first
// served (a job waits while the one before it waits).
func fifo(jobs []job, cores float64) []float64 {
	type busy struct {
		until time.Time
		cores float64
	}
	var running []busy
	free := cores
	var last time.Time
	out := make([]float64, len(jobs))
	for i, j := range jobs {
		at := j.at
		if at.Before(last) {
			at = last
		}
		for {
			kept := running[:0]
			for _, b := range running {
				if !b.until.After(at) {
					free += b.cores
				} else {
					kept = append(kept, b)
				}
			}
			running = kept
			if free >= j.cores-1e-9 || len(running) == 0 {
				break
			}
			next := running[0].until
			for _, b := range running {
				if b.until.Before(next) {
					next = b.until
				}
			}
			at = next
		}
		out[i] = at.Sub(j.at).Seconds()
		free -= j.cores
		running = append(running, busy{at.Add(time.Duration(j.sec * float64(time.Second))), j.cores})
		last = at
	}
	return out
}
