package decide

import (
	"cmp"
	"slices"
	"sort"
	"time"
)

// Busy is what a forecast knows of a runner with a job: when the job is
// expected to end (zero = nobody can say) and its name, to say whose end
// makes the room.
type Busy struct {
	Ends time.Time
	Job  string
}

// Expect is when a waiting job is expected to get its runner.
type Expect struct {
	// In: from the snapshot's time until a runner takes the job.
	In time.Duration
	// Host: where.
	Host string
	// After: the job whose end makes the room ("" = the room is there, or a
	// runner of its class is coming up for it).
	After string
	// AfterRunner: the runner of After when that job is running now ("" =
	// After is a job still waiting, whose end is forecast too). Several jobs
	// can share a name; this says which one.
	AfterRunner string
	// AfterWith: when After's end alone would not make the room (what is
	// free now and what After holds are not enough), how many other jobs end
	// on Host before it and leave theirs. 0 = After's end is enough.
	AfterWith int
}

// Horizon: a forecast looks no further ahead. A job not placed by then is
// one nobody can give a time for.
const Horizon = 2 * time.Hour

// EndOf is when a job that started at started and takes runtime is expected
// to end, seen at now: started+runtime, and for a job already past that,
// a tenth of its runtime more (at least 15s) from now: it is late, and how
// late nobody knows.
func EndOf(started, now time.Time, runtime time.Duration) time.Time {
	end := started.Add(runtime)
	if !end.After(now) {
		return now.Add(max(runtime/10, 15*time.Second))
	}
	return end
}

