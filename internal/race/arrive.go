package race

import (
	"math/rand/v2"
	"slices"
)

// arrivals is when each job of a stream comes. A job that needs another
// (Job.After) comes Delay after that one ends, so when it comes depends on
// where and how soon the other ran: on the setup.
type arrivals struct {
	jobs []Job
	at   []float64 // when each came (-1 = not yet)
	next int       // the next job by At not yet looked at
	wait []int     // jobs that need another, not come yet
	byID map[int]int
}

func newArrivals(jobs []Job) *arrivals {
	a := &arrivals{jobs: jobs, at: make([]float64, len(jobs)), byID: map[int]int{}}
	for i, j := range jobs {
		a.at[i] = -1
		a.byID[j.ID] = i
	}
	return a
}

// due is the jobs that came by t, in the order they came. ends: when each
// job ended (0 = not yet).
func (a *arrivals) due(t float64, ends []float64) []int {
	var out []int
	for a.next < len(a.jobs) && a.jobs[a.next].At <= t {
		if a.jobs[a.next].After == 0 {
			a.at[a.next] = a.jobs[a.next].At
			out = append(out, a.next)
		} else {
			a.wait = append(a.wait, a.next)
		}
		a.next++
	}
	left := a.wait[:0]
	for _, i := range a.wait {
		p := a.byID[a.jobs[i].After]
		if e := ends[p]; e > 0 && e+a.jobs[i].Delay <= t {
			a.at[i] = e + a.jobs[i].Delay
			out = append(out, i)
			continue
		}
		left = append(left, i)
	}
	a.wait = left
	slices.SortStableFunc(out, func(x, y int) int {
		if a.at[x] != a.at[y] {
			if a.at[x] < a.at[y] {
				return -1
			}
			return 1
		}
		return x - y
	})
	return out
}

// came is the jobs that came by now, with when.
func (a *arrivals) came() []int {
	var out []int
	for i, t := range a.at {
		if t >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// provision is how long the runner started seq-th takes to take a job. With
// Native, drawn as native runners came up on real hosts (decide to
// listening): median about 4.8s, one in five 5.7-12s, one in thirty-three
// 12-36s.
func (sc Scenario) provision(seq int) float64 {
	if !sc.Native {
		return sc.Provision
	}
	r := rand.New(rand.NewPCG(sc.Seed^0x5eed, uint64(seq)))
	switch x := r.Float64(); {
	case x < 0.8:
		return 3.4 + r.Float64()*2.3
	case x < 0.97:
		return 5.7 + r.Float64()*6.3
	default:
		return 12 + r.Float64()*24
	}
}
