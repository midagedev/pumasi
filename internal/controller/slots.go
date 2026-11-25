package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

// Classes with slots: are served by registered runners that the hosts keep
// and run by themselves. The controller's part is small: it hands every host
// a plan (which classes may listen there, under which budget), reads back
// what the hosts report, and tells people about it. No GitHub token is
// involved, and a host follows its last plan while the controller is away.

// slotHost is what the plans are made from: one host that is up.
type slotHost struct {
	Name, Kind, OS string
	BudgetCPUs     float64
	BudgetMem      int64
	Quarantined    bool
	Paths          []string
	Slots          []api.Slot
}

// slotPlans is the plan of every host. Pure: the same hosts give the same plans.
//
// A class is in a host's plan when its OS and its hosts: allow it there. It
// is closed there while the host lacks one of its mounts, and on a desktop
// while a server still has a runner of the class listening: servers first,
// as with scale sets.
func slotPlans(cfg *config.Config, hosts []slotHost) map[string]api.SlotPlan {
	eligible := func(cl config.Class, h slotHost) bool {
		return cl.Registered() && osOf(cl.OS) == osOf(h.OS) && (len(cl.Hosts) == 0 || slices.Contains(cl.Hosts, h.Name))
	}
	lacks := func(cl config.Class, h slotHost) string {
		for _, m := range cl.Mounts {
			if p := config.MountPath(m); !slices.Contains(h.Paths, p) {
				return p
			}
		}
		return ""
	}
	// The server that still listens for a class, if any.
	serverListens := map[string]string{}
	for _, cl := range cfg.Classes {
		for _, h := range hosts {
			if h.Kind != "server" || h.Quarantined || !eligible(cl, h) || lacks(cl, h) != "" {
				continue
			}
			for _, s := range h.Slots {
				idle := s.State == "listening" || s.State == "starting" ||
					(s.State == "off" && (s.Why == "" || strings.HasPrefix(s.Why, "no plan")))
				if s.Class == cl.Name && s.N <= cl.Slots && idle && serverListens[cl.Name] == "" {
					serverListens[cl.Name] = h.Name
				}
			}
		}
	}
	plans := map[string]api.SlotPlan{}
	for _, h := range hosts {
		p := api.SlotPlan{URL: cfg.URL, Image: cfg.Image, BudgetCPUs: h.BudgetCPUs, BudgetMem: h.BudgetMem,
			PSICPUMax: cfg.PSICPUThreshold, DiskMinFree: cfg.DiskMinFree, ProvisionSec: int64(cfg.ProvisionTimeout.Seconds())}
		if h.Quarantined {
			p.Closed = "quarantined"
		}
		for _, cl := range cfg.Classes {
			if !eligible(cl, h) {
				continue
			}
			sc := api.SlotClass{Name: cl.Name, CPUs: cl.CPUs, MemBytes: int64(cl.Memory), Slots: cl.Slots, Reserve: cl.Reserve,
				Labels: cl.RunsOn(), Env: cl.Env, Path: cl.Path, MaxDurationSec: int64(cl.MaxDuration.Seconds())}
			switch {
			case lacks(cl, h) != "":
				sc.Closed = "lacks " + lacks(cl, h)
			case h.Kind == "desktop" && serverListens[cl.Name] != "":
				sc.Closed = "servers first: " + serverListens[cl.Name] + " listens"
			}
			p.Classes = append(p.Classes, sc)
		}
		p.Version = agent.PlanVersion(p)
		plans[h.Name] = p
	}
	return plans
}

func osOf(os string) string { return cmp.Or(os, "linux") }

func (c *Controller) hasSlots() bool {
	return slices.ContainsFunc(c.cfg.Classes, func(cl config.Class) bool { return cl.Registered() })
}

