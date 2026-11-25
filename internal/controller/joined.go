package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/join"
)

// Every host comes to the controller and keeps one connection open; the agent
// API travels over it and the host's own service keeps its agent alive. A host
// of the config (hosts: - ssh: X) differs in one thing only: the controller
// goes there over ssh to run the invite line for it (bootstrap.go).

// How long to wait for a host that should be dialing in (its backoff tops out at 30s).
const joinWait = 40 * time.Second

func newHost(cfg config.Host) *host {
	h := &host{cfg: cfg}
	h.http = &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{DialContext: h.dial, DisableKeepAlives: true}}
	base := "http://" + cfg.Name
	h.agent = &api.Client{BaseURL: func(context.Context) (string, error) { return base, nil }, HTTP: h.http}
	return h
}

var errNotJoined = errors.New("the host has not joined")

func (h *host) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	p := h.peer.Load()
	if p == nil {
		return nil, errNotJoined
	}
	return p.Dial(ctx, network, addr)
}

// why says why the host was last cut off ("" = not known).
func (h *host) why() string {
	if p := h.peer.Load(); p != nil {
		return p.Link().Why
	}
	return ""
}

func (h *host) connected() bool {
	p := h.peer.Load()
	return p != nil && p.Link().Connected
}

// wait blocks until the host is connected, for at most joinWait.
func (h *host) wait(ctx context.Context) error {
	p := h.peer.Load()
	if p == nil {
		return errNotJoined
	}
	ctx, cancel := context.WithTimeout(ctx, joinWait)
	defer cancel()
	if p.Wait(ctx) {
		return nil
	}
	if why := h.why(); why != "" {
		return fmt.Errorf("the host is not connected (%s)", why)
	}
	return errors.New("the host has not connected")
}

// running lists the runners that are up on the host.
func (h *host) running(ctx context.Context) ([]string, error) {
	if err := h.wait(ctx); err != nil {
		return nil, err
	}
	s, err := h.agent.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range s.Runners {
		if r.State == "running" {
			names = append(names, r.Name)
		}
	}
	// Of the registered runners only those with a job: an idle listener is
	// stopped without loss when the host leaves.
	for _, sl := range s.Slots {
		if sl.State == "busy" && sl.Runner != nil {
			names = append(names, sl.Runner.Name)
		}
	}
	return names, nil
}

// leave asks a connected host to remove itself: runners, service, files. It
// returns how many runners that took.
// runnerToken ("" = none) lets the host also remove the registrations of the
// runners pumasi registered there.
func (h *host) leave(ctx context.Context, runnerToken string) (api.LeaveResult, error) {
	var out api.LeaveResult
	body, _ := json.Marshal(api.LeaveRequest{RunnerToken: runnerToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://host/v1/leave", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("the host did not clean up (%s)", resp.Status)
	}
	if out.Problem != "" {
		return out, fmt.Errorf("the host did not clean up: %s", out.Problem)
	}
	return out, nil
}

func memberHost(m join.Member) config.Host {
	h := config.Host{Name: m.Name, Desktop: m.Kind == "desktop"}
	h.Budget.CPUs, h.Budget.Memory = m.CPUs, config.Bytes(m.Mem)
	return h
}

// KnownHosts is the hosts of the config followed by the hosts that joined by themselves.
func KnownHosts(cfg *config.Config) []config.Host {
	out := slices.Clone(cfg.Hosts)
	for _, m := range join.ReadMembers(cfg.StateDir()) {
		if !slices.ContainsFunc(out, func(h config.Host) bool { return h.Name == m.Name }) {
			out = append(out, memberHost(m))
		}
	}
	return out
}

// syncJoined makes the host list follow the members. A host of the config is
// always on the list and gets its connection when it has joined. A host that
// joined by itself is added with its first connection and dropped when it
// leaves (its runners then close as "not configured"). It runs on the
// goroutine that evaluates; nothing else changes the list.
func (c *Controller) syncJoined(announce bool) {
	if c.join == nil {
		return
	}
	members := c.join.Members()
	member := func(name string) (join.Member, bool) {
		i := slices.IndexFunc(members, func(m join.Member) bool { return m.Name == name })
		if i < 0 {
			return join.Member{}, false
		}
		return members[i], true
	}
	hosts := slices.Clone(c.hosts)
	hosts = slices.DeleteFunc(hosts, func(h *host) bool {
		m, ok := member(h.cfg.Name)
		switch {
		case ok && m.ID != h.id:
			h.id = m.ID // joined, or joined again as another id
			h.peer.Store(c.join.Peer(m.ID))
		case !ok && h.id != "":
			h.id = ""
			h.peer.Store(nil)
		}
		if ok || h.cfg.SSH != "" {
			if ok && h.cfg.SSH == "" {
				h.cfg = memberHost(m) // the host may have joined again with another budget
			}
			return false
		}
		c.log.Info(fmt.Sprintf("host %s left", h.cfg.Name))
		c.book.resolve("host-unreachable/" + h.cfg.Name)
		c.book.resolve("agent-version/" + h.cfg.Name)
		return true
	})
	for _, m := range members {
		if slices.ContainsFunc(hosts, func(h *host) bool { return h.cfg.Name == m.Name }) {
			continue
		}
		p := c.join.Peer(m.ID)
		if announce {
			// Registered a moment ago: it becomes a host with its first connection,
			// not an unreachable one in between.
			if !p.Link().Connected {
				continue
			}
			c.log.Info(fmt.Sprintf("host %s joined  budget %gcpu/%s", m.Name, m.CPUs, gib(m.Mem)))
		}
		h := newHost(memberHost(m))
		h.id = m.ID
		h.peer.Store(p)
		hosts = append(hosts, h)
	}
	c.hostsMu.Lock()
	c.hosts = hosts
	c.hostsMu.Unlock()

	// A host that could not switch to this controller's build says so when it connects.
	for _, h := range hosts {
		p := h.peer.Load()
		if p == nil {
			continue
		}
		key := "agent-version/" + h.cfg.Name
		if ln := p.Link(); ln.Connected && ln.Stay != "" {
			c.book.open(key, "agent-version", h.cfg.Name, fmt.Sprintf(
				"the agent on %s runs another build than this controller and could not switch: %s", h.cfg.Name, ln.Stay))
		} else if ln.Connected {
			c.book.resolve(key)
		}
	}
}

// listenForHosts opens the listener the hosts dial.
func (c *Controller) listenForHosts(distDir string) error {
	c.join.Dist = distDir
	addr, err := c.join.Listen(c.cfg.Listen)
	if err != nil {
		return fmt.Errorf("%w (hosts connect to this controller there; set listen: to another address or port)", err)
	}
	c.listenAddr = addr.String()
	c.log.Info(fmt.Sprintf("hosts join at %s  (`pumasi invite` prints the line to run on a host)", addr))
	return nil
}
