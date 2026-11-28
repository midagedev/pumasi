package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/watch"
	"github.com/midagedev/pumasi/internal/watch/synth"
)

// source is where the CLI's watch commands read finished jobs from: the
// config's ledger, a ledger file, or the demo's made-up history.
type source struct {
	cfgPath, ledgerPath string
	demo                bool
	web                 string // where commits and runs are: https://github.com
}

func (s *source) flags(fs *flag.FlagSet) {
	fs.StringVar(&s.cfgPath, "config", "pumasi.yaml", "config file (its ledger is read)")
	fs.StringVar(&s.ledgerPath, "ledger", "", "read this ledger file instead (a copy works)")
	fs.BoolVar(&s.demo, "demo", false, "a made-up repository's last five weeks: no controller or ledger needed")
}

// runs are the finished jobs from `from` on, and the moment that counts as now.
func (s *source) runs(from time.Time) ([]ledger.JobRun, time.Time, error) {
	now := time.Now()
	s.web = "https://github.com"
	if s.demo {
		var out []ledger.JobRun
		for _, r := range synth.Demo(now).Runs {
			if !r.FinishedAt.Before(from) && r.FinishedAt.Before(now) {
				out = append(out, r)
			}
		}
		return out, now, nil
	}
	path := s.ledgerPath
	if path == "" {
		cfg, err := config.Load(s.cfgPath)
		if err != nil {
			return nil, now, err
		}
		path = cfg.Ledger
		if u, err := url.Parse(cfg.URL); err == nil && u.Host != "" {
			s.web = u.Scheme + "://" + u.Host
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, now, fmt.Errorf("no ledger at %s (run `pumasi up` first, or try --demo)", path)
	}
	l, err := ledger.Open(path)
	if err != nil {
		return nil, now, err
	}
	defer l.Close()
	runs, err := l.FinishedRuns(from, time.Time{})
	return runs, now, err
}

const trendUsage = `pumasi trend: how long each job and workflow takes, day by day, and where it changed.

  pumasi trend                      every job, the last 14 days, and the changes in them
  pumasi trend --job unit-tests     only jobs whose name matches (a glob or a part of it)
  pumasi trend --since 6w --week    a row a week
  pumasi trend --days --job build   every day of the jobs that match
  pumasi trend --json               the same as JSON (also GET /api/trend on the dashboard)

Numbers are of runs that succeeded: p50 is the middle run, p90 the slow end.
CHANGE is the typical time in the last third of the span against the first.
CHANGED lists steps that held across runs (not single slow runs), found among
runs whose host was not saturated by others, with the first run after the step:
its commit, its pull request, and the job's steps that grew.

Flags:
`

func runTrend(args []string) error {
	fs := flag.NewFlagSet("pumasi trend", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), trendUsage); fs.PrintDefaults() }
	var src source
	src.flags(fs)
	since := fs.String("since", "14d", "how far back: 14d, 6w, 36h")
	week := fs.Bool("week", false, "a row a week (from Monday) instead of a day")
	days := fs.Bool("days", false, "every day (or week) of each job, not a line a job")
	var f watch.Filter
	fs.StringVar(&f.Job, "job", "", "only jobs whose name matches")
	fs.StringVar(&f.Workflow, "workflow", "", "only workflows whose file matches (ci.yml)")
	fs.StringVar(&f.Repo, "repo", "", "only repositories that match (owner/name)")
	fs.StringVar(&f.Branch, "branch", "", "only runs on this branch")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q (flags go before nothing else: pumasi trend --job %s)", fs.Arg(0), fs.Arg(0))
	}
	span, err := config.ParseSpan(*since)
	if err != nil || span <= 0 {
		return fmt.Errorf("--since: %q is not a span (14d, 6w, 36h)", *since)
	}
	now := time.Now()
	from := dayStart(now.Add(-span))
	runs, now, err := src.runs(from.Add(-watch.Baseline))
	if err != nil {
		return err
	}
	rep := watch.BuildTrend(runs, watch.TrendQuery{From: from, To: now, Week: *week, Loc: time.Local, Filter: f}, watch.Options{})
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if len(rep.Series) == 0 {
		if len(runs) == 0 {
			return errors.New("no finished jobs in the ledger yet")
		}
		return errors.New("no job matches")
	}
	printTrend(os.Stdout, rep, src.web, *days)
	return nil
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func printTrend(w io.Writer, rep watch.TrendReport, web string, days bool) {
	fmt.Fprintf(w, "%s to %s, a row a %s\n\n", rep.From.Format("Mon Jan 2"), rep.To.Format("Mon Jan 2 15:04"), rep.Grain)
	fmt.Fprintln(w, "CHANGED (newest first)")
	if len(rep.Changes) == 0 {
		fmt.Fprintln(w, "  nothing: no job's time, memory or cpu stepped up or down and stayed there")
	}
	for _, g := range groupChanges(rep.Changes) {
		fmt.Fprintln(w, "  "+changeLine(g))
		if more := changeDetail(g[0], web); more != "" {
			fmt.Fprintln(w, "      "+more)
		}
	}
	fmt.Fprintln(w)
	if days {
		for _, s := range rep.Series {
			printDays(w, s)
		}
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "JOB\tRUNS\tTIME p50\tp90\tCHANGE\tMEM p90\tCPU p50\tWAIT p90\tTIME p50 BY %s\n", strings.ToUpper(rep.Grain))
	for _, s := range rep.Series {
		name := "  " + s.Job
		if s.Job == "" {
			name = path.Base(s.Workflow) + " (workflow)"
		}
		a := s.All
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, a.Runs, secs(a.Sec.P50), secs(a.Sec.P90), pctSigned(watch.Growth(s)),
			gib(int64(a.MemBytes.P90)), secs(a.CPUSec.P50), secs(a.WaitSec.P90), spark(s))
	}
	tw.Flush()
}