// syncSlots is the registered runners' part of a round: plans out, reports in.
func (c *Controller) syncSlots(ctx context.Context, now time.Time) {
	var hosts []slotHost
	for _, h := range c.hosts {
		if h.snap == nil {
			continue
		}
		c.mu.Lock()
		quarantined := !h.quarantined.IsZero()
		c.mu.Unlock()
		hosts = append(hosts, slotHost{Name: h.cfg.Name, Kind: cmp.Or(h.snap.Kind, h.cfg.Kind()), OS: h.snap.OS,
			BudgetCPUs: h.cfg.Budget.CPUs, BudgetMem: int64(h.cfg.Budget.Memory), Quarantined: quarantined, Paths: h.snap.Paths, Slots: h.snap.Slots})
	}
	plans := slotPlans(c.cfg, hosts)
	var wg sync.WaitGroup
	for _, h := range c.hosts {
		if h.snap == nil {
			continue
		}
		p := plans[h.cfg.Name]
		// A host that never had a plan gets one only when there is something in it.
		if p.Version == h.snap.SlotPlan || (len(p.Classes) == 0 && h.snap.SlotPlan == "") {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			err := h.agent.SetSlotPlan(cctx, p)
			var se *api.StatusError
			key := "no-slots/" + h.cfg.Name
			switch {
			case err == nil:
				c.book.resolve(key)
				c.ev.Write("slot-plan", map[string]any{"host": h.cfg.Name, "plan": p})
			case errors.As(err, &se) && (se.Code == http.StatusNotImplemented || se.Code == http.StatusNotFound):
				why := cmp.Or(se.Body, "its agent is an older build")
				if se.Code == http.StatusNotFound {
					why = "its agent is an older build"
				}
				c.book.open(key, "no-slots", h.cfg.Name, fmt.Sprintf("%s keeps no registered runners (%s); classes with slots: do not run there", h.cfg.Name, why))
			default:
				c.log.Warn(fmt.Sprintf("host %s did not take its plan for registered runners; it goes on with the last one", h.cfg.Name), "err", err)
			}
		}()
	}
	wg.Wait()

	registered := map[string]int{}
	for _, h := range c.hosts {
		if h.snap == nil {
			continue
		}
		name := h.cfg.Name
		for _, j := range h.snap.SlotJobs {
			c.slotJob(name, j, now)
		}
		over := 0.0
		for _, s := range h.snap.Slots {
			registered[s.Class]++
			key := fmt.Sprintf("slot-failing/%s/%s", name, s.Name)
			if s.State == "failing" {
				c.book.open(key, "slot-failing", s.Name, fmt.Sprintf("registered runner %s (%s slot %d on %s) cannot listen: %s; it is tried again every 5 minutes",
					s.Name, s.Class, s.N, name, s.Why))
			} else if s.State == "listening" || s.State == "busy" {
				c.book.resolve(key)
			}
			if s.Runner == nil {
				continue
			}
			was := c.slotSeen[s.Runner.Name]
			if was != s.State {
				switch s.State {
				case "listening":
					c.log.Info(fmt.Sprintf("listen %s on %s  (%s)", s.Name, name, s.Runner.Name))
				case "busy":
					c.log.Info(fmt.Sprintf("job %s on %s  (%s on %s)", cmp.Or(s.Job, "?"), s.Name, s.Runner.Name, name))
				}
				c.slotSeen[s.Runner.Name] = s.State
			}
			if s.State == "busy" {
				if cl := c.classByName(s.Class); cl != nil {
					over += cl.cfg.CPUs
				}
			}
		}
		// The host stops no listener that has a job on its way, so two classes
		// that took jobs in the same second can run over the budget.
		budget := minPositive(h.cfg.Budget.CPUs, h.snap.BudgetCPUs)
		for _, r := range h.snap.Runners {
			if r.State != "exited" && r.State != "gone" {
				over += r.CPUs
			}
		}
		key := "over-budget/" + name
		if budget > 0 && over > budget+1e-9 {
			c.book.open(key, "over-budget", name, fmt.Sprintf(
				"%s runs %.1f cpus of jobs on a budget of %.1f: runners of two classes took jobs at the same moment; nothing is stopped, it ends with those jobs", name, over, budget))
		} else {
			c.book.resolve(key)
		}
	}
	// Runs that are gone need no memory.
	for run := range c.slotSeen {
		live := false
		for _, h := range c.hosts {
			if h.snap == nil {
				live = true // not known: keep
				break
			}
			for _, s := range h.snap.Slots {
				live = live || (s.Runner != nil && s.Runner.Name == run)
			}
		}
		if !live {
			delete(c.slotSeen, run)
		}
	}
	if len(hosts) > 0 && c.adopted {
		for _, cl := range c.cfg.Classes {
			if !cl.Registered() || registered[cl.Name] > 0 || c.slotTold[cl.Name] {
				continue
			}
			c.slotTold[cl.Name] = true
			c.log.Warn(fmt.Sprintf("class %s has no registered runner on any connected host: its jobs wait at GitHub until `pumasi register` (or `pumasi adopt` on a host) gives it one", cl.Name))
		}
	}
}

