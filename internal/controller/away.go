package controller

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/midagedev/pumasi/internal/api"
)

// A controller that is gone cannot tell anyone. Its hosts notice — their
// connection stays down — so two of them are given notify.webhook and say so:
// the first after notify.away, the second after twice that. Two, not all:
// every host would say the same thing at the same minute; one alone may be
// the machine that went down together with the controller.

// awayHosts is how many hosts speak.
const awayHosts = 2

type awayPushed struct {
	sent  string
	since time.Time
}

// syncAway tells each connected host its part, whenever that changes or the
// host has connected anew. It runs on the evaluating goroutine; the telling
// itself does not hold it up.
func (c *Controller) syncAway(ctx context.Context) {
	after := c.cfg.Notify.AwayAfter()
	var connected []*host
	for _, h := range c.hosts {
		if d := h.awayDone.Swap(nil); d != nil {
			h.awaySent, h.awaySince = d.sent, d.since
		}
		if h.connected() {
			connected = append(connected, h)
		} else {
			h.awayRank = 0
		}
	}
	// Who speaks stays who it was; servers before desktops (a desktop sleeps).
	slices.SortStableFunc(connected, func(a, b *host) int {
		rank := func(h *host) int { return cmp.Or(h.awayRank, awayHosts+1) }
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.cfg.Kind(), b.cfg.Kind())*-1, cmp.Compare(a.cfg.Name, b.cfg.Name))
	})
	name, _ := os.Hostname()
	for i, h := range connected {
		was := h.awayRank
		h.awayRank = 0
		var want api.Away
		if after > 0 && i < awayHosts {
			h.awayRank = i + 1
			want = api.Away{Webhook: c.cfg.Notify.Webhook, AfterSec: int64(after / time.Second), Rank: i + 1, Controller: name}
			for _, cl := range c.cfg.Classes {
				want.Classes = append(want.Classes, cl.Name)
			}
		}
		if h.awayRank != was && h.awayRank > 0 {
			c.log.Info(fmt.Sprintf("host %s tells notify.webhook when it has not reached this controller for %s", h.cfg.Name, after*time.Duration(h.awayRank)))
		}
		body, _ := json.Marshal(want)
		since := h.peer.Load().Link().Since
		if h.awaySent == string(body) && h.awaySince.Equal(since) {
			continue
		}
		if !h.awayBusy.CompareAndSwap(false, true) {
			continue
		}
		go func() {
			defer h.awayBusy.Store(false)
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(cctx, http.MethodPut, "http://host/v1/away", bytes.NewReader(body))
			if err != nil {
				return
			}
			resp, err := h.http.Do(req)
			if err != nil {
				return // asked again on the next evaluation
			}
			resp.Body.Close()
			// 404: an agent of a build before this; it switches builds and connects anew.
			if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
				h.awayDone.Store(&awayPushed{sent: string(body), since: since})
			}
		}()
	}
}
