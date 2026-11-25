package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/join"
)

// joinLink: the host comes to the controller and keeps one connection open.
// The agent API travels over that connection; the host's own service keeps
// its agent alive, so there is nothing to upload or restart from here.
type joinLink struct {
	srv  *join.Server
	id   string
	peer *join.Peer
	http *http.Client
	api  *api.Client
}

// How long to wait for a host that should be dialing in (its backoff tops out at 30s).
const joinWait = 40 * time.Second

func newJoinLink(srv *join.Server, m join.Member) *joinLink {
	p := srv.Peer(m.ID)
	hc := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{DialContext: p.Dial, DisableKeepAlives: true}}
	base := "http://" + m.Name
	return &joinLink{srv: srv, id: m.ID, peer: p, http: hc,
		api: &api.Client{BaseURL: func(context.Context) (string, error) { return base, nil }, HTTP: hc}}
}

func (l *joinLink) Agent() *api.Client { return l.api }
func (l *joinLink) Via() string        { return "join" }
func (l *joinLink) Why() string        { return l.peer.Link().Why }
func (l *joinLink) Close()             {}

func (l *joinLink) wait(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, joinWait)
	defer cancel()
	if l.peer.Wait(ctx) {
		return nil
	}
	if why := l.Why(); why != "" {
		return fmt.Errorf("the host is not connected (%s)", why)
	}
	return fmt.Errorf("the host has not connected")
}

func (l *joinLink) Ensure(ctx context.Context, _ string) (string, error) {
	if err := l.wait(ctx); err != nil {
		// Not an error of this controller: the host dials when it can.
		return "not connected yet, waiting for it to dial in", nil
	}
	return "connected", nil
}

func (l *joinLink) Revive(context.Context, string) (string, error) { return "", nil }

func (l *joinLink) Running(ctx context.Context) ([]string, error) {
	if err := l.wait(ctx); err != nil {
		return nil, err
	}
	s, err := l.api.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range s.Runners {
		if r.State == "running" {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

// Clean asks the host to remove itself: containers, service, files. A host
// that is not connected cannot be asked; it has to run `pumasi leave`.
func (l *joinLink) Clean(ctx context.Context) (string, error) {
	if err := l.wait(ctx); err != nil {
		// Down removes what the controller knows either way; the host still has its side.
		l.srv.Forget(l.id)
		return "", fmt.Errorf("%w; run `pumasi leave` on that host to remove what is there", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://host/v1/leave", nil)
	if err != nil {
		return "", err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Containers int    `json:"containers"`
		Problem    string `json:"problem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the host did not clean up (%s)", resp.Status)
	}
	if out.Problem != "" {
		return "", fmt.Errorf("the host did not clean up: %s", out.Problem)
	}
	l.srv.Forget(l.id)
	return fmt.Sprint(out.Containers), nil
}

func memberHost(m join.Member) config.Host {
	h := config.Host{Name: m.Name, Kind: m.Kind}
	h.Budget.CPUs, h.Budget.Memory = m.CPUs, config.Bytes(m.Mem)
	return h
}

// KnownHosts is the hosts of the config followed by the hosts that joined.
func KnownHosts(cfg *config.Config) []config.Host {
	out := slices.Clone(cfg.Hosts)
	for _, m := range join.ReadMembers(cfg.StateDir()) {
		if !slices.ContainsFunc(out, func(h config.Host) bool { return h.Name == m.Name }) {
			out = append(out, memberHost(m))
		}
	}
	return out
}

// syncJoined makes the host list follow the members: a host that joined is
// added, one that left is dropped (its runners then close as "not configured").
// It runs on the goroutine that evaluates; nothing else changes the list.
func (c *Controller) syncJoined(announce bool) {
	if c.join == nil {
		return
	}
	members := c.join.Members()
	hosts := slices.Clone(c.hosts)
	hosts = slices.DeleteFunc(hosts, func(h *host) bool {
		if h.tr == nil || h.tr.Via() != "join" || slices.ContainsFunc(members, func(m join.Member) bool { return m.Name == h.cfg.Name }) {
			return false
		}
		c.log.Info(fmt.Sprintf("host %s left", h.cfg.Name))
		c.book.resolve("host-unreachable/" + h.cfg.Name)
		c.book.resolve("agent-version/" + h.cfg.Name)
		return true
	})
	for _, m := range members {
		mh := memberHost(m)
		i := slices.IndexFunc(hosts, func(h *host) bool { return h.cfg.Name == m.Name })
		if i >= 0 {
			if h := hosts[i]; h.tr != nil && h.tr.Via() == "join" {
				h.cfg = mh // the host may have joined again with another budget
			}
			continue
		}
		l := newJoinLink(c.join, m)
		hosts = append(hosts, &host{cfg: mh, started: true, tr: l, agent: l.Agent()})
		if announce {
			c.log.Info(fmt.Sprintf("host %s joined  budget %gcpu/%s", m.Name, m.CPUs, gib(m.Mem)))
		}
	}
	c.hostsMu.Lock()
	c.hosts = hosts
	c.hostsMu.Unlock()

	// A host that could not switch to this controller's build says so when it connects.
	for _, h := range hosts {
		l, ok := h.tr.(*joinLink)
		if !ok {
			continue
		}
		key := "agent-version/" + h.cfg.Name
		if ln := l.peer.Link(); ln.Connected && ln.Stay != "" {
			c.book.open(key, "agent-version", h.cfg.Name, fmt.Sprintf(
				"the agent on %s runs another build than this controller and could not switch: %s", h.cfg.Name, ln.Stay))
		} else if ln.Connected {
			c.book.resolve(key)
		}
	}
}

// listenForHosts opens the join listener when the config asks for one.
func (c *Controller) listenForHosts(distDir string) error {
	if c.cfg.Listen == "" {
		if n := len(c.join.Members()); n > 0 {
			c.log.Warn(fmt.Sprintf("%d hosts joined earlier but `listen:` is not set; they cannot connect", n))
		}
		return nil
	}
	c.join.Dist = distDir
	addr, err := c.join.Listen(c.cfg.Listen)
	if err != nil {
		return err
	}
	c.log.Info(fmt.Sprintf("hosts join at %s  (`pumasi invite` prints the line to run on a host)", addr))
	return nil
}
