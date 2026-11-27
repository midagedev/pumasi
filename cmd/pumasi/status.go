package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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
	if len(s.Warnings) > 0 {
		fmt.Fprintln(w, "WARNINGS")
		for _, t := range s.Warnings {
			fmt.Fprintln(w, "  "+t)
		}
		fmt.Fprintln(w)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// HELD: what the runners there hold (each its estimate, or its use when that is more).
	// FREE: what a new runner could take now, as measured. CAP: the most pumasi lends there.
	fmt.Fprintln(tw, "HOST\tKIND\tOS\tHELD\tFREE\tCAP\tCPU PRESSURE\tRUNNERS\tSTATE\tSEEN\tRTT\tAGENT\tLAST DROP")
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
		free, limit := "-", "auto"
		if h.MemBytes > 0 {
			free = sizeText(h.FreeCPUs, h.FreeMem)
		}
		if h.BudgetCPUs > 0 || h.BudgetMem > 0 {
			limit = cond(h.BudgetCPUs > 0, fmt.Sprintf("%gcpu", h.BudgetCPUs), "any") + "/" + cond(h.BudgetMem > 0, gib(h.BudgetMem), "any")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", h.Name, h.Kind, cmp.Or(h.OS, "-"), sizeText(h.UsedCPUs, h.UsedMem),
			free, limit, pressure, h.Runners, state, seen, rtt, dash(h.Agent), dash(h.LastDrop))
	}
	tw.Flush()

	fmt.Fprintln(w)
	if len(s.Runners) == 0 {
		fmt.Fprintln(w, "no runners")
	} else {
		// EST: what placement counts on for the runner's job. NOW: what it uses.
		// Over the estimate is fine: the job goes on and its next run is placed bigger.
		fmt.Fprintln(tw, "RUNNER\tCLASS\tHOST\tSLOT\tSTATE\tJOB\tAGE\tEST\tNOW")
		for _, r := range s.Runners {
			job := r.Job
			if job == "" {
				job = "-"
			}
			slot := "-"
			if r.Slot > 0 {
				slot = strconv.Itoa(r.Slot)
			}
			est, cur := sizeText(r.EstCPUs, r.EstMem), "-"
			switch {
			case r.Watched:
				est += " (first, watched)"
			case r.EstRuns == 0:
				est += " (first)"
			}
			if r.MemBytes > 0 {
				cur = sizeText(r.CPUNow, r.MemBytes)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Class, r.Host, slot, r.State, job, time.Duration(r.AgeSeconds)*time.Second, est, cur)
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if s.Mode.Name != "" && s.Mode.Name != controller.ModeNormal {
		fmt.Fprintln(w, "MODE"+strings.TrimPrefix(modeLine(s.Mode, time.Now()), "mode"))
	}
	for _, x := range s.Expedited {
		where := fmt.Sprintf("line %d", x.Position)
		if x.Pending {
			where = "GitHub still holds it"
		}
		fmt.Fprintf(w, "EXPEDITED %s (run %d) of %s, by %s: %s, %d of its class ahead\n", x.Name, x.RunID, x.Class, x.By, where, x.Ahead)
	}
	if len(s.Waiting) == 0 {
		fmt.Fprintln(w, "no jobs waiting")
	} else {
		// In the order of the line: the first class gets the next room that fits it.
		fmt.Fprintln(tw, "WAITING\tJOBS\tLINE\tWHY")
		for _, wt := range s.Waiting {
			line := "-"
			if wt.Order > 0 {
				line = strconv.Itoa(wt.Order) + cond(wt.Priority != "", ": "+wt.Priority, "")
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", wt.Class, wt.Jobs, line, dash(wt.Reason))
		}
		tw.Flush()
	}

	if len(s.Overflow) > 0 {
		// These classes take only what fits. Jobs beyond that go to other
		// runners of the same labels, or wait at GitHub where pumasi does not
		// see them: WAITING counts only what the class took.
		fmt.Fprintln(w)
		fmt.Fprintln(tw, "OVERFLOW\tTAKES\tHOLDS\tNOTE")
		for _, o := range s.Overflow {
			note := "takes only what fits now; jobs beyond that are not seen here (other runners of its labels get them, or they wait at GitHub)"
			if o.After != "" {
				note = "offers room only while " + o.After + " has none; " + note
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", o.Class, o.Take, o.Held, note)
		}
		tw.Flush()
	}

	if len(s.Sizes) > 0 {
		// What the next run of each job is placed with, learned from its last runs.
		fmt.Fprintln(w)
		fmt.Fprintln(tw, "JOB SIZE\tWORKFLOW\tRAN AS\tNEXT RUN\tLAST RUN\tLARGEST\tRUNS")
		for _, j := range s.Sizes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", j.Job, j.Workflow, j.Platform, sizeText(j.EstCPUs, j.EstMem), sizeText(j.LastCPUs, j.LastMem), sizeText(j.TopCPUs, j.TopMem), j.Runs)
		}
		tw.Flush()
	}
}

func sizeText(cpus float64, mem int64) string { return fmt.Sprintf("%.1fcpu/%s", cpus, gib(mem)) }

func cond(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
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
