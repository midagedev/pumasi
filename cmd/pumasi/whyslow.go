package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/midagedev/pumasi/internal/watch"
)

const whyUsage = `pumasi why-slow: did this run, pull request or commit make CI slower, and where?

  pumasi why-slow https://github.com/o/r/actions/runs/123   a run (its URL or number)
  pumasi why-slow https://github.com/o/r/pull/45            a pull request (or --pr 45, or pr/45)
  pumasi why-slow 4f2a1b3                                   a commit (or --commit 4f2a1b3)
  pumasi why-slow --json --pr 45                            the same as JSON (also GET /api/why-slow?pr=45)

Each job is set against its last 20 runs on the main branch before it that
succeeded on a host not saturated by others, as on the host it ran on. A job
is "slower" when it is 10% or more over their median and far outside their
usual spread (z >= 3) over two runs or more; "maybe slower" when it is less
far out (z >= 2) or ran once (one run can be a cold cache). When other jobs
on its host were slow at the same time, its time is divided by how slow
(shown as "slow host"). The steps that grew are named.

When main changed in a job at this commit, or right after this pull request
was merged with its runs already at the new level, that change is shown
first: it is what the target did to main. When main changed in a job during
the baseline, only main's runs since that change are the job's usual, and the
change is listed after.

Flags:
`

func runWhySlow(args []string) error {
	fs := flag.NewFlagSet("pumasi why-slow", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), whyUsage); fs.PrintDefaults() }
	var src source
	src.flags(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	var t watch.Target
	fs.IntVar(&t.PR, "pr", 0, "a pull request's number")
	fs.Int64Var(&t.RunID, "run", 0, "a workflow run's number (in its URL: /actions/runs/<number>)")
	fs.StringVar(&t.SHA, "commit", "", "a commit's hash (7 characters or more)")
	pos, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && t == (watch.Target{}):
		if t, err = watch.ParseTarget(pos[0]); err != nil {
			return err
		}
	case len(pos) == 0 && t != (watch.Target{}):
		t.SHA = strings.ToLower(t.SHA)
	default:
		fs.Usage()
		return flag.ErrHelp
	}
	runs, now, err := src.runs(time.Now().Add(-60 * 24 * time.Hour))
	if err != nil {
		return err
	}
	rep, err := watch.WhySlowAt(runs, t, now)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printWhy(os.Stdout, rep, src.web)
	return nil
}

func printWhy(w io.Writer, rep watch.WhyReport, web string) {
	trunk := cmpOr(rep.Branch, "main")
	fmt.Fprintf(w, "%s\nagainst the last %d runs of each job on %s before it\n\n", rep.What, rep.Baseline, trunk)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tRUNS\tTIME\tUSUAL\tp10-p90\tCHANGE\tVERDICT\tMEM\tCPU")
	uneven := false
	row := func(name string, j watch.JobWhy) {
		v := j.Time.Verdict
		if v == "usual" && math.Abs(j.Time.Ratio) >= 0.10 {
			v, uneven = "uneven", true
		}
		if j.Failed {
			v += " (failed)"
		}
		if j.Contended {
			v += " (busy host)"
		}
		if j.SlowHost > 0 {
			v += fmt.Sprintf(" (slow host x%.1f)", j.SlowHost)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s-%s\t%s\t%s\t%s\t%s\n", name, j.Time.N, secs(j.Time.This), secs(j.Time.Usual.P50), secs(j.Time.Usual.P10), secs(j.Time.Usual.P90),
			watch.Pct(j.Time.Ratio), v, change(j.Memory), change(j.CPU))
	}
	for _, j := range rep.Jobs {
		row(j.Job, j)
	}
	if rep.Total != nil {
		row("(whole run)", *rep.Total)
	}
	tw.Flush()
	if uneven {
		fmt.Fprint(w, "\nuneven: the median is 10% or more off its usual, but not clearly (a run of it was within the usual spread, or too few ran to tell), so it is not called slower or faster")
	}
	for _, j := range rep.Jobs {
		if !j.BaselineSince.IsZero() {
			fmt.Fprintf(w, "\n%s: main changed in it on %s, so its usual is main's %d runs since", j.Job, j.BaselineSince.Local().Format("Mon Jan 2 15:04"), j.Time.Usual.Runs)
		}
	}
	for _, j := range rep.Jobs {
		if !strings.Contains(j.Time.Verdict, "slower") && !strings.Contains(j.Time.Verdict, "faster") {
			continue
		}
		var grew []string
		d := j.Time.This - j.Time.Usual.P50
		for _, s := range j.Steps {
			if (d > 0 && s.Delta > 0.2*d) || (d < 0 && s.Delta < 0.2*d) {
				grew = append(grew, fmt.Sprintf("%q %s -> %s (%s)", s.Name, secs(s.Usual), secs(s.This), signedSecs(s.Delta)))
			}
		}
		if len(grew) > 0 {
			fmt.Fprintf(w, "\n%s is %s: %s", j.Job, j.Time.Verdict, strings.Join(grew, ", "))
		}
	}
	fmt.Fprintln(w)
	if len(rep.Own) > 0 {
		if rep.Target.PR != 0 {
			fmt.Fprintf(w, "\n%s changed right after #%d was merged, and its runs were already at the new level:\n", trunk, rep.Target.PR)
		} else {
			fmt.Fprintf(w, "\n%s changed at this commit:\n", trunk)
		}
		sure := map[string]bool{}
		for i, c := range rep.Own {
			sure[c.Workflow+"\x00"+c.Job+"\x00"+c.Metric] = rep.OwnSure[i]
		}
		for _, g := range watch.GroupChanges(rep.Own) {
			line := watch.ChangeLine(g)
			if c := g[0]; !sure[c.Workflow+"\x00"+c.Job+"\x00"+c.Metric] {
				line += fmt.Sprintf("  (one of %d pull requests that fit: %s)", len(c.Suspects), prList(c.Suspects))
			}
			fmt.Fprintln(w, "  "+line)
			if more := watch.ChangeDetail(g[0], web); more != "" {
				fmt.Fprintln(w, "      "+more)
			}
		}
	}
	if len(rep.Changes) > 0 {
		fmt.Fprintf(w, "\n%s itself changed in these jobs during the baseline (it mixes before and after):\n", trunk)
		for _, g := range watch.GroupChanges(rep.Changes) {
			fmt.Fprintln(w, "  "+watch.ChangeLine(g))
			if more := watch.ChangeDetail(g[0], web); more != "" {
				fmt.Fprintln(w, "      "+more)
			}
		}
	}
}

func prList(prs []int) string {
	var s []string
	for _, p := range prs {
		s = append(s, fmt.Sprintf("#%d", p))
	}
	return strings.Join(s, ", ")
}

func change(m watch.Measure) string {
	if m.Usual.P50 <= 0 || m.This <= 0 {
		return "-"
	}
	s := watch.Pct(m.Ratio)
	if m.Verdict != "usual" && m.Verdict != "unknown" {
		s += " " + m.Verdict
	}
	return s
}

func signedSecs(d float64) string {
	if d < 0 {
		return "-" + secs(-d)
	}
	return "+" + secs(d)
}
