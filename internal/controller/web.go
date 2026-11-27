package controller

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/ledger"
)

// The dashboard: static files, no build step, nothing fetched from elsewhere.
//
//go:embed web
var webFiles embed.FS

// dashSource is where the dashboard's data comes from: the running
// controller, or the demo's simulated one. One is made per request, so the
// demo can answer for the moment the request asks about.
type dashSource interface {
	live() Status
	state() (State, error)
	jobs() ([]ledger.FinishedJob, error)
	job(runner string) ([]ledger.Decision, string, error)
	series() (Series, error)
	url() string
	demo() bool
	now() time.Time
	// priority: the priority controls beyond the config (nil = none here).
	priority() *PriorityView
	// apis: the /api routes this server answers beyond /api/live, so the page
	// asks only for what is there.
	apis() []string
}

// webMux serves the dashboard and its read-only JSON.
func webMux(src func(*http.Request) dashSource) *http.ServeMux {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, src(r).live()) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		st, err := s.state()
		reply(w, struct {
			State
			URL  string   `json:"url"`
			Demo bool     `json:"demo,omitempty"`
			API  []string `json:"api"`
		}{st, s.url(), s.demo(), s.apis()}, err)
	})
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs, err := src(r).jobs()
		reply(w, jobs, err)
	})
	mux.HandleFunc("GET /api/job", func(w http.ResponseWriter, r *http.Request) {
		ds, tail, err := src(r).job(r.URL.Query().Get("runner"))
		reply(w, map[string]any{"decisions": ds, "logTail": tail}, err)
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		s, err := src(r).series()
		reply(w, s, err)
	})
	// What the dashboard shows, as a report a person or an agent reads without
	// the picture: the problems, the line and why, and what to type next.
	mux.HandleFunc("GET /api/explain", func(w http.ResponseWriter, r *http.Request) {
		s := src(r)
		st, err := s.state()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now, pv := s.now(), s.priority()
		fs := Explain(st, pv, now)
		if r.URL.Query().Get("format") == "json" {
			writeJSON(w, struct {
				At         time.Time `json:"at"`
				Controller bool      `json:"controller"`
				Demo       bool      `json:"demo"`
				Findings   []Finding `json:"findings"`
			}{now, st.Controller, s.demo(), append([]Finding{}, fs...)})
			return
		}
		var b bytes.Buffer
		WriteExplain(&b, st, fs, pv, s.url(), s.demo(), now)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b.Bytes())
	})
	return mux
}

// ctlSource is the running controller.
type ctlSource struct{ c *Controller }

func (s ctlSource) live() Status { return s.c.liveStatus() }
func (s ctlSource) state() (State, error) {
	return CollectState(s.c.cfg, s.c.ledger, s.c.liveStatus(), true, s.c.now())
}
func (s ctlSource) jobs() ([]ledger.FinishedJob, error) { return s.c.ledger.RecentJobs(50) }
func (s ctlSource) job(runner string) ([]ledger.Decision, string, error) {
	return s.c.ledger.RunnerHistory(runner)
}
func (s ctlSource) series() (Series, error) { return s.c.series(s.c.now()) }
func (s ctlSource) url() string             { return s.c.cfg.URL }
func (s ctlSource) demo() bool              { return false }
func (s ctlSource) now() time.Time          { return s.c.now() }
func (s ctlSource) priority() *PriorityView {
	v, err := CollectPriority(s.c.cfgSnapshot(), s.c.ledger, s.c.liveStatus(), true, s.c.now())
	if err != nil {
		return nil
	}
	return &v
}
func (s ctlSource) apis() []string {
	return []string{"state", "jobs", "job", "series", "explain", "metrics", "priority"}
}

// webHandler serves the dashboard, its JSON and /metrics on the status listener.
func (c *Controller) webHandler() http.Handler {
	mux := webMux(func(*http.Request) dashSource { return ctlSource{c} })
	c.priorityRoutes(mux) // expedite, mode, runtime rules (opsapi.go)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		if err := c.writeMetrics(w); err != nil {
			c.log.Error("metrics", "err", err)
		}
	})
	return mux
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (c *Controller) liveStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, v)
}

// Series is the last 24 hours in 10-minute steps, computed from the ledger so
// it survives controller restarts.
type Series struct {
	T        []int64              `json:"t"`        // step end, unix seconds
	HostCPUs map[string][]float64 `json:"hostCpus"` // cpus held by runners on each host
	Waiting  []int                `json:"waiting"`  // jobs waiting at that moment
	// Median wait of the jobs that got a runner during the step; -1 when none did.
	WaitMedianSec []float64 `json:"waitMedianSec"`
}

func (c *Controller) series(now time.Time) (Series, error) {
	const step, steps = 10 * time.Minute, 144
	start := now.Add(-steps * step)
	runners, err := c.ledger.RunnerSpans(start)
	if err != nil {
		return Series{}, err
	}
	waits, err := c.ledger.WaitSpans(start)
	if err != nil {
		return Series{}, err
	}
	cpus := map[string]float64{}
	for _, cl := range c.cfg.Classes {
		cpus[cl.Name] = cl.CPUs
	}
	s := Series{HostCPUs: map[string][]float64{}}
	// Every host there is now, joined ones too, and every host that held a
	// runner in these 24 hours even if it has left since.
	for _, h := range KnownHosts(c.cfg) {
		s.HostCPUs[h.Name] = make([]float64, steps)
	}
	for _, r := range runners {
		if _, ok := s.HostCPUs[r.Host]; !ok {
			s.HostCPUs[r.Host] = make([]float64, steps)
		}
	}
	covers := func(sp ledger.Span, t time.Time) bool {
		return !sp.From.After(t) && (sp.To.IsZero() || sp.To.After(t))
	}
	for i := range steps {
		t := start.Add(time.Duration(i+1) * step)
		s.T = append(s.T, t.Unix())
		for _, r := range runners {
			if line, ok := s.HostCPUs[r.Host]; ok && covers(r, t) {
				line[i] += cpus[r.Class]
			}
		}
		waiting := 0
		var served []float64
		for _, w := range waits {
			if covers(w, t) {
				waiting++
			}
			if w.Host == "served" && w.To.After(t.Add(-step)) && !w.To.After(t) {
				served = append(served, w.To.Sub(w.From).Seconds())
			}
		}
		s.Waiting = append(s.Waiting, waiting)
		median := -1.0
		if len(served) > 0 {
			slices.Sort(served)
			median = served[len(served)/2]
		}
		s.WaitMedianSec = append(s.WaitMedianSec, median)
	}
	return s, nil
}
