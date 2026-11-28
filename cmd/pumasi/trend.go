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
	"unicode/utf8"

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
		now = synth.DemoEnd(now) // where the demo's history ends today
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
MACHINES lists steps that were a host's, not a job's: several jobs on a host
stepping together, a step on one host's runs only, a job that moved to
another kind of machine (host and runner class).
CHANGED lists steps up or down that held across runs (not single slow runs),
found among runs whose host was not saturated by others, once the runs on
each side span a day (--min-side), with the first run after the step: its
commit, its pull request, and the job's steps that grew; then jobs slowly
growing with no step to show for it (looked for over the last 28 days at
least). CHANGE is a job's listed time steps and creep together (marked *), or
for a job with none its last third of days against its first. The table fits
the terminal (100 columns when not one); --json keeps full names.

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
	minSide := fs.String("min-side", "", "list a job's change once the runs on each side of it span this long (default "+watch.Span(watch.MinSide)+"; 0 = at once)")
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
	from := watch.TrendFrom(now, span)
	runs, now, err := src.runs(from.Add(-watch.Baseline))
	if err != nil {
		return err
	}
	var o watch.Options
	if *minSide != "" {
		d, err := config.ParseSpan(*minSide)
		if err != nil || d < 0 {
			return fmt.Errorf("--min-side: %q is not a span (1d, 12h, 0)", *minSide)
		}
		if d == 0 {
			d = -1 // at once
		}
		o.MinBefore, o.MinAfter = d, d
	}
	rep := watch.BuildTrend(runs, watch.TrendQuery{From: from, To: now, Week: *week, Loc: time.Local, Filter: f}, o)
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
	printTrend(os.Stdout, rep, src.web, *days, tableWidth())
	return nil
}

func printTrend(w io.Writer, rep watch.TrendReport, web string, days bool, width int) {
	fmt.Fprintf(w, "%s to %s, a row a %s\n", rep.From.Format("Mon Jan 2"), rep.To.Format("Mon Jan 2 15:04"), rep.Grain)
	if rep.Notice != "" {
		fmt.Fprintln(w, rep.Notice)
	}
	fmt.Fprintln(w)
	if len(rep.Shifts) > 0 {
		fmt.Fprintln(w, "MACHINES (steps of hosts, not of jobs; newest first)")
		for _, s := range rep.Shifts {
			fmt.Fprintln(w, "  "+watch.ShiftLine(s))
			if s.Jobs > 1 {
				fmt.Fprintln(w, "      "+fitList(watch.ShiftJobs(s), width-6))
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "CHANGED (newest first)")
	if len(rep.Changes) == 0 && len(rep.Creeping) == 0 {
		if rep.Notice != "" {
			fmt.Fprintln(w, "  nothing yet: too little history to tell a step from a few hours of a host")
		} else {
			fmt.Fprintln(w, "  nothing: no job's time, memory or cpu stepped up or down and stayed there, or crept up")
		}
	}
	for _, g := range watch.GroupChanges(rep.Changes) {
		fmt.Fprintln(w, "  "+watch.ChangeLine(g))
		if more := watch.ChangeDetail(g[0], web); more != "" {
			fmt.Fprintln(w, "      "+more)
		}
	}
	for _, c := range rep.Creeping {
		fmt.Fprintf(w, "  %s / %s: slowly growing, time %s over %s along its trend (%s -> %s, at least %s), no single commit\n",
			path.Base(c.Workflow), c.Job, pctSigned(c.Ratio), fmt.Sprintf("%.0fd", c.Span.Hours()/24), secs(c.From), secs(c.To), pctSigned(c.Low))
	}
	fmt.Fprintln(w)
	if days {
		for _, s := range rep.Series {
			printDays(w, s)
		}
		return
	}
	printTable(w, rep, width)
	fmt.Fprintln(w, "\nCHANGE: a job's time steps and creep listed above, together (marked *); for one with none, the median of the span's last third of days against its first.\n"+
		"Creep is looked for over the last 28 days at least: it is too slow to tell in less.")
}

func changeCell(s watch.Series) string {
	if s.ChangeFrom == "listed" {
		return pctSigned(s.Change) + "*"
	}
	return pctSigned(s.Change)
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

func secs(s float64) string {
	if s <= 0 {
		return "-"
	}
	return watch.Secs(s)
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

// printTable is a line a job, fitted to width: long job names are shortened
// (shortNames), the other columns kept whole.
func printTable(w io.Writer, rep watch.TrendReport, width int) {
	head := []string{"JOB", "RUNS", "TIME p50", "p90", "CHANGE", "MEM p90", "CPU p50", "WAIT p90", "TIME p50 BY " + strings.ToUpper(rep.Grain)}
	rows := [][]string{head}
	var labels []string
	for _, s := range rep.Series {
		if s.Job != "" {
			labels = append(labels, s.Job)
		}
		a := s.All
		rows = append(rows, []string{s.Job, fmt.Sprint(a.Runs), secs(a.Sec.P50), secs(a.Sec.P90), changeCell(s),
			gib(int64(a.MemBytes.P90)), secs(a.CPUSec.P50), secs(a.WaitSec.P90), spark(s)})
	}
	// What the columns after the name take, each with the two spaces
	// before it.
	rest := 0
	for c := 1; c < len(head); c++ {
		cw := 0
		for _, r := range rows {
			cw = max(cw, utf8.RuneCountInString(r[c]))
		}
		rest += 2 + cw
	}
	room := max(width-rest, 16)
	short := shortNames(labels, room-2)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, r := range rows {
		if i > 0 {
			if s := rep.Series[i-1]; s.Job != "" {
				r[0] = "  " + short[r[0]]
			} else {
				r[0] = cutMiddle(path.Base(s.Workflow), room-len(" (workflow)")) + " (workflow)"
			}
		}
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// fitList joins items with ", " up to width, and says how many more.
func fitList(items []string, width int) string {
	out := ""
	for i, it := range items {
		next := it
		if i > 0 {
			next = out + ", " + it
		}
		more := ""
		if i+1 < len(items) {
			more = fmt.Sprintf(", and %d more", len(items)-i-1)
		}
		if i > 0 && utf8.RuneCountInString(next+more) > width {
			return out + fmt.Sprintf(", and %d more", len(items)-i)
		}
		out = next
	}
	return out
}
