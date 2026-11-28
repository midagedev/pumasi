package watch

import (
	"math"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// Baseline is how much history before a span the search for changes reads,
// so that a change early in the span has runs before it.
const Baseline = 14 * 24 * time.Hour

// TrendReport is `pumasi trend` and /api/trend: every job's numbers over a
// span, and the changes whose first run is in it.
type TrendReport struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Grain   string    `json:"grain"` // day | week
	Series  []Series  `json:"series"`
	Changes []Change  `json:"changes"`
	// Creeping: jobs whose time grew slowly over the span, with no step at
	// a commit to show for it.
	Creeping []Creep `json:"creeping"`
	// Note says what the numbers are of, for a reader without the docs.
	Note string `json:"note"`
}

const trendNote = "time, memory and cpu are of runs that succeeded (p50/p90, nearest rank); memory leaves page cache out; " +
	"changes are steps that held across runs, found among runs whose host was not saturated by others, each set against what its host usually gives; " +
	"creeping is a job's time that grew 12% or more along a trend over the span or the last 28 days, whichever is longer (Theil–Sen on its daily medians, the 95% interval above zero), with no step; " +
	"a series' change is the time steps listed (and creep) together, or for one with none the median of its last third of days against its first"

// BuildTrend makes the report from runs that reach Baseline before q.From.
func BuildTrend(runs []ledger.JobRun, q TrendQuery, o Options) TrendReport {
	rep := TrendReport{From: q.From, To: q.To, Grain: "day", Note: trendNote, Series: Trend(runs, q), Changes: []Change{}, Creeping: []Creep{}}
	if q.Week {
		rep.Grain = "week"
	}
	if rep.Series == nil {
		rep.Series = []Series{}
	}
	var picked []ledger.JobRun
	for _, r := range runs {
		if q.Filter.Matches(r) && r.FinishedAt.Before(q.To) {
			picked = append(picked, r)
		}
	}
	all := Find(picked, o)
	for _, c := range all {
		if !c.First.At.Before(q.From) {
			rep.Changes = append(rep.Changes, c)
		}
	}
	// The newest first: what a reader wants to know about is what just happened.
	slices.Reverse(rep.Changes)
	rep.Creeping = creeping(picked, q, all)
	// Each series' change: what the lists above say of it, so one screen
	// does not say two things.
	for i := range rep.Series {
		s := &rep.Series[i]
		s.Change, s.ChangeFrom = Growth(*s), "thirds"
		ratio, listed := 1.0, false
		for _, c := range rep.Changes {
			if c.Metric == "time" && c.Repo == s.Repo && c.Workflow == s.Workflow && c.Job == s.Job && s.Job != "" && c.Holds {
				ratio *= 1 + c.Ratio
				listed = true
			}
		}
		for _, c := range rep.Creeping {
			if c.Repo == s.Repo && c.Workflow == s.Workflow && c.Job == s.Job {
				ratio *= 1 + c.Ratio
				listed = true
			}
		}
		if listed {
			s.Change, s.ChangeFrom = ratio-1, "listed"
		}
	}
	return rep
}

// Creep is a job whose time grew along a trend over the span, with no step.
type Creep struct {
	Repo     string  `json:"repo"`
	Workflow string  `json:"workflow"`
	Job      string  `json:"job"`
	Ratio    float64 `json:"ratio"` // over the span, along the trend
	Low      float64 `json:"low"`   // the 95% interval's low end of Ratio
	From     float64 `json:"from"`  // the trend's seconds at the span's start and end
	To       float64 `json:"to"`
	Days     int     `json:"days"` // days with runs it is of
	// Span: how far back it was looked for (the trend's span, or CreepSpan
	// when that is longer).
	Span time.Duration `json:"span"`
}

// CreepSpan is the least span creep is looked for over, and CreepMin the
// least growth over it that is reported.
const (
	CreepSpan = 28 * 24 * time.Hour
	CreepMin  = 0.12
)

// creeping: jobs with no time step in the span whose daily median time (runs
// that succeeded on a host not saturated by others, each on its host's
// scale) grew CreepMin or more over the span (at least CreepSpan), its 95%
// interval above zero. The runs should reach that far back.
func creeping(runs []ledger.JobRun, q TrendQuery, found []Change) []Creep {
	type key struct{ repo, wf, job string }
	// Creep is slow: it is looked for over at least CreepSpan.
	from := q.From
	if q.To.Sub(from) < CreepSpan {
		from = q.To.Add(-CreepSpan)
	}
	// A job whose time stepped in that span (listed or not) is not creeping.
	has := map[key]bool{}
	for _, c := range found {
		if c.Metric == "time" && !c.First.At.Before(from) {
			has[key{c.Repo, c.Workflow, c.Job}] = true
		}
	}
	by := map[key][]ledger.JobRun{}
	var order []key
	for _, r := range runs {
		k := key{r.Repo, r.Workflow, r.Job}
		if has[k] || r.Result != "succeeded" || r.Contended() || r.RunSec <= 0 {
			continue
		}
		if by[k] == nil {
			order = append(order, k)
		}
		by[k] = append(by[k], r)
	}
	out := []Creep{}
	for _, k := range order {
		all := by[k]
		hf := hostFactors(all, "time")
		day := map[int][]float64{}
		for _, r := range all {
			if r.StartedAt.Before(from) || !r.StartedAt.Before(q.To) {
				continue
			}
			v := r.RunSec
			if f := hf[r.Host]; f > 0 {
				v /= f
			}
			d := int(r.StartedAt.Sub(from).Hours() / 24)
			day[d] = append(day[d], math.Log(v))
		}
		var xs, ys []float64
		for d, v := range day {
			if len(v) >= 2 {
				xs = append(xs, float64(d))
				ys = append(ys, median(v))
			}
		}
		if len(xs) < 7 {
			continue
		}
		slope, lo, _ := theilSenAt(xs, ys, 1.96)
		span := q.To.Sub(from).Hours() / 24
		c := Creep{Repo: k.repo, Workflow: k.wf, Job: k.job, Ratio: math.Exp(slope*span) - 1, Low: math.Exp(lo*span) - 1, Days: len(xs)}
		c.Span = q.To.Sub(from)
		if c.Ratio < CreepMin || c.Low <= 0 {
			continue
		}
		// The trend line's ends, through the median of the points.
		mid := median(slices.Clone(ys)) - slope*median(slices.Clone(xs))
		c.From, c.To = math.Exp(mid), math.Exp(mid+slope*span)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Creep) int { return cmpFloat(b.Ratio, a.Ratio) })
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// theilSenAt: the Theil–Sen slope of y against x, and the interval around
// it Kendall's tau gives at z (1.96 = 95%).
func theilSenAt(x, y []float64, z float64) (m, lo, hi float64) {
	n := len(x)
	var sl []float64
	for i := range n {
		for j := range n {
			if x[j] > x[i] {
				sl = append(sl, (y[j]-y[i])/(x[j]-x[i]))
			}
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
	return m, sl[max(0, min(len(sl)-1, l))], sl[max(0, min(len(sl)-1, u))]
}

// Growth is how a series' typical time moved over its span: the median of
// the daily p50s in its last third against its first third (0 = too few days
// with runs to say).
func Growth(s Series) float64 {
	var v []float64
	for _, b := range s.Buckets {
		if b.Succeeded > 0 {
			v = append(v, b.Sec.P50)
		}
	}
	if len(v) < 3 {
		return 0
	}
	third := len(v) / 3
	first, last := median(slices.Clone(v[:third])), median(slices.Clone(v[len(v)-third:]))
	if first <= 0 {
		return 0
	}
	return last/first - 1
}
