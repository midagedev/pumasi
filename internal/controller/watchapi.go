package controller

import (
	"net/http"
	"strconv"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/watch"
)

// watchRoutes: the trend of every job and where it changed (GET /api/trend),
// for the dashboard and for agents. The query takes what `pumasi trend`
// takes: since (14d), week (1), job, workflow, repo, branch.
func watchRoutes(mux *http.ServeMux, src func(*http.Request) dashSource) {
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
