package controller

import (
	"embed"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/ledger"
)

// The dashboard: three static files, no build step, nothing fetched from elsewhere.
//
//go:embed web
var webFiles embed.FS

// webHandler serves the dashboard and its read-only JSON on the status listener.
func (c *Controller) webHandler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, c.liveStatus()) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		st, err := CollectState(c.cfg, c.ledger, c.liveStatus(), true, c.now())
		reply(w, struct {
			State
			URL string `json:"url"`
		}{st, c.cfg.URL}, err)
	})
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, _ *http.Request) {
		jobs, err := c.ledger.RecentJobs(50)
		reply(w, jobs, err)
	})
	mux.HandleFunc("GET /api/job", func(w http.ResponseWriter, r *http.Request) {
		ds, tail, err := c.ledger.RunnerHistory(r.URL.Query().Get("runner"))
		reply(w, map[string]any{"decisions": ds, "logTail": tail}, err)
	})
	// The one request that changes something: `pumasi register` hands over a
	// registration token for the hosts. Only as JSON: a page in a browser
	// cannot send that to another origin without being asked first.
	mux.HandleFunc("POST /api/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Token, Class, Host string }
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			http.Error(w, "application/json only", http.StatusUnsupportedMediaType)
			return
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Token == "" {
			http.Error(w, "a registration token is required", http.StatusBadRequest)
			return
		}
		out, err := c.Register(r.Context(), req.Token, req.Class, req.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, _ *http.Request) {
		s, err := c.series(c.now())
		reply(w, s, err)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		var snaps []api.Snapshot
		for _, s := range c.liveStatus().Hosts {
			if s != nil {
				snaps = append(snaps, *s)
			}
		}
		slices.SortFunc(snaps, func(a, b api.Snapshot) int { return cmpString(a.Host, b.Host) })
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		agent.WriteMetrics(w, snaps...)
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
