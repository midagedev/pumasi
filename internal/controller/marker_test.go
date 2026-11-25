package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/pumasi/internal/api"
	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/ledger"
)

// GitHub says the runner has a job; the agent has not read "Running job" from
// the runner's output. Said once after a minute, and once more when it shows.
func TestMarkerLateIsSaidOnce(t *testing.T) {
	var out bytes.Buffer
	c := &Controller{log: slog.New(slog.NewTextHandler(&out, nil))}
	h := &host{cfg: config.Host{Name: "server1"}, snap: &api.Snapshot{SampledAt: time.Now().Add(-3 * time.Second)}}
	ar := api.Runner{Name: "linux-s-server1-aa", State: "running"}
	lr := ledger.Runner{Name: ar.Name, JobID: "j1", Job: "build", StartedAt: time.Now().Add(-30 * time.Second)}

	c.markerCheck(h, lr, ar) // 30s: the line may still be on its way
	c.markerCheck(h, ledger.Runner{Name: ar.Name}, ar)
	if out.Len() != 0 {
		t.Fatalf("too early or no job: %s", out.String())
	}
	lr.StartedAt = time.Now().Add(-90 * time.Second)
	c.markerCheck(h, lr, ar)
	c.markerCheck(h, lr, ar)
	if n := strings.Count(out.String(), "has not seen"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, out.String())
	}
	for _, want := range []string{"linux-s-server1-aa", "build", "server1", "agent sampled 3s ago"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	now := time.Now()
	ar.JobStartedAt = &now
	c.markerCheck(h, lr, ar)
	c.markerCheck(h, lr, ar)
	if n := strings.Count(out.String(), "now sees the job"); n != 1 {
		t.Fatalf("recovery said %d times:\n%s", n, out.String())
	}
}
