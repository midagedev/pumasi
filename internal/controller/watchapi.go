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
		now := s.now()
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
		runs, err := s.runs(s.now().Add(-60 * 24 * time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rep, err := watch.WhySlow(runs, t)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, rep)
	})
	mux.HandleFunc("GET /api/trend", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		q := r.URL.Query()
		since := 14 * 24 * time.Hour
		if v := q.Get("since"); v != "" {
			d, err := config.ParseSpan(v)
			if err != nil || d <= 0 {
				http.Error(w, "since: "+strconv.Quote(v)+" is not a span (14d, 6w, 36h)", http.StatusBadRequest)
				return
			}
			since = d
		}
		now := s.now()
		from := now.Add(-since)
		from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location())
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
// has one), and where that came from.
func hostCores(s dashSource) (float64, string) {
	if s.demo() {
		return synth.DemoCores, "the demo's hosts"
	}
	total := 0.0
	for _, h := range s.live().Hosts {
		if h == nil {
			continue
		}
		if h.BudgetCPUs > 0 {
			total += h.BudgetCPUs
		} else {
			total += h.CPUs
		}
	}
	if total == 0 {
		return 0, ""
	}
	return total, "the hosts joined now"
}