// Forecast plays the snapshot forward with the placement itself: the
// running jobs end when busy says, each waiting job takes as long as runs
// says (by Demand.ID), and Decide is asked again at each end. A job's
// expected start is when Decide starts a runner for it plus provision (the
// time a runner takes to come up), or when a runner of its class that is
// coming up now will be up. Jobs missing from the result get no time: they
// wait for something no end opens (a host that is closed or down, a class
// with no host), for a job whose end nobody can say, or past Horizon.
//
// It is a forecast, not a promise: it knows of no job that comes after the
// snapshot, of no host that closes or opens, and it takes cpu pressure to
// ease once a job has ended.
func Forecast(s Snapshot, busy map[string]Busy, runs map[string]time.Duration, provision time.Duration) map[string]Expect {
	out := map[string]Expect{}
	cur := s
	cur.Runners = slices.Clone(s.Runners)
	cur.Demand = slices.Clone(s.Demand)
	cur.Hosts = slices.Clone(s.Hosts)
	// A runner kept warm takes no job of its own: the jobs that are waiting
	// are what it plays forward.
	cur.Classes = slices.Clone(s.Classes)
	for i := range cur.Classes {
		cur.Classes[i].Warm, cur.Classes[i].Soon = 0, 0
	}
	ends := map[string]Busy{}
	for name, b := range busy {
		ends[name] = b
	}
	named := map[string]string{} // demand id -> its name
	for _, d := range s.Demand {
		named[d.ID] = cmp.Or(d.Name, "a job of "+d.Class)
	}
	take := func(id, runner string, at time.Time) {
		ends[runner] = Busy{Job: named[id]}
		if r, ok := runs[id]; ok {
			ends[runner] = Busy{Ends: at.Add(r), Job: named[id]}
		}
		cur.Demand = slices.DeleteFunc(cur.Demand, func(d Demand) bool { return d.ID == id })
	}

	// Runners that are up or coming up without a job take their class's
	// oldest jobs, the first up the oldest, as GitHub hands them out.
	byClass := map[string][]Demand{}
	for _, d := range cur.Demand {
		if !d.Pending {
			byClass[d.Class] = append(byClass[d.Class], d)
		}
	}
	for c := range byClass {
		sort.Slice(byClass[c], func(i, j int) bool { return inClass(byClass[c][i], byClass[c][j]) })
	}
	up := map[string]bool{}
	for _, h := range s.Hosts {
		up[h.Name] = h.Up
	}
	free := slices.Clone(cur.Runners)
	sort.SliceStable(free, func(i, j int) bool { return free[i].CreatedAt.Before(free[j].CreatedAt) })
	for _, r := range free {
		if r.JobKnown || r.Container == "exited" || r.State == "done" || len(byClass[r.Class]) == 0 {
			continue
		}
		d := byClass[r.Class][0]
		byClass[r.Class] = byClass[r.Class][1:]
		if !up[r.Host] {
			continue // its host is unreachable: when it comes up nobody can say
		}
		at := s.Now
		if r.ListeningAt.IsZero() {
			at = maxTime(s.Now, r.CreatedAt.Add(provision))
		}
		out[d.ID] = Expect{In: at.Sub(s.Now), Host: r.Host}
		take(d.ID, r.Name, at)
		for i := range cur.Runners {
			if cur.Runners[i].Name == r.Name {
				cur.Runners[i].JobKnown, cur.Runners[i].Working, cur.Runners[i].State = true, true, "busy"
			}
		}
	}

	// now: the snapshot once the runners up now have their jobs, to ask
	// whether After's end alone makes a job's room
	now := cur
	now.Runners, now.Demand = slices.Clone(cur.Runners), slices.Clone(cur.Demand)
	alone := func(runner, job, host string) bool {
		s := now
		s.Runners = slices.DeleteFunc(slices.Clone(now.Runners), func(r Runner) bool { return r.Name == runner })
		for _, st := range Decide(s).Start {
			if st.Job == job && st.Host == host {
				return true
			}
		}
		return false
	}
	endedOn := map[string]int{} // host -> jobs ended there so far
	afterOn := map[string]int{} // host -> 1 where After ended
	after, afterRunner := "", ""
	for step := 0; step < 1000 && len(cur.Demand) > 0; step++ {
		p := Decide(cur)
		for _, st := range p.Stop {
			cur.Runners = slices.DeleteFunc(cur.Runners, func(r Runner) bool { return r.Name == st.Runner })
		}
		for _, st := range p.Start {
			name := "forecast/" + st.Job
			up := cur.Now.Add(provision)
			e := Expect{In: up.Sub(s.Now), Host: st.Host, After: after, AfterRunner: afterRunner}
			if afterRunner != "" && !alone(afterRunner, st.Job, st.Host) {
				e.AfterWith = endedOn[st.Host] - afterOn[st.Host]
			}
			out[st.Job] = e
			take(st.Job, name, up)
			cur.Runners = append(cur.Runners, Runner{Name: name, Class: st.Class, Host: st.Host, CPUs: st.CPUs, MemBytes: st.MemBytes,
				State: "busy", JobKnown: true, Working: true, Container: "running", CreatedAt: cur.Now, ListeningAt: up, JobStartedAt: up})
		}
		// The next end.
		var next time.Time
		for _, r := range cur.Runners {
			if e := ends[r.Name].Ends; !e.IsZero() && (next.IsZero() || e.Before(next)) {
				next = e
			}
		}
		if next.IsZero() || next.Sub(s.Now) > Horizon {
			break
		}
		next = maxTime(next, cur.Now)
		after, afterRunner = "", ""
		afterOn = map[string]int{}
		eased := map[string]bool{}
		cur.Runners = slices.DeleteFunc(cur.Runners, func(r Runner) bool {
			e := ends[r.Name]
			if e.Ends.IsZero() || e.Ends.After(next) {
				return false
			}
			if after == "" {
				after = e.Job
				afterOn[r.Host] = 1
				if _, now := busy[r.Name]; now {
					afterRunner = r.Name
				}
			}
			endedOn[r.Host]++
			eased[r.Host] = true
			return true
		})
		cur.Now = next
		// A job that ended took its load with it: the pressure it made eases.
		for i := range cur.Hosts {
			if h := &cur.Hosts[i]; eased[h.Name] {
				h.PSICPU, h.PSIMem, h.PSIIO, h.Pressed = 0, 0, 0, nil
			}
		}
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
