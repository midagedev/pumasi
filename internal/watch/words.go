package watch

import (
	"fmt"
	"math"
	"path"
	"strings"
)

// How changes read, for `pumasi trend`, `pumasi why-slow` and the
// notifications: one line a person reads without the docs.

// GroupChanges puts together the changes of one job that one run begins
// (more work is more time and more cpu): they are one line.
func GroupChanges(cs []Change) [][]Change {
	var out [][]Change
	idx := map[string]int{}
	for _, c := range cs {
		k := c.Repo + "\x00" + c.Workflow + "\x00" + c.Job + "\x00" + c.First.Runner
		if i, ok := idx[k]; ok {
			out[i] = append(out[i], c)
			continue
		}
		idx[k] = len(out)
		out = append(out, []Change{c})
	}
	return out
}

// ChangeLine: "ci.yml / unit-tests: time +38% (4m33s -> 6m17s), cpu +37% (…)
// since commit 349f789 on main, Thu Nov 19 10:47; still so".
func ChangeLine(g []Change) string {
	c := g[0]
	var what []string
	for _, m := range g {
		what = append(what, fmt.Sprintf("%s %s (%s -> %s)", m.Metric, PctSigned(m.Ratio), MetricText(m.Metric, m.Before), MetricText(m.Metric, m.After)))
	}
	line := fmt.Sprintf("%s / %s: %s", path.Base(c.Workflow), c.Job, strings.Join(what, ", "))
	if c.Gradual {
		line += fmt.Sprintf(", crept over the days around %s (no single commit)", c.First.At.Local().Format("Mon Jan 2"))
	} else {
		line += ", since " + RunName(c.First)
		if c.PR != 0 && c.PR != c.First.PR {
			line += fmt.Sprintf(" (pull request #%d ran at the new level before it)", c.PR)
		}
	}
	if c.Holds {
		return line + "; still so"
	}
	return line + "; now " + MetricText(c.Metric, c.Now)
}

func MetricText(metric string, v float64) string {
	if metric == "memory" {
		return GiB(v)
	}
	return Secs(v)
}

// RunName: "commit 9f8e7d6 on main, Thu Nov 19 11:40" or "pull request #12 (9f8e7d6), …".
func RunName(r RunRef) string {
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

// ChangeDetail: the steps that grew, the commits that could not be told
// apart, and where to look.
func ChangeDetail(c Change, web string) string {
	var parts []string
	if c.Metric == "time" && len(c.Steps) > 0 {
		var grew []string
		total := c.After - c.Before
		for _, s := range c.Steps {
			d := s.After - s.Before
			if (total > 0 && d > 0.2*total) || (total < 0 && d < 0.2*total) {
				grew = append(grew, fmt.Sprintf("step %q %s -> %s", s.Name, Secs(s.Before), Secs(s.After)))
			}
		}
		if len(grew) > 0 {
			parts = append(parts, strings.Join(grew, ", "))
		}
	}
	if n := len(c.Between); n > 0 && !c.Gradual {
		var shas []string
		for _, b := range c.Between {
			shas = append(shas, Short(b.SHA))
		}
		parts = append(parts, fmt.Sprintf("or %s just before it (not counted: busy host or failed)", strings.Join(shas, ", ")))
	}
	if c.First.SHA != "" && !c.Gradual {
		parts = append(parts, fmt.Sprintf("%s/%s/commit/%s", web, c.Repo, c.First.SHA))
	}
	return strings.Join(parts, "; ")
}

func Short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// PctSigned is a share as +38% (- for nothing).
func PctSigned(f float64) string {
	if f == 0 || math.IsNaN(f) {
		return "-"
	}
	return fmt.Sprintf("%+.0f%%", 100*f)
}
