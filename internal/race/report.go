package race

import (
	"fmt"
	"math"
	"strings"
	"text/tabwriter"
)

// HostRoom is a host and the room both setups have on it.
type HostRoom struct {
	Name string  `json:"name"`
	OS   string  `json:"os"`
	CPU  float64 `json:"cpu"`
	Mem  float64 `json:"mem"` // GiB
}

// Report is a scenario run on each setup.
type Report struct {
	Scenario Scenario   `json:"scenario"`
	Hosts    []HostRoom `json:"hosts"`
	Jobs     []Job      `json:"jobs"`
	Fixed    []Fixed    `json:"fixed"`
	Runs     []Result   `json:"runs"`
}

// Run runs sc on the usual fixed runners, the tuned ones, and pumasi with
// and without what it measured before.
func Run(sc Scenario) Report {
	jobs := Stream(sc)
	r := Report{Scenario: sc, Jobs: jobs, Fixed: []Fixed{Uniform(), PerClass(jobs), Split(jobs)}}
	for _, h := range Hosts {
		c, m := h.Room()
		r.Hosts = append(r.Hosts, HostRoom{Name: h.Name, OS: h.OS, CPU: c, Mem: round2(float64(m) / gb)})
	}
	for _, f := range r.Fixed {
		r.Runs = append(r.Runs, RunFixed(f, jobs))
	}
	cold := sc
	cold.Cold = true
	r.Runs = append(r.Runs, RunPumasi(sc, jobs), RunPumasi(cold, jobs))
	return r
}

// Get is the run of a setup by name.
func (r Report) Get(name string) Result {
	for _, x := range r.Runs {
		if x.Name == name {
			return x
		}
	}
	return Result{}
}

func mmss(s float64) string {
	s = math.Round(s)
	return fmt.Sprintf("%d:%02d", int(s)/60, int(s)%60)
}

// Table is the report in numbers, a row per setup.
func (r Report) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d jobs from %d pull-request pushes, every %g-%gs; seed %d\n", len(r.Jobs), r.Scenario.Pushes, r.Scenario.Every[0], r.Scenario.Every[1], r.Scenario.Seed)
	for _, f := range r.Fixed {
		per := map[string][]string{}
		for _, s := range f.Slots {
			k := fmt.Sprintf("%gG", s.Mem)
			if s.Small {
				k += " small"
			} else if len(s.Classes) == 1 {
				k += " " + s.Classes[0]
			}
			per[s.Host] = append(per[s.Host], k)
		}
		fmt.Fprintf(&b, "%s: %d runners:", f.Name, len(f.Slots))
		for _, h := range Hosts {
			fmt.Fprintf(&b, " %s %s;", h.Name, count(per[h.Name]))
		}
		b.WriteString("\n")
	}
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "setup\tmedian wait\tp90 wait\tmax wait\tmost waiting\tlast start\tall done\tslowdown\tp90 slowdown\tpeak psi\tmem used\tmem held\tcpu used\t")
	for _, x := range r.Runs {
		s := x.Summary
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%.2f\t%.2f\t%.0f\t%.0f%%\t%.0f%%\t%.0f%%\t\n", x.Name, mmss(s.MedianWait), mmss(s.P90Wait), mmss(s.MaxWait),
			s.PeakWaiting, mmss(s.Drained), mmss(s.Done), s.MeanSlowdown, s.P90Slowdown, s.PeakPSI, s.MemUse*100, s.MemHeld*100, s.CPUUse*100)
	}
	w.Flush()
	return b.String()
}

func count(xs []string) string {
	var out []string
	for i := 0; i < len(xs); {
		j := i
		for j < len(xs) && xs[j] == xs[i] {
			j++
		}
		out = append(out, fmt.Sprintf("%dx%s", j-i, xs[i]))
		i = j
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, " + ")
}

// Sweep runs other seeds and paces: the film's numbers are not one lucky draw.
func Sweep() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	names := []string{"fixed", "fixed-per-class", "fixed-split", "pumasi", "pumasi-cold"}
	fmt.Fprintf(w, "pushes every\tseeds\tjobs\t%s\t%s\t\n", strings.Join(names, "\t"), strings.Join(names, "\t"))
	for _, every := range [][2]float64{{25, 40}, {35, 55}, {50, 70}} {
		var n, jobs int
		med := map[string]float64{}
		p90 := map[string]float64{}
		for seed := uint64(1); seed <= 10; seed++ {
			sc := Film
			sc.Seed, sc.Every = seed, every
			r := Run(sc)
			n++
			jobs += len(r.Jobs)
			for _, x := range r.Runs {
				med[x.Name] += x.Summary.MedianWait
				p90[x.Name] += x.Summary.P90Wait
			}
		}
		k := float64(n)
		fmt.Fprintf(w, "%g-%gs\t%d\t%d\t", every[0], every[1], n, jobs/n)
		for _, m := range []map[string]float64{med, p90} {
			for _, name := range names {
				fmt.Fprintf(w, "%s\t", mmss(m[name]/k))
			}
		}
		fmt.Fprintln(w)
	}
	w.Flush()
	b.WriteString("(median wait, then p90 wait: each the average over the seeds)\n")
	return b.String()
}
