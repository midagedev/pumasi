package config

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// PriorityRule gives the jobs it matches a place in line. It only matters
// while room is short: with room for everything, everything starts.
type PriorityRule struct {
	// Match: every key that is set must match. class and event are exact;
	// workflow (the file's name: deploy-*.yml), job (the name GitHub shows),
	// repo (owner/name), ref (refs/heads/main) and branch (main) are globs.
	Match struct {
		Class    string `yaml:"class"`
		Repo     string `yaml:"repo"`
		Workflow string `yaml:"workflow"`
		Job      string `yaml:"job"`
		Ref      string `yaml:"ref"`
		Branch   string `yaml:"branch"`
		Event    string `yaml:"event"`
	} `yaml:"match"`
	// Level: high, normal or low.
	Level string `yaml:"level"`
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

// String is the rule as its match reads in the config.
func (r PriorityRule) String() string {
	var parts []string
	for _, kv := range [][2]string{{"class", r.Match.Class}, {"repo", r.Match.Repo}, {"workflow", r.Match.Workflow}, {"job", r.Match.Job},
		{"ref", r.Match.Ref}, {"branch", r.Match.Branch}, {"event", r.Match.Event}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+": "+kv[1])
		}
	}
	return strings.Join(parts, ", ")
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

func (r PriorityRule) validate() error {
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

// LevelOf is a job's level (1 high, 0 normal, -1 low) by the first rule that
// matches it, and that rule in words ("" = no rule matched).
func (c *Config) LevelOf(j WaitingJob) (level int, why string) {
	for i, r := range c.Priority {
		if r.matches(j) {
			return r.level(), fmt.Sprintf("%s by rule %d (%s)", r.Level, i+1, r)
		}
	}
	return 0, ""
}
