package controller

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/midagedev/pumasi/internal/agent"
	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/ledger"
)

// writeMetrics is /metrics: the hosts' families as the agents report them,
// then what only the controller knows — runners by state, jobs waiting, what
// overflow classes take, open incidents and the jobs finished so far.
func (c *Controller) writeMetrics(w io.Writer) error {
	live := c.liveStatus()
	var snaps []api.Snapshot
	for _, s := range live.Hosts {
		if s != nil {
			snaps = append(snaps, *s)
		}
	}
	slices.SortFunc(snaps, func(a, b api.Snapshot) int { return cmpString(a.Host, b.Host) })
	st, err := CollectState(c.cfg, c.ledger, live, true, c.now())
	if err != nil {
		return err
	}
	totals, err := c.ledger.JobTotals()
	if err != nil {
		return err
	}
	agent.WriteHostFamilies(w, snaps...)
	writeControllerFamilies(w, st, totals)
	_, err = fmt.Fprintln(w, "# EOF")
	return err
}

func writeControllerFamilies(w io.Writer, st State, totals []ledger.JobTotal) {
	family := func(name, typ, help string) {
		fmt.Fprintf(w, "# TYPE %s %s\n# HELP %s %s\n", name, typ, name, help)
	}
	sample := func(name string, v float64, labels ...string) {
		fmt.Fprintf(w, "%s{%s} %s\n", name, strings.Join(labels, ","), strconv.FormatFloat(v, 'g', -1, 64))
	}
	l := agent.MetricLabel

	family("pumasi_host_reachable", "gauge", "1 while the host's agent answers the controller.")
	for _, h := range st.Hosts {
		v := 0.0
		if h.Reachable != nil && *h.Reachable {
			v = 1
		}
		sample("pumasi_host_reachable", v, l("host", h.Name))
	}

	// Runners by class, host and state; a state with none has no sample.
	type key struct{ class, host, state string }
	n := map[key]int{}
	for _, r := range st.Runners {
		n[key{r.Class, r.Host, r.State}]++
	}
	keys := make([]key, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmpString(a.class+"\x00"+a.host+"\x00"+a.state, b.class+"\x00"+b.host+"\x00"+b.state)
	})
	family("pumasi_runners", "gauge", "Runners pumasi placed, by class, host and state (starting, idle, busy, ...).")
	for _, k := range keys {
		sample("pumasi_runners", float64(n[k]), l("class", k.class), l("host", k.host), l("state", k.state))
	}

	family("pumasi_jobs_waiting", "gauge", "Jobs of the class that wait for a runner here.")
	family("pumasi_jobs_waiting_oldest_seconds", "gauge", "How long the oldest waiting job of the class has waited.")
	for _, q := range st.Waiting {
		sample("pumasi_jobs_waiting", float64(q.Jobs), l("class", q.Class))
		sample("pumasi_jobs_waiting_oldest_seconds", float64(q.OldestSeconds), l("class", q.Class))
	}

	family("pumasi_overflow_take", "gauge", "How many jobs an overflow class may hold at once right now.")
	family("pumasi_overflow_held", "gauge", "How many jobs an overflow class holds (waiting for a runner here, or running).")
	for _, o := range st.Overflow {
		sample("pumasi_overflow_take", float64(o.Take), l("class", o.Class))
		sample("pumasi_overflow_held", float64(o.Held), l("class", o.Class))
	}

	family("pumasi_incidents_open", "gauge", "Problems pumasi has open.")
	sample("pumasi_incidents_open", float64(len(st.Incidents)))

	family("pumasi_jobs", "counter", "Jobs finished on pumasi's runners, by class and result.")
	for _, t := range totals {
		sample("pumasi_jobs_total", float64(t.Jobs), l("class", t.Class), l("result", t.Result))
	}
	family("pumasi_job_seconds", "counter", "Seconds the finished jobs ran, by class and result.")
	for _, t := range totals {
		sample("pumasi_job_seconds_total", t.Seconds, l("class", t.Class), l("result", t.Result))
	}
}
