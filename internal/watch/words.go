package watch

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"time"
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
	// Time first: its steps are the detail a group shows.
	rank := map[string]int{"time": 0, "memory": 1, "cpu": 2}
	for _, g := range out {
		slices.SortStableFunc(g, func(a, b Change) int { return rank[a.Metric] - rank[b.Metric] })
	}
	return out
}

// ChangeLine: "ci.yml / unit-tests: time +38% (4m33s -> 6m17s), cpu +37% (…)
// since commit 349f789 on main, Thu Nov 19 10:47; still so".
func ChangeLine(g []Change) string {
	c := g[0]
	var what []string
	for _, m := range g {
		what = append(what, changeText(m))
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

// changeText: "memory -60% (2.0G -> 0.8G, in 2 steps)".
func changeText(m Change) string {
	in := ""
	if m.Parts > 1 {
		in = fmt.Sprintf(", in %d steps", m.Parts)
	}
	return fmt.Sprintf("%s %s (%s -> %s%s)", m.Metric, PctSigned(m.Ratio), MetricText(m.Metric, m.Before), MetricText(m.Metric, m.After), in)
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

// ShiftLine: "server2: 6 jobs 30–60% faster since Thu Nov 19 10:05 — the
// machine, not the code", or for a job that moved, "ci.yml / e2e: time -40%
// (10m00s -> 6m00s) when it moved from server1/small to server1/large, …".
func ShiftLine(s Shift) string {
	at := func(t time.Time) string { return t.Local().Format("Mon Jan 2 15:04") }
	way := "faster"
	if s.Slower {
		way = "slower"
		if c := s.Changes[0]; c.Metric == "memory" && len(unitsOf(s.Changes)[0]) == 1 {
			way = "bigger"
		}
	} else if c := s.Changes[0]; c.Metric == "memory" && len(unitsOf(s.Changes)[0]) == 1 {
		way = "smaller"
	}
	if s.Kind == "moved" {
		us := unitsOf(s.Changes)
		if len(us) == 1 {
			c := us[0].lead()
			var what []string
			for _, m := range us[0] {
				what = append(what, changeText(m))
			}
			return fmt.Sprintf("%s: %s when it moved from %s to %s, %s — another kind of machine, not the code",
				JobName(c.Workflow, c.Job), strings.Join(what, ", "), strings.Join(c.Moved.From, ", "), strings.Join(c.Moved.To, ", "), at(c.First.At))
		}
		var from []string
		last := s.At
		for _, u := range us {
			c := u.lead()
			for _, m := range c.Moved.From {
				if !slices.Contains(from, m) {
					from = append(from, m)
				}
			}
			if c.First.At.After(last) {
				last = c.First.At
			}
		}
		return fmt.Sprintf("%d jobs %s %s when they moved from %s to %s, %s to %s — another kind of machine, not the code",
			s.Jobs, rangeText(s), way, strings.Join(from, ", "), strings.Join(us[0].lead().Moved.To, ", "), at(s.At), last.Local().Format("15:04"))
	}
	hosts := strings.Join(s.Hosts, ", ")
	if hosts == "" {
		hosts = "every host"
	}
	size := rangeText(s)
	who := fmt.Sprintf("%d jobs", s.Jobs)
	if s.Jobs == 1 {
		c := s.Changes[0]
		who = JobName(c.Workflow, c.Job)
	}
	line := fmt.Sprintf("%s: %s %s %s", hosts, who, size, way)
	switch {
	case s.Until.IsZero():
		line += " since " + at(s.At)
	case s.Back:
		line += fmt.Sprintf(" from %s to %s, back since", at(s.At), at(s.Until))
	default:
		line += fmt.Sprintf(" from %s, the other way since %s", at(s.At), at(s.Until))
	}
	if s.Jobs == 1 && s.Changes[0].OneHost {
		line += ", on that host's runs only"
	}
	return line + " — the machine, not the code"
}

// ShiftJobs: the jobs of a host's shift and each one's step, the biggest
// first: "unit-tests -63%, lint -60%".
func ShiftJobs(s Shift) []string {
	type js struct {
		name string
		r    float64
	}
	var out []js
	seen := map[string]bool{}
	for _, u := range unitsOf(s.Changes) {
		c := u.lead()
		if seen[u.job()] {
			continue
		}
		seen[u.job()] = true
		r := c.Ratio
		if c.OneHost {
			r = c.OnHost
		}
		out = append(out, js{JobName(c.Workflow, c.Job), r})
	}
	slices.SortStableFunc(out, func(a, b js) int { return cmpFloat(math.Abs(b.r), math.Abs(a.r)) })
	var names []string
	for _, j := range out {
		names = append(names, j.name+" "+PctSigned(j.r))
	}
	return names
}

// rangeText: a shift's jobs' steps, "30–60%".
func rangeText(s Shift) string {
	lo, hi := math.Abs(s.Low), math.Abs(s.High)
	lo, hi = math.Min(lo, hi), math.Max(lo, hi)
	if fmt.Sprintf("%.0f", 100*lo) == fmt.Sprintf("%.0f", 100*hi) {
		return fmt.Sprintf("%.0f%%", 100*hi)
	}
	return fmt.Sprintf("%.0f–%.0f%%", 100*lo, 100*hi)
}
