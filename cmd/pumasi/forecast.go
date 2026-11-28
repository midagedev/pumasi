package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/watch"
)

const forecastUsage = `pumasi forecast: how fast CI grows, and at that rate when its busiest hours
need every core and when the wait for room doubles.

  pumasi forecast                  the running up's hosts and its ledger
  pumasi forecast --cores 48       the ledger, against 48 cores
  pumasi forecast --weeks 12       read 12 weeks (default 8; 3 or more)
  pumasi forecast --json           the same as JSON (also GET /api/forecast)
  pumasi forecast --demo           a made-up repository

Growth is the Theil–Sen slope of the weekly cpu-hours, with its 80% interval:
eight weeks of a team whose merges come at random tell it only to a few
percent a week, and every date below moves with it. The busiest hours are the
95th percentile of the hours' cores in use. The waits replay the last week's
jobs, each taking the cores it used, first come first served, on all the cores
as one pool, with more runs at the times runs came as CI grows; hosts of mixed
sizes, memory and sizing by the largest recent run make real waits come sooner.

Flags:
`

func runForecast(args []string) error {
	fs := flag.NewFlagSet("pumasi forecast", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), forecastUsage); fs.PrintDefaults() }
	var src source
	src.flags(fs)
	cores := fs.Float64("cores", 0, "the hosts' cores for CI (default: ask the running up)")
	weeks := fs.Int("weeks", 8, "weeks to read")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *weeks < 3 {
		fs.Usage()
		return flag.ErrHelp
	}
	var rep watch.ForecastReport
	switch {
	case *cores == 0 && !src.demo && src.ledgerPath == "":
		// The running up knows its hosts.
		cfg, err := config.Load(src.cfgPath)
		if err != nil {
			return err
		}
		if err := call(cfg, "GET", "/api/forecast?weeks="+strconv.Itoa(*weeks), nil, &rep); err != nil {
			fmt.Fprintf(os.Stderr, "%v; reading the ledger without the hosts (--cores N for when they fill)\n", err)
			if rep, err = local(&src, 0, *weeks); err != nil {
				return err
			}
		}
	default:
		var err error
		if rep, err = local(&src, *cores, *weeks); err != nil {
			return err
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printForecast(os.Stdout, rep)
	return nil
}

// local: the forecast from the ledger (or the demo), against the cores
// asked (--cores), else the cores the demo's hosts lend.
func local(src *source, asked float64, weeks int) (watch.ForecastReport, error) {
	runs, now, err := src.runs(time.Now().Add(-time.Duration(weeks) * 7 * 24 * time.Hour))
	if err != nil {
		return watch.ForecastReport{}, err
	}
	cores := asked
	if cores == 0 && src.demo {
		cores = controller.DemoCores()
	}
	rep := watch.Forecast(runs, cores, now, weeks)
	switch {
	case asked > 0:
		rep.SetCores("--cores", false)
	case cores > 0:
		rep.SetCores("the demo's hosts", true)
	}
	return rep, nil
}

func printForecast(w io.Writer, rep watch.ForecastReport) {
	fmt.Fprintln(w, rep.Words)
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WEEK FROM\tJOBS\tCPU-HOURS\tBUSIEST HOURS (CORES)\tWAIT p90")
	for _, wk := range rep.Weeks {
		note := ""
		if wk.Partial {
			note = "  (the ledger starts later: not in the growth)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%.0f\t%.1f\t%s%s\n", wk.From.Local().Format("Mon Jan 2"), wk.Jobs, wk.CPUHours, wk.PeakCores, secs(wk.WaitP90), note)
	}
	tw.Flush()
	if rep.Cores > 0 {
		if rep.Lent {
			fmt.Fprintf(w, "\ncores: %.0f the hosts lend (%s: each its budget, else its cores; none counted twice where runners share them)\n", rep.Cores, rep.CoresFrom)
		} else {
			fmt.Fprintf(w, "\ncores: %.0f (%s)\n", rep.Cores, rep.CoresFrom)
		}
	}
}
