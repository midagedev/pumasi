package watch

import (
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
	// Note says what the numbers are of, for a reader without the docs.
	Note string `json:"note"`
}

const trendNote = "time, memory and cpu are of runs that succeeded (p50/p90, nearest rank); memory leaves page cache out; " +
	"changes are steps that held across runs, found among runs whose host was not saturated by others, each set against what its host usually gives"

// BuildTrend makes the report from runs that reach Baseline before q.From.
func BuildTrend(runs []ledger.JobRun, q TrendQuery, o Options) TrendReport {
	rep := TrendReport{From: q.From, To: q.To, Grain: "day", Note: trendNote, Series: Trend(runs, q), Changes: []Change{}}
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
	for _, c := range Find(picked, o) {
		if !c.First.At.Before(q.From) {
			rep.Changes = append(rep.Changes, c)
		}
	}
	// The newest first: what a reader wants to know about is what just happened.
	slices.Reverse(rep.Changes)
	return rep
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
