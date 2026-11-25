package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

const (
	quarantineAfter = 3 // provision failures in a row on one host
	rerunGiveUp     = 2 * time.Hour
)

// quarantineProbe is how often a quarantined host is checked (a var for tests).
var quarantineProbe = 5 * time.Minute

// provisionFailed counts a runner that did not come up for a reason on the
// host's side. Three in a row close the host for new runners.
func (c *Controller) provisionFailed(h *host, why string) {
	c.mu.Lock()
	h.provFails++
	n, already := h.provFails, !h.quarantined.IsZero()
	if n >= quarantineAfter && !already {
		h.quarantined, h.probedAt = c.now(), c.now()
	}
	c.mu.Unlock()
	if n >= quarantineAfter && !already {
		c.book.open("quarantine/"+h.cfg.Name, "host-quarantined", h.cfg.Name, fmt.Sprintf(
			"%s quarantined: %d runners in a row failed to start there (last: %s); no new runners placed, checked every %s",
			h.cfg.Name, n, firstLine(why), quarantineProbe))
	}
}

func (c *Controller) provisionOK(h *host) {
	c.mu.Lock()
	h.provFails = 0
	c.mu.Unlock()
}

// probeQuarantined checks each quarantined host (docker answers, the image is
// there or pulls) without starting a runner, and reopens it when the check passes.
func (c *Controller) probeQuarantined(ctx context.Context, now time.Time) {
	for _, h := range c.hosts {
		c.mu.Lock()
		due := !h.quarantined.IsZero() && h.snap != nil && now.Sub(h.probedAt) >= quarantineProbe
		if due {
			h.probedAt = now
		}
		c.mu.Unlock()
		if !due || !h.probing.CompareAndSwap(false, true) {
			continue
		}
		go func() {
			defer h.probing.Store(false)
			cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			problem, err := h.agent.Check(cctx, c.cfg.Image)
			if err != nil {
				problem = err.Error()
			}
			if problem != "" {
				c.log.Info(fmt.Sprintf("host %s stays quarantined: %s", h.cfg.Name, firstLine(problem)))
				return
			}
			c.mu.Lock()
			h.quarantined, h.provFails = time.Time{}, 0
			c.mu.Unlock()
			c.book.resolve("quarantine/" + h.cfg.Name)
		}()
	}
}

// closedReason says why an up host takes no new runners ("" = it does), and
// keeps the disk incident in step.
func (c *Controller) closedReason(h *host) string {
	c.mu.Lock()
	quarantined := !h.quarantined.IsZero()
	c.mu.Unlock()
	disk := ""
	if s := h.snap; s != nil {
		key := "disk-low/" + h.cfg.Name
		if pct := 100 * float64(s.DiskFree) / float64(max(s.DiskTotal, 1)); s.DiskTotal > 0 && pct < c.cfg.DiskMinFree {
			disk = fmt.Sprintf("disk %.0f%% free", pct)
			c.book.open(key, "disk-low", h.cfg.Name, fmt.Sprintf(
				"%s has %s free on docker's disk (%.0f%%, below %.0f%%); no new runners placed there",
				h.cfg.Name, gib(s.DiskFree), pct, c.cfg.DiskMinFree))
		} else if s.DiskTotal > 0 {
			c.book.resolve(key)
		}
	}
	switch {
	case quarantined:
		return "quarantined"
	default:
		return disk
	}
}

// How a runner's end is told and whether its job may be rerun.
const (
	endNormal      = ""
	endRebooted    = "rebooted"     // infrastructure
	endDied        = "died"         // infrastructure: killed from outside, container gone
	endOOM         = "oom"          // the job's own doing
	endMaxDuration = "max-duration" // the user's limit
)

// endCause classifies a stop. final is the agent's last word on the runner
// (nil when the host did not answer). Only a job GitHub never reported as
// completed can have died on our side.
func endCause(before ledger.Runner, st decide.Stop, final *api.Runner) string {
	oom := before.OOMKills > 0 || (final != nil && final.OOMKills > 0)
	switch {
	case strings.HasPrefix(st.Reason, decide.OverMaxDuration):
		return endMaxDuration
	case st.Reason == rebootReason:
		return endRebooted
	case before.State != "busy" || before.Result != "":
		return endNormal
	case oom:
		return endOOM
	case strings.HasPrefix(st.Reason, "container missing"):
		return endDied
	case final != nil && final.ExitCode != nil && *final.ExitCode != 0:
		return endDied
	}
	return endNormal
}