// slotJob records an ended job of a registered runner the first time a host
// reports it, and tells what went wrong with it.
func (c *Controller) slotJob(host string, j api.SlotJob, now time.Time) {
	if c.slotJobs[j.Name] {
		return
	}
	c.slotJobs[j.Name] = true
	row := ledger.SlotJob{Name: j.Name, Class: j.Class, Host: host, Job: j.Job, State: "done", Result: strings.ToLower(j.Result),
		ProvisionMs: j.ProvisionMs, LogTail: j.LogTail,
		Usage: ledger.Usage{CPUSec: j.CPUSec, MemPeak: j.MemPeak, IOBytes: j.IOBytes, ExitCode: j.ExitCode, OOMKills: j.OOMKills}}
	if j.ListeningAt != nil {
		row.ListeningAt = *j.ListeningAt
	}
	if j.JobStartedAt != nil {
		row.StartedAt = *j.JobStartedAt
	}
	row.FinishedAt = now
	if j.ExitedAt != nil {
		row.FinishedAt = *j.ExitedAt
	}
	what := cmp.Or(j.Job, "a job")
	switch {
	case j.Stopped != "":
		row.State, row.Reason = "failed", j.Stopped
	case j.Result == "":
		row.State, row.Reason = "failed", "the runner ended under its job"
	}
	inserted, err := c.ledger.InsertSlotJob(row)
	if err != nil {
		c.log.Error("ledger slot job", "err", err)
		return
	}
	if !inserted {
		return // told by an earlier controller
	}
	c.ev.Write("slot-job", map[string]any{"host": host, "job": j})
	switch {
	case j.Stopped != "":
		c.log.Info(fmt.Sprintf("stop %s  %s", j.Registered, j.Stopped))
		c.book.report("problem", "max-duration/"+j.Name, "max-duration", j.Registered, fmt.Sprintf(
			"%s on %s ran longer than the %s maxDuration; the runner was stopped and GitHub will fail the job", what, host, j.Class), "runner stopped; not rerun")
	case j.Result == "":
		code := "unknown"
		if j.ExitCode != nil {
			code = fmt.Sprint(*j.ExitCode)
		}
		c.log.Info(fmt.Sprintf("died %s on %s", what, j.Registered))
		c.book.report("problem", "runner-died/"+j.Name, "runner-died", j.Registered, fmt.Sprintf(
			"registered runner %s died while running %s on %s (exit code %s); GitHub will fail that job", j.Registered, what, host, code),
			"its files are removed and the slot listens again; a registered runner's job is not rerun by pumasi")
	default:
		c.log.Info(fmt.Sprintf("done %s on %s  %s", what, j.Registered, strings.ToLower(j.Result)))
	}
	if j.OOMKills > 0 {
		c.book.report("problem", "oom/"+j.Name, "oom", j.Registered, fmt.Sprintf(
			"%s hit the %s memory limit on %s (peak %s)", what, j.Class, host, gib(j.MemPeak)), "none: the job or the class size has to change")
	}
	if j.Leftover != "" {
		c.book.report("problem", "leftover/"+j.Name, "leftover", j.Registered, fmt.Sprintf(
			"the files of %s on %s are removed, but the host may not delete all of them: %s", what, host, j.Leftover),
			"none: delete it on the host (usually files a job wrote as root through a container)")
	}
}

