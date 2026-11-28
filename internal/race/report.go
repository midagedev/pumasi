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

// Run runs sc on the usual fixed runners, the tuned ones, and pumasi: as
// configured, without what it measured before, and without each of its
// later steps.
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
	// Each step pumasi took, from the strict reservation on: cpu shared
	// (cpuOvercommit), then runners kept warm (warm), as configured by default.
	strict, nowarm, cold := sc, sc, sc
	strict.CPUOvercommit, strict.Warm, nowarm.Warm, cold.Cold = 1, 0, 0, true
	r.Runs = append(r.Runs, RunPumasi(strict, jobs), RunPumasi(nowarm, jobs), RunPumasi(sc, jobs), RunPumasi(cold, jobs))
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

// Sweep runs ten seeds at three paces, and the busiest pace again with
// every job busy from start to end: the film's numbers are not one lucky
// draw. Each figure is the average over the seeds.
func Sweep() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pushes every\tsetup\tmedian wait\tp90 wait\tmax wait\tmost waiting\tall done\tslowdown\tp90 slowdown\t")
	type load struct {
		every [2]float64
		busy  bool
	}
	for _, l := range []load{{[2]float64{25, 40}, false}, {[2]float64{35, 55}, false}, {[2]float64{50, 70}, false}, {[2]float64{25, 40}, true}} {
		sum := map[string]*Summary{}
		var names []string
		for seed := uint64(1); seed <= 10; seed++ {
			sc := Film
			sc.Seed, sc.Every, sc.Busy = seed, l.every, l.busy
			for _, x := range Run(sc).Runs {
				t := sum[x.Name]
				if t == nil {
					t = &Summary{}
					sum[x.Name] = t
					names = append(names, x.Name)
				}
				s := x.Summary
				t.MedianWait += s.MedianWait / 10
				t.P90Wait += s.P90Wait / 10
				t.MaxWait += s.MaxWait / 10
				t.MeanWait += float64(s.PeakWaiting) / 10
				t.Done += s.Done / 10
				t.MeanSlowdown += s.MeanSlowdown / 10
				t.P90Slowdown += s.P90Slowdown / 10
			}
		}
		pace := fmt.Sprintf("%g-%gs", l.every[0], l.every[1])
		if l.busy {
			pace += " busy"
		}
		for _, n := range names {
			t := sum[n]
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.1f\t%s\t%.3f\t%.3f\t\n", pace, n, mmss(t.MedianWait), mmss(t.P90Wait), mmss(t.MaxWait), t.MeanWait, mmss(t.Done), t.MeanSlowdown, t.P90Slowdown)
		}
	}
	w.Flush()
	b.WriteString("(each figure the average over seeds 1-10; busy: every job uses its cpu from start to end)\n")
	return b.String()
}

// IdleTable is what pumasi's runners without a job cost, with runners kept
// warm and without, at the sweep's paces: what they held on average (and
// the share of all the hosts' room), the most memory at once, and per run
// how many were started warm, how many of those a job took, and how many
// runners went without ever having a job.
func IdleTable() string {
	var roomCPU, roomMem float64
	for _, h := range Hosts {
		c, m := h.Room()
		roomCPU, roomMem = roomCPU+c, roomMem+float64(m)/gb
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pushes every\tsetup\tidle cpu held\tidle mem held\tmost idle mem\twarm started\ttaken by a job\tgone without a job\t")
	for _, every := range [][2]float64{{25, 40}, {35, 55}, {50, 70}} {
		for _, warm := range []int{0, Film.Warm} {
			var st Idle
			name := ""
			for seed := uint64(1); seed <= 10; seed++ {
				sc := Film
				sc.Seed, sc.Every, sc.Warm = seed, every, warm
				x := RunPumasi(sc, Stream(sc))
				name = x.Name
				st.WarmStarted += x.Idle.WarmStarted
				st.WarmServed += x.Idle.WarmServed
				st.Stopped += x.Idle.Stopped
				st.HeldCPU += x.Idle.HeldCPU / 10
				st.HeldMem += x.Idle.HeldMem / 10
				st.PeakMem += x.Idle.PeakMem / 10
			}
			served := "-"
			if st.WarmStarted > 0 {
				served = fmt.Sprintf("%.0f%%", 100*float64(st.WarmServed)/float64(st.WarmStarted))
			}
			fmt.Fprintf(w, "%g-%gs\t%s\t%.1f (%.0f%%)\t%.1fG (%.0f%%)\t%.1fG\t%.1f\t%s\t%.1f\t\n", every[0], every[1], name, st.HeldCPU, 100*st.HeldCPU/roomCPU,
				st.HeldMem, 100*st.HeldMem/roomMem, st.PeakMem, float64(st.WarmStarted)/10, served, float64(st.Stopped)/10)
		}
	}
	w.Flush()
	fmt.Fprintf(&b, "(seeds 1-10, figures per run; idle: runners coming up or listening without a job; the hosts' room is %.0fcpu/%.0fG)\n", roomCPU, roomMem)
	return b.String()
}
