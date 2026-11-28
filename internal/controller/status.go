package controller

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
)

// Status is what a running controller knows beyond the ledger: each agent's
// last snapshot (nil = unreachable) and the jobs the last round could not place.
type Status struct {
	Hosts   map[string]*api.Snapshot `json:"hosts"`
	Waiting []Waiting                `json:"waiting"`
	// Closed: hosts that are up but take no new runners, with the reason.
	Closed map[string]string `json:"closed,omitempty"`
	// Links: how each host is attached and how that connection is doing.
	Links map[string]Link `json:"links,omitempty"`
	// Overflow: the classes that take only what fits, and how many jobs each takes now.
	Overflow []Overflow `json:"overflow,omitempty"`
	// Warnings: what is not a problem of the moment but of the setup (exposure.go).
	Warnings []string `json:"warnings,omitempty"`
	// Line: each job that waited for room in the last round, in the order of
	// the line, with what came of it. Covering: per class, how many of its
	// oldest jobs runners that are up without a job will take (not in Line).
	Line     []decide.Lined `json:"line,omitempty"`
	Covering map[string]int `json:"covering,omitempty"`
	// Expect: when each waiting job is expected to start, by its id, as the
	// last round played it forward (decide.Forecast). A job without one waits
	// for something no end in sight opens.
	Expect map[string]Expect `json:"expect,omitempty"`
	// Over: by id, the memory a waiting job would need past its class's hard
	// limit; it is held at the limit and will be cut short there again.
	Over map[string]int64 `json:"over,omitempty"`
	// Warm: per class, how many runners it keeps up without a job now
	// (decide.WarmCount of how its jobs came lately).
	Warm map[string]int `json:"warm,omitempty"`
	// Soon: per class, how many jobs are expected within the time a runner
	// takes to come up, as the jobs they need end (decide.Soon).
	Soon map[string]int `json:"soon,omitempty"`
	// GiveWay: runners up without a job that would step aside for a job that
	// comes, as the last round counted them (decide.Plan.GiveWay).
	GiveWay []string `json:"giveWay,omitempty"`
}

// Expect is when a waiting job is expected to start, where, and whose end
// makes the room ("" = there is room, or its runner is coming up).
type Expect struct {
	StartsAt time.Time `json:"startsAt"`
	Host     string    `json:"host"`
	After    string    `json:"after,omitempty"`
}

// Link is the connection to one host, whichever side opened it.
type Link struct {
	SeenAt   time.Time `json:"seenAt"`             // the last answer (zero = none yet)
	RTTMs    float64   `json:"rttMs"`              // how long that answer took
	Agent    string    `json:"agent,omitempty"`    // the agent's build
	LastDrop string    `json:"lastDrop,omitempty"` // why the host was last cut off
}

type Waiting struct {
	Class  string `json:"class"`
	Jobs   int    `json:"jobs"`
	Reason string `json:"reason"`
	// Priority: why the class's next job stands where it does in line ("" = by its age).
	// The list is in the order of the line.
	Priority string `json:"priority,omitempty"`
}

func (c *Controller) setStatus(snap decide.Snapshot, plan decide.Plan, expect map[string]Expect) {
	defers := plan.Defer
	st := Status{Hosts: map[string]*api.Snapshot{}, Waiting: []Waiting{}, Closed: map[string]string{}, Links: map[string]Link{},
		Line: plan.Line, Covering: map[string]int{}, Expect: expect, GiveWay: plan.GiveWay}
	for _, d := range snap.Demand {
		if d.Over > 0 {
			if st.Over == nil {
				st.Over = map[string]int64{}
			}
			st.Over[d.ID] = d.Over
		}
	}
	stopped := map[string]bool{}
	for _, s := range plan.Stop {
		stopped[s.Runner] = true
	}
	for _, r := range snap.Runners {
		if !r.JobKnown && !stopped[r.Name] && r.Container != "exited" && r.State != "done" {
			st.Covering[r.Class]++
		}
	}
	for _, h := range c.hosts {
		st.Hosts[h.cfg.Name] = h.snap
		st.Links[h.cfg.Name] = Link{SeenAt: h.seenAt, RTTMs: float64(h.rtt.Microseconds()) / 1000, Agent: h.version, LastDrop: h.why()}
		if h.snap != nil && h.closed != "" {
			st.Closed[h.cfg.Name] = h.closed
		}
	}
	for _, d := range defers {
		st.Waiting = append(st.Waiting, Waiting{Class: d.Class, Jobs: d.Count, Reason: d.Reason, Priority: d.Priority})
	}
	for _, cl := range snap.Classes {
		if cl.Warm > 0 {
			if st.Warm == nil {
				st.Warm = map[string]int{}
			}
			st.Warm[cl.Name] = cl.Warm
		}
		if cl.Soon > 0 {
			if st.Soon == nil {
				st.Soon = map[string]int{}
			}
			st.Soon[cl.Name] = cl.Soon
		}
	}
	st.Warnings = c.exposure()
	for _, cl := range c.classes {
		if cl.cfg.Overflow {
			st.Overflow = append(st.Overflow, Overflow{Class: cl.cfg.Name, Take: int(cl.take.Load()), Held: int(cl.held.Load()), After: cl.cfg.After})
		}
	}
	c.mu.Lock()
	c.status = st
	c.mu.Unlock()
}

func addrFile(cfg *config.Config) string { return filepath.Join(cfg.StateDir(), "controller.addr") }

// serveStatus answers `pumasi status` on a loopback port written beside the ledger.
func (c *Controller) serveStatus() (func(), error) {
	l, err := net.Listen("tcp", c.cfg.Dashboard)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(addrFile(c.cfg), []byte(l.Addr().String()+"\n"), 0o600); err != nil {
		l.Close()
		return nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: c.webHandler()}
	c.log.Info("dashboard http://" + l.Addr().String() + "/  (`pumasi open`)")
	switch {
	case !onLoopback(c.cfg.Dashboard) && c.cfg.DashboardToken == "":
		c.log.Info("the dashboard is open to other machines for reading; its writes (expedite, mode, priority rules) come only from this machine: dashboardToken takes them from elsewhere")
	case !onLoopback(c.cfg.Dashboard):
		c.log.Info("the dashboard is open to other machines; its writes (expedite, mode, priority rules) need Authorization: Bearer <dashboardToken>, from this machine too")
	}
	go func() { _ = srv.Serve(l) }()
	return func() {
		_ = srv.Close()
		_ = os.Remove(addrFile(c.cfg))
	}, nil
}

// ReadStatus asks the running controller. ok is false when none answers.
func ReadStatus(cfg *config.Config) (st Status, ok bool) {
	b, err := os.ReadFile(addrFile(cfg))
	if err != nil {
		return st, false
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + strings.TrimSpace(string(b)) + "/api/live")
	if err != nil {
		return st, false
	}
	defer resp.Body.Close()
	return st, json.NewDecoder(resp.Body).Decode(&st) == nil
}

// DashboardURL is where the running controller serves its dashboard ("" = none running).
func DashboardURL(cfg *config.Config) string {
	if _, ok := ReadStatus(cfg); !ok {
		return ""
	}
	b, _ := os.ReadFile(addrFile(cfg))
	return "http://" + strings.TrimSpace(string(b)) + "/"
}
