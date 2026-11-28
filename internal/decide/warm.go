package decide

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"
)

// WarmWindow: how far back a class's arrivals say how many runners it
// keeps warm. A class quiet this long keeps none, and the idle timeout ends
// the ones it kept.
const WarmWindow = 30 * time.Minute

// Burst: jobs of a class that arrive this close together came together:
// the workflows of one push, the legs of one matrix.
const Burst = 15 * time.Second

// WarmCount is how many runners a class keeps up without a job, from when
// its recent jobs arrived: as many as the most of them that came together
// (within Burst of the first), and none for a class that came only once —
// one burst says nothing of the next. At most most.
func WarmCount(arrivals []time.Time, most int) int {
	ts := slices.Clone(arrivals)
	slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
	bursts, largest := 0, 0
	for i := 0; i < len(ts); {
		j := i
		for j < len(ts) && ts[j].Sub(ts[i]) <= Burst {
			j++
		}
		bursts, largest = bursts+1, max(largest, j-i)
		i = j
	}
	if bursts < 2 {
		return 0
	}
	return min(largest, most)
}

// up is how many runners of c are kept up without a job: Warm, or Soon
// when more jobs are expected now. Not for an overflow class.
func (c Class) up() int {
	if c.Overflow {
		return 0
	}
	return max(c.Warm, c.Soon)
}

// soonFor is the i-th expected job of the class in words: each runner
// started ahead names its own job, not all the first one.
func soonFor(c Class, i int) string {
	if len(c.SoonFor) == 0 {
		return "a job of the class is expected"
	}
	return c.SoonFor[min(i, len(c.SoonFor)-1)]
}

// charged is what a runner holds of its host.
func charged(r Runner, classes map[string]Class) [2]float64 {
	cpus, mem := r.CPUs, float64(r.MemBytes)
	if c, ok := classes[r.Class]; ok && cpus == 0 && mem == 0 {
		cpus, mem = c.CPUs, float64(c.MemBytes)
	}
	return [2]float64{cpus, mem}
}

// idleRunner: up and listening, no job, nothing ending it. A runner whose
// job GitHub has not reported yet but whose worker runs has one.
func idleRunner(r Runner) bool {
	return !r.JobKnown && !r.Working && r.State != "busy" && !r.ListeningAt.IsZero() && r.Container == "running"
}

// warmKept is the runners that have no idle timeout: of each warm class,
// the Warm that listened last, on hosts that take the class.
func warmKept(s Snapshot, runners []Runner, classes map[string]Class, hosts map[string]Host) map[string]bool {
	idle := map[string][]Runner{}
	for _, r := range runners {
		c := classes[r.Class]
		if c.up() <= 0 || !idleRunner(r) {
			continue
		}
		if ok, _ := takes(s, c, hosts[r.Host]); ok {
			idle[r.Class] = append(idle[r.Class], r)
		}
	}
	out := map[string]bool{}
	for cl, rs := range idle {
		sort.Slice(rs, func(i, j int) bool {
			if !rs[i].ListeningAt.Equal(rs[j].ListeningAt) {
				return rs[i].ListeningAt.After(rs[j].ListeningAt)
			}
			return rs[i].Name < rs[j].Name
		})
		for _, r := range rs[:min(len(rs), classes[cl].up())] {
			out[r.Name] = true
		}
	}
	return out
}

// spareSet is the runners no job is theirs yet: n by class (runners up or
// coming up without a job, less the jobs GitHub holds for the class), and
// the listening ones by host, the largest first, that may give up their room.
type spareSet struct {
	n    map[string]int
	idle map[string][]Runner
	gone map[string]bool
}

func spares(s Snapshot, runners []Runner, stopped []Stop, classes map[string]Class, covering map[string]int) spareSet {
	sp := spareSet{n: map[string]int{}, idle: map[string][]Runner{}, gone: map[string]bool{}}
	held := map[string]int{}
	for _, d := range s.Demand {
		if !d.Pending {
			held[d.Class]++
		}
	}
	for c, n := range covering {
		sp.n[c] = max(0, n-held[c])
	}
	out := map[string]bool{}
	for _, st := range stopped {
		out[st.Runner] = true
	}
	for _, r := range runners {
		if !out[r.Name] && idleRunner(r) && sp.n[r.Class] > 0 {
			sp.idle[r.Host] = append(sp.idle[r.Host], r)
		}
	}
	for _, rs := range sp.idle {
		sort.SliceStable(rs, func(i, j int) bool {
			a, b := charged(rs[i], classes), charged(rs[j], classes)
			if a[1] != b[1] {
				return a[1] > b[1]
			}
			return rs[i].Name < rs[j].Name
		})
	}
	return sp
}

// all is the listening spare runners, by host.
func (sp spareSet) all() []Runner {
	var out []Runner
	for _, h := range slices.Sorted(maps.Keys(sp.idle)) {
		for _, r := range sp.idle[h] {
			if !sp.gone[r.Name] {
				out = append(out, r)
			}
		}
	}
	return out
}

// held is how many of a class's spare runners give up their room to no
// job: its WarmMin, which the class keeps whatever waits.
func (sp spareSet) held(c Class) int { return c.WarmMin }

func (sp spareSet) take(r Runner) {
	sp.n[r.Class]--
	sp.gone[r.Name] = true
}

// yieldFor is the spare runners on h, of classes other than class, whose
// room given up lets a runner of cpu/mem fit there; nil when theirs is not
// enough. Runners of h hold a.
func (sp spareSet) yieldFor(s Snapshot, h Host, a [2]float64, class string, cpu, mem float64, classes map[string]Class) []Runner {
	var out []Runner
	used := map[string]int{}
	for _, r := range sp.idle[h.Name] {
		if sp.gone[r.Name] || r.Class == class || used[r.Class] >= sp.n[r.Class]-sp.held(classes[r.Class]) {
			continue
		}
		used[r.Class]++
		out = append(out, r)
		c := charged(r, classes)
		a = [2]float64{a[0] - c[0], a[1] - c[1]}
		if fc, fm := s.free(h, a); fc+1e-9 >= cpu && fm >= mem {
			return out
		}
	}
	return nil
}

// giving is the listening spare runners that make way for a job that
// comes, of each class all but those it keeps whatever waits (held).
func (sp spareSet) giving(classes map[string]Class) []Runner {
	var out []Runner
	used := map[string]int{}
	for _, r := range sp.all() {
		if used[r.Class] >= sp.n[r.Class]-sp.held(classes[r.Class]) {
			continue
		}
		used[r.Class]++
		out = append(out, r)
	}
	return out
}

// without is alloc with the room of the listening spare runners given back:
// what a job that comes would find once they made way for it.
func (sp spareSet) without(alloc map[string][2]float64, classes map[string]Class) map[string][2]float64 {
	out := maps.Clone(alloc)
	for _, r := range sp.giving(classes) {
		c, a := charged(r, classes), out[r.Host]
		out[r.Host] = [2]float64{a[0] - c[0], a[1] - c[1]}
	}
	return out
}

// warmStarts starts runners for the warm classes short of their Warm, one
// class at a time in turn, each sized for the class's next job and placed
// as a job would be, on no host kept for a waiting job and for no class
// behind one.
func warmStarts(s Snapshot, hostNames []string, hosts map[string]Host, alloc map[string][2]float64, live, starting map[string]int, sp spareSet, kept, behind map[string]bool) []Start {
	want := map[string]int{}
	var order []Class
	for _, c := range s.Classes {
		if n := c.up(); n > 0 && !behind[c.Name] && n > sp.n[c.Name] {
			want[c.Name] = n - sp.n[c.Name]
			order = append(order, c)
		}
	}
	var out []Start
	for more := true; more; {
		more = false
		for _, c := range order {
			if want[c.Name] <= 0 || c.MaxRunners > 0 && live[c.Name] >= c.MaxRunners {
				continue
			}
			cpu, mem := c.CPUs, float64(c.MemBytes)
			switch {
			case c.Warm > 0 && c.NextCPUs > 0 && c.NextMem > 0:
				cpu, mem = c.NextCPUs, float64(c.NextMem)
			case c.Warm <= 0 && c.SoonCPUs > 0 && c.SoonMem > 0:
				cpu, mem = c.SoonCPUs, float64(c.SoonMem)
			}
			ceil := ceiling(s, c)
			best, bestSlots := "", -1.0
			var bestCPU, bestMem float64
			for _, hn := range hostNames {
				h := hosts[hn]
				if ok, _ := takes(s, c, h); !ok || kept[hn] || s.MaxStarting > 0 && starting[hn] >= s.MaxStarting {
					continue
				}
				hc, hm, capped, lends := sizeOn(h, cpu, mem, ceil)
				if !lends || capped {
					continue // a job that runs alone is not kept waiting for
				}
				fc, fm := s.free(h, alloc[hn])
				if fc+1e-9 < hc || fm < hm || reservedFor(s, c, hn, hc, hm, hosts, alloc, live) != "" {
					continue
				}
				if slots := min(fc/hc, fm/hm); best == "" || better(h, slots, hosts[best], bestSlots) {
					best, bestSlots, bestCPU, bestMem = hn, slots, hc, hm
				}
			}
			if best == "" {
				want[c.Name] = 0
				continue
			}
			fc, fm := s.free(hosts[best], alloc[best])
			have := c.up() - want[c.Name]
			of := fmt.Sprint(c.Warm)
			if c.WarmMin > 0 {
				of += fmt.Sprintf(", warmMin %d", c.WarmMin)
			}
			reason := fmt.Sprintf("warm: kept up for the class's next job (%d of %s), holds %.1fcpu/%s of the free %.1fcpu/%s, the largest of its recent jobs",
				have+1, of, bestCPU, gib(bestMem), fc, gib(fm))
			if have >= c.Warm {
				reason = fmt.Sprintf("ahead: %s (%d of %d expected), holds %.1fcpu/%s of the free %.1fcpu/%s",
					soonFor(c, have), have+1, c.Soon, bestCPU, gib(bestMem), fc, gib(fm))
			}
			out = append(out, Start{Class: c.Name, Host: best, CPUs: bestCPU, MemBytes: int64(bestMem), Reason: reason})
			a := alloc[best]
			alloc[best] = [2]float64{a[0] + bestCPU, a[1] + bestMem}
			starting[best]++
			live[c.Name]++
			want[c.Name]--
			more = true
		}
	}
	return out
}
