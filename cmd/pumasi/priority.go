package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
)

// runPriority shows the priority rules of the config file and where they put
// the jobs waiting now. It changes nothing: the rules are changed in the
// file, and a running `pumasi up` reads them again by itself.
func runPriority(args []string) error {
	fs := flag.NewFlagSet("pumasi priority", flag.ContinueOnError)
	cfgPath := fs.String("config", "pumasi.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	w := os.Stdout
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(cfg.Priority) == 0 {
		fmt.Fprintf(w, "no priority rules in %s: jobs get room oldest first.\n", *cfgPath)
		fmt.Fprintln(w, "Add them under `priority:`, the first match counts, for example:")
		fmt.Fprintln(w, "  priority:\n    - {match: {workflow: \"deploy-*.yml\"}, level: high}\n    - {match: {event: schedule}, level: low}")
	} else {
		fmt.Fprintln(tw, "RULE\tLEVEL\tMATCH")
		for i, r := range cfg.Priority {
			fmt.Fprintf(tw, "%d\t%s\t%s\n", i+1, r.Level, r)
		}
		tw.Flush()
		fmt.Fprintf(w, "\nA high job is placed as if it had come %s earlier, a low one %s later: nobody waits forever.\n", cfg.PriorityHeadStart, cfg.PriorityHeadStart)
	}
	fmt.Fprintln(w, "The rules order classes, not the jobs inside one: GitHub gives a class's runners its oldest job first,")
	fmt.Fprintln(w, "so the jobs before a high job in its class move up with it. A running `pumasi up` reads the rules")
	fmt.Fprintln(w, "again when the file changes; no running job is stopped for them.")

	if _, err := os.Stat(cfg.Ledger); err != nil {
		return nil // nothing has run yet
	}
	l, err := ledger.Open(cfg.Ledger)
	if err != nil {
		return err
	}
	defer l.Close()
	open, err := l.OpenDemand()
	if err != nil {
		return err
	}
	fmt.Fprintln(w)
	if len(open) == 0 {
		fmt.Fprintln(w, "no jobs waiting")
		return nil
	}
	var ds []decide.Demand
	wf := map[string]string{}
	for _, d := range open {
		dd := decide.Demand{ID: d.JobID, Class: d.Class, FirstSeen: d.FirstSeen, Name: d.Job}
		dd.Level, dd.Ranked = cfg.LevelOf(config.WaitingJob{Class: d.Class, Repo: d.Repo, WorkflowRef: d.Workflow, Name: d.Job, Event: d.Event})
		ds = append(ds, dd)
		file, _, _ := strings.Cut(d.Workflow, "@")
		wf[d.JobID] = path.Base(file)
	}
	// Jobs a runner is already coming up for are still listed: only `up` knows which.
	fmt.Fprintln(tw, "LINE\tCLASS\tJOB\tWORKFLOW\tWAITED\tWHY HERE")
	now := time.Now()
	for i, d := range decide.Line(ds, nil, cfg.PriorityHeadStart) {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", i+1, d.Class, dash(d.Name), dash(wf[d.ID]), now.Sub(d.FirstSeen).Round(time.Second), cmpOr(d.Ranked, "by its age"))
	}
	return tw.Flush()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
