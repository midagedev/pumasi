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
)

// watchRoutes: the trend of every job and where it changed (GET /api/trend),
// and one run, pull request or commit against the usual (GET /api/why-slow),
// for the dashboard and for agents. /api/trend takes what `pumasi trend`
// takes: since (14d), week (1), job, workflow, repo, branch; /api/why-slow
// one of run (a number or URL), pr, commit.
func watchRoutes(mux *http.ServeMux, src func(*http.Request) dashSource) {
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