func printDays(w io.Writer, s watch.Series) {
	fmt.Fprintln(w, s.Name())
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  FROM\tRUNS\tOK\tTIME p50\tp90\tMEM p50\tp90\tCPU p50\tp90\tWAIT p50\tp90\tCONTENDED\tCPU-HOURS")
	for _, b := range s.Buckets {
		if b.Runs == 0 {
			fmt.Fprintf(tw, "  %s\t0\t\t\t\t\t\t\t\t\t\t\t\n", b.Start.Format("Mon Jan 2"))
			continue
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%.2f\n", b.Start.Format("Mon Jan 2"), b.Runs, b.Succeeded,
			secs(b.Sec.P50), secs(b.Sec.P90), gib(int64(b.MemBytes.P50)), gib(int64(b.MemBytes.P90)), secs(b.CPUSec.P50), secs(b.CPUSec.P90),
			secs(b.WaitSec.P50), secs(b.WaitSec.P90), b.Contended, b.CPUHours)
	}
	tw.Flush()
	fmt.Fprintln(w)
}

// groupChanges puts together the changes of one job that one run begins
// (more work is more time and more cpu): they are one line.
func groupChanges(cs []watch.Change) [][]watch.Change {
	var out [][]watch.Change
	idx := map[string]int{}
	for _, c := range cs {
		k := c.Repo + "\x00" + c.Workflow + "\x00" + c.Job + "\x00" + c.First.Runner
		if i, ok := idx[k]; ok {
			out[i] = append(out[i], c)
			continue
		}
		idx[k] = len(out)
		out = append(out, []watch.Change{c})
	}
	return out
}

