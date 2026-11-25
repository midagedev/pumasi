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
}

// Link is the connection to one host, whichever side opened it.
type Link struct {
	Via      string    `json:"via"`                // ssh | join
	SeenAt   time.Time `json:"seenAt"`             // the last answer (zero = none yet)
	RTTMs    float64   `json:"rttMs"`              // how long that answer took
	Agent    string    `json:"agent,omitempty"`    // the agent's build
	LastDrop string    `json:"lastDrop,omitempty"` // why the host was last cut off
}

type Waiting struct {
	Class  string `json:"class"`
	Jobs   int    `json:"jobs"`
	Reason string `json:"reason"`
}

func (c *Controller) setStatus(defers []decide.Defer) {
	st := Status{Hosts: map[string]*api.Snapshot{}, Waiting: []Waiting{}, Closed: map[string]string{}, Links: map[string]Link{}}
	for _, h := range c.hosts {
		st.Hosts[h.cfg.Name] = h.snap
		if h.tr != nil {
			ln := Link{Via: h.tr.Via(), SeenAt: h.seenAt, RTTMs: float64(h.rtt.Microseconds()) / 1000, LastDrop: h.tr.Why()}
			if h.snap != nil {
				ln.Agent = h.snap.Version
			}
			st.Links[h.cfg.Name] = ln
		}
		if h.snap != nil && h.closed != "" {
			st.Closed[h.cfg.Name] = h.closed
		}
	}
	for _, d := range defers {
		st.Waiting = append(st.Waiting, Waiting{Class: d.Class, Jobs: d.Count, Reason: d.Reason})
	}
	c.mu.Lock()
	c.status = st
	c.mu.Unlock()
}

func addrFile(cfg *config.Config) string { return filepath.Join(cfg.StateDir(), "controller.addr") }

// serveStatus answers `pumasi status` on a loopback port written beside the ledger.
func (c *Controller) serveStatus() (func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(addrFile(c.cfg), []byte(l.Addr().String()+"\n"), 0o600); err != nil {
		l.Close()
		return nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: c.webHandler()}
	c.log.Info("dashboard http://" + l.Addr().String() + "/  (`pumasi open`)")
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
