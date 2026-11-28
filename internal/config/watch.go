package config

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Watch is what pumasi keeps and watches of the jobs it ran: how long they
// take, what they use, and the limits a person set on them. Every limit that
// is crossed is an incident (the problems panel, notify) until it is not.
type Watch struct {
	// KeepSteps: how long the step timings of a finished job are kept
	// (default 90d). The job's own row, with its time, memory and commit,
	// stays as long as the ledger.
	KeepSteps Span `yaml:"keepSteps"`
	// Limits on the jobs and workflows they match.
	Limits []Limit `yaml:"limits"`
	// CPUHoursPerDay: all jobs together may use this many cpu-hours a day
	// (from midnight, this machine's time). 0 = no budget.
	CPUHoursPerDay float64 `yaml:"cpuHoursPerDay"`
	// Slope: a job whose typical time grew more than this in a week (the
	// last 7 days against the 7 before) is a problem. "10%"; 0 = not watched.
	Slope Percent `yaml:"slope"`
	// Regressions: a job that got slower (or bigger) by at least this since
	// a commit is announced, once (default 20%; "off" = never). Smaller
	// steps, down to 10%, are still listed by `pumasi trend`.
	Regressions *Percent `yaml:"regressions"`
}

// Limit is what the jobs or workflows a match picks may take. The time,
// memory and wait of a job are the median of its last 5 runs that
// succeeded: one slow run is not a problem, a job that is now slow is.
type Limit struct {
	// Match: every key that is set must match, as in priority rules
	// (workflow: nightly.yml, job: "e2e*", branch: main, event: schedule, …).
	Match RuleMatch `yaml:"match"`
	// MaxJobTime: each job it matches, from start to end.
	MaxJobTime Span `yaml:"maxJobTime"`
	// MaxRunTime: each workflow it matches, a run from its first job's start
	// to its last job's end (the jobs pumasi ran).
	MaxRunTime Span `yaml:"maxRunTime"`
	// MaxMemory: each job it matches, at its peak (page cache left out).
	MaxMemory Bytes `yaml:"maxMemory"`
	// MaxWait: a job it matches waiting longer than this for a runner, now.
	MaxWait Span `yaml:"maxWait"`
	// FinishBy: "07:00" (this machine's time): a run of the workflow it
	// matches has to have started in the day before and finished by then.
	FinishBy string `yaml:"finishBy"`
}

// String is the limit's match as it reads in the config.
func (l Limit) String() string { return PriorityRule{Match: l.Match}.String() }

// Deadline is FinishBy as hours and minutes (ok false = none).
func (l Limit) Deadline() (h, m int, ok bool) {
	if l.FinishBy == "" {
		return 0, 0, false
	}
	t, err := time.Parse("15:04", l.FinishBy)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func (l Limit) validate() error {
	if l.String() == "" {
		return fmt.Errorf("match needs at least one of class, repo, workflow, job, ref, branch, event")
	}
	for _, p := range []string{l.Match.Repo, l.Match.Workflow, l.Match.Job, l.Match.Ref, l.Match.Branch} {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("bad pattern %q", p)
		}
	}
	if l.FinishBy != "" {
		if _, _, ok := l.Deadline(); !ok {
			return fmt.Errorf("finishBy %q is not a time of day (07:00)", l.FinishBy)
		}
	}
	if l.MaxJobTime == 0 && l.MaxRunTime == 0 && l.MaxMemory == 0 && l.MaxWait == 0 && l.FinishBy == "" {
		return fmt.Errorf("set at least one of maxJobTime, maxRunTime, maxMemory, maxWait, finishBy")
	}
	return nil
}

// DefaultKeepSteps keeps a quarter of step timings: enough to name the step
// that grew in any regression of the last weeks, about 2 KB a job.
const DefaultKeepSteps = 90 * 24 * time.Hour

// DefaultAnnounce: regressions this big or bigger are announced.
const DefaultAnnounce = 0.20

func (w *Watch) defaults() {
	if w.KeepSteps == 0 {
		w.KeepSteps = Span(DefaultKeepSteps)
	}
	if w.Regressions == nil {
		p := Percent(DefaultAnnounce)
		w.Regressions = &p
	}
}

func (w Watch) validate() error {
	if w.CPUHoursPerDay < 0 || w.Slope < 0 {
		return fmt.Errorf("watch: cpuHoursPerDay and slope must not be negative")
	}
	for i, l := range w.Limits {
		if err := l.validate(); err != nil {
			return fmt.Errorf("watch limit %d: %w", i+1, err)
		}
	}
	return nil
}

// Announce is the smallest regression that is announced (0 = none is).
func (w Watch) Announce() float64 {
	if w.Regressions == nil {
		return DefaultAnnounce
	}
	return float64(*w.Regressions)
}

// Percent is a share written as "10%", 0.1, or off.
type Percent float64

func (p *Percent) UnmarshalYAML(n *yaml.Node) error {
	v := strings.TrimSpace(n.Value)
	switch strings.ToLower(v) {
	case "off", "false", "none", "0", "":
		*p = 0
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	if err != nil || f < 0 {
		return fmt.Errorf("bad share %q (10%%, 0.1 or off)", v)
	}
	if strings.HasSuffix(v, "%") {
		f /= 100
	}
	*p = Percent(f)
	return nil
}

// Matches says whether a job is picked by the match (every key that is set).
func (m RuleMatch) Matches(j WaitingJob) bool { return PriorityRule{Match: m}.matches(j) }

// Span is a duration that also reads days and weeks: 90d, 2w, 36h, 15m.
type Span time.Duration

func (s Span) D() time.Duration { return time.Duration(s) }

func (s *Span) UnmarshalYAML(n *yaml.Node) error {
	d, err := ParseSpan(n.Value)
	if err != nil {
		return err
	}
	*s = Span(d)
	return nil
}

// ParseSpan reads 90d, 2w or anything time.ParseDuration reads.
func ParseSpan(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("bad duration %q", v)
			}
			return time.Duration(f * float64(unit)), nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration %q (90d, 2w, 36h, 15m)", v)
	}
	return d, nil
}
