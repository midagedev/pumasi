package synth

import (
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
)

// ShiftScenario is a host that changes under every job, with a real
// regression of one job in the same history.
type ShiftScenario struct {
	Name   string
	Host   string // "" = every host
	Factor float64
	For    time.Duration // 0 = holds
}

// ShiftScenarios are the hosts' changes the search is scored on.
var ShiftScenarios = []ShiftScenario{
	{Name: "a host 1.5x slower (disk)", Host: "linux-slow", Factor: 1.5},
	{Name: "a host 0.7x (caches cleared)", Host: "linux-fast", Factor: 0.7},
	{Name: "a host 2x for 6h (neighbour)", Host: "linux-fast", Factor: 2, For: 6 * time.Hour},
	{Name: "every host 1.3x (mirror)", Host: "", Factor: 1.3},
}

// ShiftResult is how the search did on a host's change, before (Plain) and
// after machines were told apart.
type ShiftResult struct {
	Scenario  string
	For       time.Duration // the host's change lasted this long (0 = held)
	Pinned    bool
	Inside    bool // the regression was within 12 hours of the host's change, else 3 days or more away
	Histories int
	// FalseJobs: changes listed as jobs' within a day of the host's change
	// (one a job), other than the regression; Plain and with machines told apart.
	FalsePlain, False int
	// Found: the regression was listed as its job's.
	FoundPlain, Found int
	// Shifts: histories where the host's change was shown as a machine's.
	Shifts int
}

// EvalShift scores the search on histories where a host changes in the
// third week and one job gets 20% slower at a random moment of days 8 to 26,
// three days or more from the host's change (or, Inside, within 12 hours).
func EvalShift(ec EvalConfig) []ShiftResult {
	if ec.Days == 0 {
		ec.Days = 28
	}
	var out []ShiftResult
	for _, pinned := range []bool{false, true} {
		for _, inside := range []bool{false, true} {
			for si, sc := range ShiftScenarios {
				res := ShiftResult{Scenario: sc.Name, For: sc.For, Pinned: pinned, Inside: inside}
				for i := range ec.Histories {
					seed := uint64(7_000_000 + si*100_000 + i)
					rng := rand.New(rand.NewPCG(seed, 11))
					shiftAt := time.Duration(14*24+rng.IntN(7*24)) * time.Hour
					regAt := time.Duration(8*24+rng.IntN(18*24)) * time.Hour
					for (regAt - shiftAt).Abs() < 3*24*time.Hour {
						regAt = time.Duration(8*24+rng.IntN(18*24)) * time.Hour
					}
					if inside {
						regAt = shiftAt + time.Duration(rng.IntN(24)-12)*time.Hour
					}
					var jobs []Job
					for _, j := range Shop {
						if !j.Nightly {
							jobs = append(jobs, j)
						}
					}
					job := jobs[rng.IntN(len(jobs))]
					h := Generate(Config{Seed: seed, Start: EvalStart, Days: ec.Days, Merges: ec.Merges, Pinned: pinned,
						HostShifts: []HostShift{{Host: sc.Host, After: shiftAt, For: sc.For, Factor: sc.Factor}},
						Inject:     []Planned{{Change: Change{Job: job.Name, Metric: "duration", Factor: 1.2}, After: regAt}}})
					if len(h.Injected) == 0 {
						continue
					}
					res.Histories++
					in := h.Injected[0]
					at := EvalStart.Add(shiftAt)
					reg := Scenario{Metric: "duration", Factor: 1.2}
					window := 24*time.Hour + sc.For
					count := func(cs []watch.Change) (found bool, false_ int) {
						hit := hitOf(cs, in, reg)
						seen := map[string]bool{}
						for _, c := range cs {
							if hit != nil && c.Job == hit.Job && abs(c.First.At.Sub(hit.First.At).Hours()) < 24 {
								continue
							}
							if d := c.First.At.Sub(at); d > -24*time.Hour && d < window {
								seen[c.Job] = true
							}
						}
						return hit != nil, len(seen)
					}
					plain := watch.Options{Plain: true}
					f, n := count(watch.Find(h.Runs, plain))
					res.FalsePlain += n
					if f {
						res.FoundPlain++
					}
					jobsFound, shifts := watch.FindAll(h.Runs, ec.Options)
					f, n = count(jobsFound)
					res.False += n
					if f {
						res.Found++
					}
					for _, s := range shifts {
						if s.Kind == "host" && abs(s.At.Sub(at).Hours()) < 24 {
							res.Shifts++
							break
						}
					}
				}
				out = append(out, res)
			}
		}
	}
	return out
}

// WriteShiftReport prints EvalShift's results.
func WriteShiftReport(w io.Writer, ec EvalConfig, rs []ShiftResult) {
	fmt.Fprintf(w, "%d histories a row, %d days, a job +20%% at a random moment of days 8-26 three days or more from the host's change (apart), or within 12h of it (inside)\n", ec.Histories, cmpDays(ec.Days))
	fmt.Fprintf(w, "%-30s %-7s %-7s %18s %18s %12s\n", "host's change", "hosts", "reg.", "false jobs/hist", "regression found", "shown as host")
	fmt.Fprintf(w, "%-30s %-7s %-7s %18s %18s %12s\n", "", "", "", "before -> after", "before -> after", "")
	for _, r := range rs {
		hosts, where := "shared", "apart"
		if r.Pinned {
			hosts = "pinned"
		}
		if r.Inside {
			where = "inside"
		}
		n := float64(max(r.Histories, 1))
		fmt.Fprintf(w, "%-30s %-7s %-7s %8.2f -> %5.2f %8s -> %5s %12s\n", r.Scenario, hosts, where,
			float64(r.FalsePlain)/n, float64(r.False)/n, pct(frac(r.FoundPlain, r.Histories)), pct(frac(r.Found, r.Histories)), pct(frac(r.Shifts, r.Histories)))
	}
}
