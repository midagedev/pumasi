package config

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// PriorityRule gives the jobs it matches a place in line. It only matters
// while room is short: with room for everything, everything starts.
type PriorityRule struct {
	// Match: every key that is set must match. class and event are exact;
	// workflow (the file's name: deploy-*.yml), job (the name GitHub shows),
	// repo (owner/name), ref (refs/heads/main) and branch (main) are globs.
	Match RuleMatch `yaml:"match" json:"match"`
	// Level: high, normal or low.
	Level string `yaml:"level" json:"level"`
}

// RuleMatch is what a rule picks jobs by; every key that is set must match.
type RuleMatch struct {
	Class    string `yaml:"class" json:"class,omitempty"`
	Repo     string `yaml:"repo" json:"repo,omitempty"`
	Workflow string `yaml:"workflow" json:"workflow,omitempty"`
	Job      string `yaml:"job" json:"job,omitempty"`
	Ref      string `yaml:"ref" json:"ref,omitempty"`
	Branch   string `yaml:"branch" json:"branch,omitempty"`
	Event    string `yaml:"event" json:"event,omitempty"`
}

// DefaultHeadStart is how much earlier a high job counts as having come (and
// a low one later).
const DefaultHeadStart = 30 * time.Minute

// WaitingJob is what a rule sees of a job.
type WaitingJob struct {
	Class, Repo, WorkflowRef, Name, Event string
}

func (r PriorityRule) level() int {
	switch r.Level {
	case "high":
		return 1
	case "low":
		return -1
	}
	return 0
}

func (m RuleMatch) keys() [][2]string {
	return [][2]string{{"class", m.Class}, {"repo", m.Repo}, {"workflow", m.Workflow}, {"job", m.Job},
		{"ref", m.Ref}, {"branch", m.Branch}, {"event", m.Event}}
}

// String is the rule as its match reads in the config.
func (r PriorityRule) String() string {
	var parts []string
	for _, kv := range r.Match.keys() {
		if kv[1] != "" {
			parts = append(parts, kv[0]+": "+kv[1])
		}
	}
	return strings.Join(parts, ", ")
}

// YAML is the rule as a line under priority: in the config file.
func (r PriorityRule) YAML() string {
	var parts []string
	for _, kv := range r.Match.keys() {
		if kv[1] != "" {
			parts = append(parts, kv[0]+": "+strconv.Quote(kv[1]))
		}
	}
	return "- {match: {" + strings.Join(parts, ", ") + "}, level: " + r.Level + "}"
}

func (r PriorityRule) matches(j WaitingJob) bool {
	file, ref, _ := strings.Cut(j.WorkflowRef, "@")
	glob := func(pattern, s string) bool {
		ok, _ := path.Match(pattern, s)
		return pattern == "" || ok
	}
	m := r.Match
	return (m.Class == "" || m.Class == j.Class) && (m.Event == "" || strings.EqualFold(m.Event, j.Event)) &&
		glob(m.Repo, j.Repo) && glob(m.Workflow, path.Base(file)) && glob(m.Job, j.Name) &&
		glob(m.Ref, ref) && glob(m.Branch, strings.TrimPrefix(ref, "refs/heads/")) &&
		(m.Branch == "" || strings.HasPrefix(ref, "refs/heads/"))
}

// Validate says what is wrong with a rule (nil = nothing).
func (r PriorityRule) Validate() error {
	if r.Level != "high" && r.Level != "normal" && r.Level != "low" {
		return fmt.Errorf("level must be high, normal or low")
	}
	if r.String() == "" {
		return fmt.Errorf("match needs at least one of class, repo, workflow, job, ref, branch, event")
	}
	for _, p := range []string{r.Match.Repo, r.Match.Workflow, r.Match.Job, r.Match.Ref, r.Match.Branch} {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("bad pattern %q", p)
		}
	}
	return nil
}

// LevelOf is a job's level (1 high, 0 normal, -1 low) by the first rule of
// the config file that matches it, and that rule in words ("" = none matched).
func (c *Config) LevelOf(j WaitingJob) (level int, why string) {
	return LevelIn(c.Rules(nil), j)
}

// Rule is a priority rule with where it comes from: the config file, or
// added while `pumasi up` runs (a runtime rule, kept in the ledger).
type Rule struct {
	PriorityRule
	ID      string    // "file-3" (the third of the file), "r-12" (runtime)
	Runtime bool      // added while `pumasi up` runs
	By      string    // who added a runtime rule
	At      time.Time // when
	Expires time.Time // zero = until removed (runtime rules only)
}

// Rules is every rule in force, in the order they are tried: the runtime
// ones before the file's. A rule added at runtime is a decision of the moment
// that the file did not foresee.
func (c *Config) Rules(runtime []Rule) []Rule {
	out := append([]Rule{}, runtime...)
	for i, r := range c.Priority {
		out = append(out, Rule{PriorityRule: r, ID: fmt.Sprintf("file-%d", i+1)})
	}
	return out
}

// LevelIn is a job's level by the first of rules that matches it, and that
// rule in words ("" = none matched).
func LevelIn(rules []Rule, j WaitingJob) (level int, why string) {
	for _, r := range rules {
		if r.matches(j) {
			if r.Runtime {
				return r.level(), fmt.Sprintf("%s by runtime rule %s (%s)", r.Level, r.ID, r.PriorityRule)
			}
			return r.level(), fmt.Sprintf("%s by rule %s (%s)", r.Level, strings.TrimPrefix(r.ID, "file-"), r.PriorityRule)
		}
	}
	return 0, ""
}

// MayBeHigh says whether some rule can make a job of the class high.
func MayBeHigh(rules []Rule, class string) bool {
	for _, r := range rules {
		if r.Level == "high" && (r.Match.Class == "" || r.Match.Class == class) {
			return true
		}
	}
	return false
}