// changeLine: "ci.yml / unit-tests: time +38% (4m33s -> 6m17s), cpu +37% (…)
// since commit 349f789 on main, Thu Nov 19 10:47; still so".
func changeLine(g []watch.Change) string {
	c := g[0]
	var what []string
	for _, m := range g {
		what = append(what, fmt.Sprintf("%s %s (%s -> %s)", m.Metric, pctSigned(m.Ratio), metricText(m.Metric, m.Before), metricText(m.Metric, m.After)))
	}
	line := fmt.Sprintf("%s / %s: %s", path.Base(c.Workflow), c.Job, strings.Join(what, ", "))
	if c.Gradual {
		line += fmt.Sprintf(", crept over the days around %s (no single commit)", c.First.At.Local().Format("Mon Jan 2"))
	} else {
		line += ", since " + runName(c.First)
		if c.PR != 0 && c.PR != c.First.PR {
			line += fmt.Sprintf(" (pull request #%d ran at the new level before it)", c.PR)
		}
	}
	if c.Holds {
		return line + "; still so"
	}
	return line + "; now " + metricText(c.Metric, c.Now)
}

func metricText(metric string, v float64) string {
	if metric == "memory" {
		return gib(int64(v))
	}
	return secs(v)
}

// runName: "commit 9f8e7d6 on main, Thu Nov 19 11:40" or "pull request #12 (9f8e7d6), …".
func runName(r watch.RunRef) string {
	sha := r.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	at := r.At.Local().Format("Mon Jan 2 15:04")
	switch {
	case r.PR != 0 && sha != "":
		return fmt.Sprintf("pull request #%d (%s), %s", r.PR, sha, at)
	case r.PR != 0:
		return fmt.Sprintf("pull request #%d, %s", r.PR, at)
	case sha != "" && r.Branch != "":
		return fmt.Sprintf("commit %s on %s, %s", sha, r.Branch, at)
	case sha != "":
		return fmt.Sprintf("commit %s, %s", sha, at)
	}
	return fmt.Sprintf("run %d, %s", r.RunID, at)
}

// changeDetail: the steps that grew, the commits that could not be told
// apart, and where to look.
func changeDetail(c watch.Change, web string) string {
	var parts []string
	if c.Metric == "time" && len(c.Steps) > 0 {
		var grew []string
		total := c.After - c.Before
		for _, s := range c.Steps {
			d := s.After - s.Before
			if (total > 0 && d > 0.2*total) || (total < 0 && d < 0.2*total) {
				grew = append(grew, fmt.Sprintf("step %q %s -> %s", s.Name, secs(s.Before), secs(s.After)))
			}
		}
		if len(grew) > 0 {
			parts = append(parts, strings.Join(grew, ", "))
		}
	}
	if n := len(c.Between); n > 0 && !c.Gradual {
		var shas []string
		for _, b := range c.Between {
			shas = append(shas, short(b.SHA))
		}
		parts = append(parts, fmt.Sprintf("or %s just before it (not counted: busy host or failed)", strings.Join(shas, ", ")))
	}
	if c.First.SHA != "" && !c.Gradual {
		parts = append(parts, fmt.Sprintf("%s/%s/commit/%s", web, c.Repo, c.First.SHA))
	}
	return strings.Join(parts, "; ")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func secs(s float64) string {
	switch {
	case s <= 0:
		return "-"
	case s < 60:
		return fmt.Sprintf("%.0fs", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
	}
	return fmt.Sprintf("%dh%02dm", int(s)/3600, int(s)%3600/60)
}

func pctSigned(f float64) string {
	if f == 0 || math.IsNaN(f) {
		return "-"
	}
	return fmt.Sprintf("%+.0f%%", 100*f)
}

// spark draws the series' time p50 of each bucket, low to high; a gap is a
// bucket without a run that succeeded.
func spark(s watch.Series) string {
	bars := []rune("▁▂▃▄▅▆▇█")
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, b := range s.Buckets {
		if b.Succeeded > 0 {
			lo, hi = min(lo, b.Sec.P50), max(hi, b.Sec.P50)
		}
	}
	var out []rune
	for _, b := range s.Buckets {
		switch {
		case b.Succeeded == 0:
			out = append(out, ' ')
		case hi-lo < 1e-9 || hi < lo*1.05:
			out = append(out, bars[1]) // within 5%: flat
		default:
			out = append(out, bars[int(math.Round((b.Sec.P50-lo)/(hi-lo)*float64(len(bars)-1)))])
		}
	}
	return string(out)
}