// reportOOM tells once per runner that its job hit the class's memory limit.
func (c *Controller) reportOOM(lr ledger.Runner, ar api.Runner) {
	limit := ar.MemBytes
	if cl := c.classByName(lr.Class); cl != nil {
		limit = int64(cl.cfg.Memory)
	}
	c.book.report("problem", "oom/"+lr.Name, "out-of-memory", lr.Name, fmt.Sprintf(
		"%s hit the %s %s memory limit on %s (peak %s); the kernel killed a process of the job",
		jobName(lr), lr.Class, gib(limit), lr.Host, gib(ar.MemPeak)), "none: raise the class's memory or fix the job; not rerun")
}

// requestRerun queues the run of a job lost to an infrastructure failure.
func (c *Controller) requestRerun(r ledger.Runner, reason string) string {
	if c.cfg.Rerun <= 0 {
		return ""
	}
	if r.RunID == 0 || r.Repo == "" {
		return "not rerun: the job's workflow run is unknown"
	}
	queued, err := c.ledger.RequestRerun(r.Repo, r.RunID, reason, c.now(), c.cfg.Rerun)
	if err != nil {
		c.log.Error("ledger rerun", "err", err)
		return ""
	}
	if !queued {
		return fmt.Sprintf("not rerun: run %d was already rerun %d times", r.RunID, c.cfg.Rerun)
	}
	return fmt.Sprintf("failed jobs of run %d are rerun once GitHub closes the run", r.RunID)
}

// checkReruns asks GitHub to rerun the failed jobs of queued runs. A run can
// only be rerun after it completed, which GitHub takes minutes to decide when a
// runner vanished.
func (c *Controller) checkReruns(ctx context.Context, now time.Time) {
	rs, err := c.ledger.PendingReruns()
	if err != nil || len(rs) == 0 {
		return
	}
	for _, r := range rs {
		if now.Sub(c.rerunCheck[r.RunID]) < 30*time.Second {
			continue
		}
		c.rerunCheck[r.RunID] = now
		key := fmt.Sprintf("rerun/%d/%d", r.RunID, now.Unix())
		status, conclusion, err := c.runStatus(ctx, r.Repo, r.RunID)
		switch {
		case err != nil:
			c.log.Warn("run status", "run", r.RunID, "err", err)
		case status != "completed":
			if now.Sub(r.RequestedAt) > rerunGiveUp {
				_ = c.ledger.RerunSettled(r.RunID, false)
				c.book.report("problem", key, "rerun", fmt.Sprint(r.RunID), fmt.Sprintf(
					"run %d not rerun: GitHub still has it %s %s after %s", r.RunID, status, rerunGiveUp, r.Reason), "gave up")
			}
		case conclusion != "failure":
			_ = c.ledger.RerunSettled(r.RunID, false)
			c.log.Info(fmt.Sprintf("run %d ended %s; nothing to rerun", r.RunID, conclusion))
		default:
			if err := c.rerunFailedJobs(ctx, r.Repo, r.RunID); err != nil {
				c.log.Warn("rerun failed jobs", "run", r.RunID, "err", err)
				continue
			}
			_ = c.ledger.RerunSettled(r.RunID, true)
			c.book.report("recovered", key, "rerun", fmt.Sprint(r.RunID), fmt.Sprintf(
				"reran the failed jobs of run %d: %s", r.RunID, r.Reason), "rerun-failed-jobs")
		}
	}
}

func (c *Controller) rerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST rerun-failed-jobs: %s", resp.Status)
	}
	return nil
}

// adopt records a running container the ledger does not know: the ledger was
// rebuilt, or the runner was started by a controller whose ledger is gone.
func (c *Controller) adopt(ctx context.Context, h *host, ar api.Runner) (ledger.Runner, bool) {
	if c.classByName(ar.Class) == nil || ar.State != "running" {
		return ledger.Runner{}, false
	}
	if _, err := c.ledger.Runner(ar.Name); err == nil {
		return ledger.Runner{}, false // known and closed (written off earlier): not ours to bring back here
	}
	var id int64
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ref, err := c.gh.GetRunnerByName(cctx, ar.Name)
	cancel()
	if err == nil && ref != nil {
		id = int64(ref.ID)
	}
	t := func(p *time.Time) time.Time {
		if p == nil {
			return time.Time{}
		}
		return *p
	}
	created := t(ar.StartedAt)
	if created.IsZero() {
		created = c.now()
	}
	if err := c.ledger.InsertAdopted(ar.Name, ar.Class, h.cfg.Name, id, created, t(ar.ListeningAt), t(ar.JobStartedAt)); err != nil {
		c.log.Error("ledger adopt", "err", err)
		return ledger.Runner{}, false
	}
	lr, err := c.ledger.Runner(ar.Name)
	if err != nil {
		return ledger.Runner{}, false
	}
	c.log.Info(fmt.Sprintf("adopt %s  on %s, not in the ledger (%s)", ar.Name, h.cfg.Name, lr.State))
	return lr, true
}
