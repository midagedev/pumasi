package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

// watchRoutes: the trend of every job and where it changed (GET /api/trend),
// and one run, pull request or commit against the usual (GET /api/why-slow),
// for the dashboard and for agents. /api/trend takes what `pumasi trend`
// takes: since (14d), week (1), job, workflow, repo, branch; /api/why-slow
// one of run (a number or URL), pr, commit; /api/forecast weeks (8) and
// cores (default: the hosts' now, each its budget if it has one).
func watchRoutes(mux *http.ServeMux, src func(*http.Request) dashSource) {
	mux.HandleFunc("GET /api/forecast", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		q := r.URL.Query()
		weeks := 8
		if v := q.Get("weeks"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 3 || n > 52 {
				http.Error(w, "weeks: 3 to 52", http.StatusBadRequest)
				return
			}
			weeks = n
		}
		cores, from := 0.0, ""
		if v := q.Get("cores"); v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n <= 0 {
				http.Error(w, "cores: a number over 0", http.StatusBadRequest)
				return
			}
			cores, from = n, "asked"
		} else {
			cores, from = hostCores(s)
		}
		now := watchNow(s)
		runs, err := s.runs(now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep := watch.Forecast(runs, cores, now, weeks)
		rep.CoresFrom = from
		writeJSON(w, rep)
	})
	mux.HandleFunc("GET /api/why-slow", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		q := r.URL.Query()
		var t watch.Target
		var err error
		switch {
		case q.Get("run") != "":
			t, err = watch.ParseTarget(q.Get("run"))
		case q.Get("pr") != "":
			t.PR, err = strconv.Atoi(strings.TrimPrefix(q.Get("pr"), "#"))
		case q.Get("commit") != "":
			t, err = watch.ParseTarget(q.Get("commit"))
		default:
			err = errors.New("give one of run, pr, commit")
		}
		if err != nil || t == (watch.Target{}) {
			http.Error(w, fmt.Sprint("why-slow: ", err), http.StatusBadRequest)
			return
		}
		runs, err := s.runs(watchNow(s).Add(-60 * 24 * time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep, err := watch.WhySlowAt(runs, t, watchNow(s))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, rep)
	})
	mux.HandleFunc("GET /api/trend", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		q := r.URL.Query()
		since := watch.TrendSpan
		if v := q.Get("since"); v != "" {
			d, err := config.ParseSpan(v)
			if err != nil || d <= 0 {
				http.Error(w, "since: "+strconv.Quote(v)+" is not a span (14d, 6w, 36h)", http.StatusBadRequest)
				return
			}
			since = d
		}
		now := watchNow(s)
		from := watch.TrendFrom(now, since)
		runs, err := s.runs(from.Add(-watch.Baseline))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		week, _ := strconv.ParseBool(q.Get("week"))
		f := watch.Filter{Job: q.Get("job"), Workflow: q.Get("workflow"), Repo: q.Get("repo"), Branch: q.Get("branch")}
		writeJSON(w, watch.BuildTrend(runs, watch.TrendQuery{From: from, To: now, Week: week, Loc: now.Location(), Filter: f}, watch.Options{}))
	})
}

// hostCores: the cores CI has on the hosts now (each host's budget when it
// has one, else its cores: what it lends runners, not counting cores shared
// by placement), and where that came from. The demo's are its whole fleet's,
// a host its story takes away for a while included, as the CLI's are.
func hostCores(s dashSource) (float64, string) {
	if s.demo() {
		return DemoCores(), "the demo's hosts"
	}
	total := 0.0
	for _, h := range s.live().Hosts {
		if h == nil {
			continue
		}
		total += lendCores(h.BudgetCPUs, h.CPUs)
	}
	if total == 0 {
		return 0, ""
	}
	return total, "the hosts joined now"
}

// watchNow is the moment the CI watch reads up to: now, or for the demo
// the start of today, where its history ends (the same numbers all day).
func watchNow(s dashSource) time.Time {
	if s.demo() {
		return synth.DemoEnd(s.now())
	}
	return s.now()
}

// lendCores: what a host lends runners, its budget or else its cores.
func lendCores(budget, cpus float64) float64 {
	if budget > 0 {
		return budget
	}
	return cpus
}

// DemoCores are the cores the demo's hosts lend, what `pumasi forecast
// --demo` and the demo's /api/forecast set its history against.
func DemoCores() float64 {
	total := 0.0
	for _, h := range demoHosts {
		total += lendCores(h.budgetCPUs, h.cpus)
	}
	return total
}
