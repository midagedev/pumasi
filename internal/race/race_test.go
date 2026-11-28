package race

import (
	"encoding/json"
	"testing"
)

// The same scenario gives the same numbers: the film's can be made again.
func TestRunIsDeterministic(t *testing.T) {
	a, _ := json.Marshal(Run(Film))
	b, _ := json.Marshal(Run(Film))
	if string(a) != string(b) {
		t.Fatal("two runs of one scenario differ")
	}
}

// Each setup runs every job once, never before it arrived, and ends with none waiting.
func TestEverySetupRunsEveryJob(t *testing.T) {
	r := Run(Film)
	if len(r.Runs) != 7 {
		t.Fatalf("%d runs, want 7", len(r.Runs))
	}
	for _, x := range r.Runs {
		last := x.Frames[len(x.Frames)-1]
		if last.Done != len(r.Jobs) || len(last.Waiting) != 0 {
			t.Fatalf("%s: last frame done %d of %d, %d waiting", x.Name, last.Done, len(r.Jobs), len(last.Waiting))
		}
		for i, w := range x.Waits {
			if w < 0 {
				t.Fatalf("%s: job %d started before it came", x.Name, r.Jobs[i].ID)
			}
		}
		// A job runs on one host, in one stretch of frames.
		where := map[int]string{}
		ended := map[int]bool{}
		for _, f := range x.Frames {
			seen := map[int]bool{}
			for _, h := range f.Hosts {
				for _, j := range h.Jobs {
					if j.ID == 0 {
						continue
					}
					if ended[j.ID] || (where[j.ID] != "" && where[j.ID] != h.Name) || seen[j.ID] {
						t.Fatalf("%s: job %d runs twice or moves (t=%g)", x.Name, j.ID, f.T)
					}
					where[j.ID], seen[j.ID] = h.Name, true
				}
			}
			for id := range where {
				if !seen[id] {
					ended[id] = true
				}
			}
		}
		if len(where) != len(r.Jobs) {
			t.Fatalf("%s: %d jobs ran, want %d", x.Name, len(where), len(r.Jobs))
		}
	}
}

// Neither setup runs a host out of memory: the fixed runners are sized so
// that they cannot, and pumasi places within what is free.
func TestNoHostRunsOutOfMemory(t *testing.T) {
	r := Run(Film)
	room := map[string]float64{}
	for _, h := range r.Hosts {
		room[h.Name] = h.Mem
	}
	for _, f := range r.Fixed {
		held := map[string]float64{}
		for _, s := range f.Slots {
			held[s.Host] += s.Mem
		}
		for h, m := range held {
			if m > room[h]+1e-9 {
				t.Fatalf("%s: runners of %s are sized for %.1fG, room %.1fG", f.Name, h, m, room[h])
			}
		}
	}
	for _, x := range r.Runs {
		for _, f := range x.Frames {
			for _, h := range f.Hosts {
				var used float64
				for _, j := range h.Jobs {
					used += j.Mem
				}
				if used > room[h.Name]+1e-9 {
					t.Fatalf("%s: %s uses %.1fG at %gs, room %.1fG", x.Name, h.Name, used, f.T, room[h.Name])
				}
			}
		}
	}
}

// Jobs that want twice the cores of their host get half of what they want,
// run twice as long, and the host's cpu pressure rises past the controller's line.
func TestSharedCoresSlowJobsDown(t *testing.T) {
	h := Hosts[3] // linux-spare: 16 cores
	const n = 8
	var jobs []Job
	var runs []*run
	for i := range n {
		jobs = append(jobs, Job{ID: i + 1, Kind: "lint", Dur: 100, CPU: (2*h.CPUs - h.OtherCPU) / n, Mem: 1})
	}
	for i := range jobs {
		runs = append(runs, start(i, h.Name, 0, jobs))
		runs[i].work = 500 // up to pace, far from its end
		jobs[i].Dur = 1000
	}
	m := newMachines()
	var pace map[string]float64
	for i := 1; i <= 30; i++ {
		pace = m.pace(float64(i)*Tick, runs, jobs)
	}
	if p := pace[h.Name]; p < 0.49 || p > 0.51 {
		t.Fatalf("pace %.2f, want 0.5", p)
	}
	if w := runs[0].work - 500; w < 29.5 || w > 30.5 {
		t.Fatalf("a minute of ticks did %.1fs, want 30s", w)
	}
	if m.psi[h.Name] <= PSICPUThreshold {
		t.Fatalf("pressure %.0f, want above %d", m.psi[h.Name], PSICPUThreshold)
	}
	if pace["linux-server"] != 1 || m.psi["linux-server"] != 0 {
		t.Fatal("an idle host slowed down")
	}
}

// pumasi gives a time to every job that waits, and the first time it gives
// is within a minute at the median, at the busiest pace with long-tailed runs.
func TestForecastOfTheWait(t *testing.T) {
	sc := Film
	sc.Every, sc.Tails = [2]float64{12, 20}, true
	st := ETA(sc, 3)
	if st.Waited < 50 || st.Told != st.Waited {
		t.Fatalf("%d of %d waiting jobs given a time", st.Told, st.Waited)
	}
	if st.FirstMedian > 60 || st.InBand < 0.5 {
		t.Fatalf("median error %.0fs, %.0f%% within 60s/30%%", st.FirstMedian, 100*st.InBand)
	}
}

// Runners kept warm take most jobs the moment they come at a pace the hosts
// keep up with: the median wait drops below the time a runner takes to come
// up, most warm runners get a job, and few go without one.
func TestWarmRunnersTakeJobsAsTheyCome(t *testing.T) {
	var with, without []float64
	var st Idle
	for seed := uint64(1); seed <= 3; seed++ {
		sc := Film
		sc.Seed = seed
		jobs := Stream(sc)
		w := RunPumasi(sc, jobs)
		sc.Warm = 0
		n := RunPumasi(sc, jobs)
		with, without = append(with, w.Summary.MedianWait), append(without, n.Summary.MedianWait)
		st.WarmStarted, st.WarmServed, st.Stopped = st.WarmStarted+w.Idle.WarmStarted, st.WarmServed+w.Idle.WarmServed, st.Stopped+w.Idle.Stopped
	}
	for i := range with {
		if with[i] >= Film.Provision || without[i] < Film.Provision {
			t.Fatalf("median waits with warm %v, without %v", with, without)
		}
	}
	if st.WarmStarted == 0 || float64(st.WarmServed) < 0.7*float64(st.WarmStarted) || st.Stopped > st.WarmStarted/10 {
		t.Fatalf("warm runners: %+v", st)
	}
}

// Fixed runners sized once do not see the jobs change: grown jobs run past
// their runners' memory and over the hosts', and a class whose heavy jobs
// come more often than it was sized for has its queue grow. pumasi, which
// measured the jobs as they were, keeps every host within its memory and
// its longest wait shorter.
func TestDriftAfterTheFixedRunnersWereSized(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		sc := Film
		sc.Seed, sc.Drift = seed, Drifts[0] // jobs grow
		rs := DriftRun(sc)
		perClass, refit, pumasi := rs[0], rs[3], rs[4]
		if perClass.Summary.OverRunner == 0 || perClass.Summary.HostOverSecs == 0 {
			t.Fatalf("seed %d, grown jobs on runners sized before: %+v", seed, perClass.Summary)
		}
		if refit.Summary.OverRunner != 0 || pumasi.Summary.HostOverSecs != 0 {
			t.Fatalf("seed %d: sized again %+v, pumasi %+v", seed, refit.Summary, pumasi.Summary)
		}
		sc.Drift = Drifts[1] // heavy jobs every push
		rs = DriftRun(sc)
		if rs[4].Summary.MaxWait >= rs[0].Summary.MaxWait {
			t.Fatalf("seed %d, heavy jobs: pumasi waits up to %gs, per-class sized before %gs", seed, rs[4].Summary.MaxWait, rs[0].Summary.MaxWait)
		}
	}
}

// With no drift, the stream is the one it always was.
func TestNoDriftIsTheSameStream(t *testing.T) {
	sc := Film
	sc.Drift = Drift{Name: "none"}
	a, b := Stream(sc), Stream(Film)
	if len(a) != len(b) || a[10] != b[10] {
		t.Fatalf("streams differ")
	}
}
