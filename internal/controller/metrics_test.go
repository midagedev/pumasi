package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/midagedev/pumasi/internal/decide"
	"github.com/midagedev/pumasi/internal/ledger"
	"github.com/midagedev/pumasi/internal/size"
)

// /metrics carries, after the hosts' families, what only the controller
// knows: runners by state, finished jobs by result and how long they ran.
// It ends with the one "# EOF" OpenMetrics allows.
func TestMetricsHaveTheControllersFamilies(t *testing.T) {
	r := newRig(t)
	k := ledger.KeyOf("o/r", "o/r/.github/workflows/ci.yml", "build")
	for _, n := range []string{"small-h1-a", "small-h1-b"} {
		if err := r.ledger.InsertRunner(n, "small", "linux/docker", "h1", *r.now, size.Estimate{}); err != nil {
			t.Fatal(err)
		}
		r.finished(n, k, 0.5, gbs, 0)
	}
	r.runner("small-h1-live")
	r.ag.Sample(context.Background())
	r.refreshHosts(context.Background(), *r.now)
	r.setStatus(decide.Snapshot{}, decide.Plan{}, nil)
	var b strings.Builder
	if err := r.writeMetrics(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"pumasi_host_cpus{",
		`pumasi_jobs_total{class="small",result="succeeded"} 2`,
		`pumasi_job_seconds_total{class="small",result="succeeded"} 200`,
		`pumasi_runners{class="small",host="h1",state="`,
		"pumasi_incidents_open{} 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "# EOF") != 1 || !strings.HasSuffix(out, "# EOF\n") {
		t.Errorf("want exactly one # EOF, at the end")
	}
}
