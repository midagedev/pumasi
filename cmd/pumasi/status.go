package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/ledger"
)

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
	live, ok := controller.ReadStatus(cfg)
	out, err := controller.CollectState(cfg, l, live, ok, time.Now())
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

func printStatus(w io.Writer, s controller.State) {
	if !s.Controller {
		fmt.Fprintln(w, "no controller running (start one with `pumasi up`); showing the ledger only")
		fmt.Fprintln(w)
	}
	if len(s.Incidents) > 0 {
		fmt.Fprintln(w, "PROBLEMS")
		now := time.Now()
		for _, in := range s.Incidents {
			fmt.Fprintf(w, "  %s  (open %s)\n", in.Detail, now.Sub(in.OpenedAt).Round(time.Second))
		}
		fmt.Fprintln(w)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tKIND\tCPUS\tMEMORY\tCPU PRESSURE\tRUNNERS\tSTATE\tVIA\tSEEN\tRTT\tAGENT\tLAST DROP")
	for _, h := range s.Hosts {
		pressure, state := "-", "unknown"
		if h.CPUPressure != nil {
			pressure = fmt.Sprintf("%.1f%%", *h.CPUPressure)
		}
		if h.Reachable != nil {
			state = "unreachable"
			if *h.Reachable {
				state = "ok"
				if h.Closed != "" {
					state = h.Closed
				}
			}
		}
		seen, rtt := "-", "-"
		if h.SeenSeconds != nil {
			seen = (time.Duration(*h.SeenSeconds) * time.Second).String() + " ago"
			rtt = fmt.Sprintf("%.0fms", h.RTTMs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%g/%g\t%s/%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Name, h.Kind, h.UsedCPUs, h.BudgetCPUs,
			gib(h.UsedMem), gib(h.BudgetMem), pressure, h.Runners, state, h.Via, seen, rtt, dash(h.Agent), dash(h.LastDrop))
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

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func gib(b int64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }

// runOpen opens the running controller's dashboard in the browser.
func runOpen(args []string) error {
	fs := flag.NewFlagSet("pumasi open", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	url := controller.DashboardURL(cfg)
	if url == "" {
		return fmt.Errorf("no controller running: the dashboard is served by `pumasi up`")
	}
	fmt.Println(url)
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, url).Start()
}