// RegisterOutcome is what one host did with a registration token.
type RegisterOutcome struct {
	Host string `json:"host"`
	api.RegisterResult
}

// Register hands a registration token to the connected hosts (host "" = all
// of them), which register runners for their empty slots. The token is kept
// nowhere: it lives in this call and in the hosts' config.sh runs.
func (c *Controller) Register(ctx context.Context, token, class, only string) ([]RegisterOutcome, error) {
	if !c.hasSlots() {
		return nil, errors.New("no class of the config has slots:, so there is nothing to register")
	}
	if class != "" {
		if cl := c.classByName(class); cl == nil || !cl.cfg.Registered() {
			return nil, fmt.Errorf("class %q has no slots: in the config", class)
		}
	}
	c.hostsMu.Lock()
	hosts := slices.Clone(c.hosts)
	c.hostsMu.Unlock()
	var targets []*host
	for _, h := range hosts {
		if (only == "" || h.cfg.Name == only) && h.connected() {
			targets = append(targets, h)
		}
	}
	if len(targets) == 0 {
		if only != "" {
			return nil, fmt.Errorf("host %s is not connected", only)
		}
		return nil, errors.New("no host is connected (`pumasi invite` prints the line that joins one)")
	}
	out := make([]RegisterOutcome, len(targets))
	var wg sync.WaitGroup
	for i, h := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			res, err := h.agent.RegisterSlots(cctx, api.RegisterRequest{Token: token, Class: class})
			if err != nil {
				res.Problem = err.Error()
			}
			out[i] = RegisterOutcome{Host: h.cfg.Name, RegisterResult: res}
			if len(res.Registered) > 0 {
				c.log.Info(fmt.Sprintf("host %s  registered %s", h.cfg.Name, strings.Join(res.Registered, ", ")))
			}
		}()
	}
	wg.Wait()
	select {
	case c.kick <- struct{}{}:
	default:
	}
	return out, nil
}

// RunnerTokenEnv names the variable `register` and `down` read a one-time
// runner token from: a registration token to register with, a registration
// or removal token to remove registrations with.
const RunnerTokenEnv = "PUMASI_RUNNER_TOKEN"

// removalToken is the token the hosts remove their registrations with when
// they leave: the operator's, or one fetched with the controller's GitHub
// token when it has one. "" = none; the hosts then keep the runners' files.
func (c *Controller) removalToken(ctx context.Context) string {
	if t := strings.TrimSpace(os.Getenv(RunnerTokenEnv)); t != "" {
		return t
	}
	if c.token == "" || !c.hasSlots() {
		return ""
	}
	base, err := runnersAPI(c.cfg.URL)
	if err != nil {
		c.log.Warn("no removal token: " + err.Error())
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/remove-token", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubREST.Do(req)
	if err != nil {
		c.log.Warn("no removal token", "err", err)
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		c.log.Warn(fmt.Sprintf("no removal token: GitHub answered %s; the hosts keep the files of their registered runners", resp.Status))
		return ""
	}
	return out.Token
}

// runnersAPI is the REST path of the self-hosted runners of a registration
// URL: a repository, an organization or an enterprise.
func runnersAPI(regURL string) (string, error) {
	u, err := url.Parse(regURL)
	if err != nil {
		return "", err
	}
	base := "https://api.github.com"
	if u.Host != "github.com" {
		base = u.Scheme + "://" + u.Host + "/api/v3"
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "enterprises":
		return base + "/enterprises/" + parts[1] + "/actions/runners", nil
	case len(parts) == 2:
		return base + "/repos/" + parts[0] + "/" + parts[1] + "/actions/runners", nil
	case len(parts) == 1 && parts[0] != "":
		return base + "/orgs/" + parts[0] + "/actions/runners", nil
	}
	return "", fmt.Errorf("url %s is neither a repository, an organization nor an enterprise", regURL)
}
