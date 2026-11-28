package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Watch is what pumasi keeps and watches of the jobs it ran: how long they
// take, what they use, and the limits a person set on them.
type Watch struct {
	// KeepSteps: how long the step timings of a finished job are kept
	// (default 90d). The job's own row, with its time, memory and commit,
	// stays as long as the ledger.
	KeepSteps Span `yaml:"keepSteps"`
}

// DefaultKeepSteps keeps a quarter of step timings: enough to name the step
// that grew in any regression of the last weeks, about 2 KB a job.
const DefaultKeepSteps = 90 * 24 * time.Hour

func (w *Watch) defaults() {
	if w.KeepSteps == 0 {
		w.KeepSteps = Span(DefaultKeepSteps)
	}
}

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
