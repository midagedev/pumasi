package controller

import (
	"context"
	"fmt"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

// A class with overflow: true takes only as many jobs as it can run now. The
// number goes to GitHub with every poll for messages (maxCapacity): GitHub
// assigns the scale set no more jobs than that, and gives the others to
// whatever else serves the same labels — a fixed runner, another scale set.
// pumasi never hears of those jobs.

// Overflow is one such class for `pumasi status`.
type Overflow struct {
	Class string `json:"class"`
	// Take: how many jobs the class may hold at once right now. Held: how
	// many of those it holds (waiting for a runner here, or running).
	Take  int    `json:"take"`
	Held  int    `json:"held"`
	After string `json:"after,omitempty"`
}

// measuredPoll is the message session of an overflow class: each poll carries
// the class's take of the moment, and a poll that is out when the take
// changes is given up and sent again. GitHub holds a poll open for most of a
// minute, and room that came or went meanwhile would otherwise not be told.
// A message is GitHub's until it is deleted, so a poll given up loses none.
type measuredPoll struct {
	listener.Client
	cl *class
}

func (p *measuredPoll) GetMessage(ctx context.Context, lastMessageID, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	for {
		select {
		case <-p.cl.retaken: // told before this poll: it carries the new number anyway
		default:
		}
		pctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			select {
			case <-p.cl.retaken:
				cancel()
			case <-done:
			}
		}()
		msg, err := p.Client.GetMessage(pctx, lastMessageID, int(p.cl.take.Load()))
		close(done)
		again := err != nil && ctx.Err() == nil && pctx.Err() != nil
		cancel()
		if !again {
			return msg, err
		}
	}
}

// setTakes works out each overflow class's take from the plan just made: the
// jobs it holds and has a place for, plus the room the hosts have left for more. A class
// that comes after another offers room only while that one has none.
func (c *Controller) setTakes(s decide.Snapshot, p decide.Plan) {
	held := map[string]int{}
	for _, d := range s.Demand {
		held[d.Class]++
	}
	for _, r := range s.Runners {
		if r.JobKnown && r.State == "busy" {
			held[r.Class]++
		}
	}
	unplaced := map[string]int{}
	for _, d := range p.Defer {
		unplaced[d.Class] += d.Count
	}
	// An expedited job the class holds is kept, with the jobs ahead of it, even
	// without a place: room is being kept for it here.
	for _, l := range p.Line {
		if l.Expedite > 0 && !l.Pending && l.Host == "" {
			unplaced[l.Class]--
		}
	}
	for _, cl := range c.classes {
		if !cl.cfg.Overflow {
			continue
		}
		room := p.Room[cl.cfg.Name]
		if a := cl.cfg.After; a != "" && p.Room[a] > 0 {
			room = 0
		}
		// A job the class holds and cannot place is not asked for: GitHub takes
		// back one above the number when its assignment comes round (every five
		// minutes) and gives it to another runner of its labels that is free.
		keep := max(0, held[cl.cfg.Name]-max(0, unplaced[cl.cfg.Name]))
		take := int32(keep + room)
		cl.held.Store(int32(held[cl.cfg.Name]))
		if old := cl.take.Swap(take); old != take {
			c.log.Debug(fmt.Sprintf("class %s takes %d jobs at once (was %d): holds %d, room for %d more", cl.cfg.Name, take, old, held[cl.cfg.Name], room))
			if c.ev != nil {
				c.ev.Write("take", map[string]any{"class": cl.cfg.Name, "take": take, "was": old, "held": held[cl.cfg.Name], "room": room})
			}
			select {
			case cl.retaken <- struct{}{}:
			default:
			}
		}
	}
}

// nextSize is what the next job of a class is taken to need while nobody
// knows which job it will be: the largest of the sizes of the jobs GitHub has
// lately given the class. The largest, because taking a job too many makes it
// wait here while a runner elsewhere could have had it. A job counts by what
// is known of it anywhere, as it would be sized if it came: its own runs
// under another class, or its workflow's other jobs when it has none — the
// class need not have run it, and a job it gave back is still counted. Zero
// when nothing of the class's jobs is measured: its next one is then counted
// as the guess it is.
func (c *Controller) nextSize(cl config.Class) size.Estimate {
	var next size.Estimate
	keys, err := c.ledger.AssignedJobs(cl.Name, nextJobs)
	if err != nil {
		return next
	}
	platform := cl.Platform()
	byWorkflow := map[ledger.JobKey]size.Estimate{}
	for _, k := range keys {
		k.Platform = platform
		runs, err := c.ledger.JobRuns(k, size.Window)
		if err != nil {
			continue
		}
		var e size.Estimate
		if len(runs) > 0 {
			e = size.Learn(runs, size.Estimate{})
		} else {
			w := ledger.JobKey{Repo: k.Repo, Workflow: k.Workflow, Platform: platform}
			known, ok := byWorkflow[w]
			if !ok {
				if wr, err := c.ledger.WorkflowRuns(k.Repo, k.Workflow, platform, size.Window); err == nil && len(wr) > 0 {
					known = size.Learn(wr, size.Estimate{})
				}
				byWorkflow[w] = known
			}
			e = known
		}
		next.CPUs, next.MemBytes = max(next.CPUs, e.CPUs), max(next.MemBytes, e.MemBytes)
	}
	return next
}

// nextJobs: how many of a class's latest jobs its next one is sized by.
const nextJobs = 50
