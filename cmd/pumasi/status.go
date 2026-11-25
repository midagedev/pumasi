package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ledger"
)

type statusOut struct {
	Controller bool            `json:"controller"` // a `pumasi up` is running and answered
	Hosts      []hostStatus    `json:"hosts"`
	Runners    []runnerStatus  `json:"runners"`
	Waiting    []waitingStatus `json:"waiting"`
}

type hostStatus struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	UsedCPUs   float64 `json:"usedCpus"` // sum of the sizes of the runners placed here
	BudgetCPUs float64 `json:"budgetCpus"`
	UsedMem    int64   `json:"usedMem"`
	BudgetMem  int64   `json:"budgetMem"`
	// CPU pressure (PSI some avg10, percent); nil when the agent did not answer.
	CPUPressure *float64 `json:"cpuPressure"`
	Runners     int      `json:"runners"`
	Reachable   *bool    `json:"reachable"` // nil = unknown, no controller running
}

type runnerStatus struct {
	Name       string `json:"name"`
	Class      string `json:"class"`
	Host       string `json:"host"`
	State      string `json:"state"`
	Job        string `json:"job"`
	AgeSeconds int64  `json:"ageSeconds"`
}

type waitingStatus struct {
	Class  string `json:"class"`
	Jobs   int    `json:"jobs"`
	Reason string `json:"reason"`
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("pumasi status", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.Ledger); err != nil {
		return fmt.Errorf("nothing to show yet: %s does not exist (run `pumasi up` first)", cfg.Ledger)
	}
	l, err := ledger.Open(cfg.Ledger)
	if err != nil {
		return err
	}
	defer l.Close()
	out, err := collectStatus(cfg, l, time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printStatus(os.Stdout, out)
	return nil
}

func collectStatus(cfg *config.Config, l *ledger.Ledger, now time.Time) (statusOut, error) {
	out := statusOut{Hosts: []hostStatus{}, Runners: []runnerStatus{}, Waiting: []waitingStatus{}}
	live, ok := controller.ReadStatus(cfg)
	out.Controller = ok

	runners, err := l.LiveRunners()
	if err != nil {
		return out, err
	}
	size := map[string]config.Class{}
	for _, c := range cfg.Classes {
		size[c.Name] = c
	}
	hosts := map[string]int{} // name -> index in out.Hosts
	for _, h := range cfg.Hosts {
		hs := hostStatus{Name: h.Name, Kind: h.Kind, BudgetCPUs: h.Budget.CPUs, BudgetMem: int64(h.Budget.Memory)}
		if ok {
			snap := live.Hosts[h.Name]
			up := snap != nil
			hs.Reachable = &up
			if up {
				p := snap.PSI["cpu"]
				hs.CPUPressure = &p
			}
		}
		out.Hosts = append(out.Hosts, hs)
		hosts[h.Name] = len(out.Hosts) - 1
	}
	for _, r := range runners {
		out.Runners = append(out.Runners, runnerStatus{Name: r.Name, Class: r.Class, Host: r.Host, State: r.State,
			Job: r.Job, AgeSeconds: int64(now.Sub(r.CreatedAt).Seconds())})
		if i, ok := hosts[r.Host]; ok {
			h := &out.Hosts[i]
			h.Runners++
			h.UsedCPUs += size[r.Class].CPUs
			h.UsedMem += int64(size[r.Class].Memory)
		}
	}

	if ok {
		for _, w := range live.Waiting {
			out.Waiting = append(out.Waiting, waitingStatus(w))
		}
		return out, nil
	}
	// No controller: every open job in the ledger waits, with the last reason recorded for its class.
	open, err := l.OpenDemand()
	if err != nil {
		return out, err
	}
	idx := map[string]int{}
	for _, d := range open {
		i, seen := idx[d.Class]
		if !seen {
			i = len(out.Waiting)
			idx[d.Class] = i
			out.Waiting = append(out.Waiting, waitingStatus{Class: d.Class, Reason: l.LastDefer(d.Class)})
		}
		out.Waiting[i].Jobs++
	}
	return out, nil
}

func printStatus(w io.Writer, s statusOut) {
	if !s.Controller {
		fmt.Fprintln(w, "no controller running (start one with `pumasi up`); showing the ledger only")
		fmt.Fprintln(w)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tKIND\tCPUS\tMEMORY\tCPU PRESSURE\tRUNNERS\tSTATE")
	for _, h := range s.Hosts {
		pressure, state := "-", "unknown"
		if h.CPUPressure != nil {
			pressure = fmt.Sprintf("%.1f%%", *h.CPUPressure)
		}
		if h.Reachable != nil {
			state = "unreachable"
			if *h.Reachable {
				state = "ok"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%g/%g\t%s/%s\t%s\t%d\t%s\n", h.Name, h.Kind, h.UsedCPUs, h.BudgetCPUs,
			gib(h.UsedMem), gib(h.BudgetMem), pressure, h.Runners, state)
	}
	tw.Flush()

	fmt.Fprintln(w)
	if len(s.Runners) == 0 {
		fmt.Fprintln(w, "no runners")
	} else {
		fmt.Fprintln(tw, "RUNNER\tCLASS\tHOST\tSTATE\tJOB\tAGE")
		for _, r := range s.Runners {
			job := r.Job
			if job == "" {
				job = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Class, r.Host, r.State, job, time.Duration(r.AgeSeconds)*time.Second)
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if len(s.Waiting) == 0 {
		fmt.Fprintln(w, "no jobs waiting")
		return
	}
	fmt.Fprintln(tw, "WAITING\tJOBS\tWHY")
	for _, wt := range s.Waiting {
		reason := wt.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", wt.Class, wt.Jobs, reason)
	}
	tw.Flush()
}

func gib(b int64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }
