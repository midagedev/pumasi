package agent

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/midagedev/pumasi/internal/api"
)

// WriteMetrics renders snapshots (one per host) in the OpenMetrics text format.
func WriteMetrics(w io.Writer, snaps ...api.Snapshot) {
	family := func(name, typ, help string) {
		fmt.Fprintf(w, "# TYPE %s %s\n# HELP %s %s\n", name, typ, name, help)
	}
	sample := func(name, labels string, v float64) {
		fmt.Fprintf(w, "%s{%s} %s\n", name, labels, strconv.FormatFloat(v, 'g', -1, 64))
	}
	hostGauge := func(name, help string, v func(api.Snapshot) float64) {
		family(name, "gauge", help)
		for _, s := range snaps {
			sample(name, label("host", s.Host), v(s))
		}
	}
	// runner emits one family with a sample per runner of every host; ok=false skips a runner.
	runner := func(name, suffix, typ, help string, v func(api.Runner) (float64, bool)) {
		family(name, typ, help)
		for _, s := range snaps {
			for _, r := range s.Runners {
				if x, ok := v(r); ok {
					sample(name+suffix, strings.Join([]string{label("host", s.Host), label("class", r.Class), label("runner", r.Name)}, ","), x)
				}
			}
		}
	}

	hostGauge("pumasi_host_cpus", "Logical CPUs on the host.", func(s api.Snapshot) float64 { return s.CPUs })
	hostGauge("pumasi_host_memory_bytes", "MemTotal of the host.", func(s api.Snapshot) float64 { return float64(s.MemBytes) })
	hostGauge("pumasi_host_budget_cpus", "CPUs pumasi may lend on this host (0 = unset).", func(s api.Snapshot) float64 { return s.BudgetCPUs })
	hostGauge("pumasi_host_budget_memory_bytes", "Memory pumasi may lend on this host (0 = unset).", func(s api.Snapshot) float64 { return float64(s.BudgetMem) })
	hostGauge("pumasi_host_nonrunner_cpus", "CPU cores used by everything except pumasi runners.", func(s api.Snapshot) float64 { return s.NonRunnerCPU })
	hostGauge("pumasi_host_nonrunner_memory_bytes", "Memory used by everything except pumasi runners.", func(s api.Snapshot) float64 { return float64(s.NonRunnerMem) })
	family("pumasi_host_pressure_avg10", "gauge", "Host PSI some avg10, percent.")
	for _, s := range snaps {
		for _, res := range sortedKeys(s.PSI) {
			sample("pumasi_host_pressure_avg10", label("host", s.Host)+","+label("resource", res), s.PSI[res])
		}
	}

	runner("pumasi_runner_running", "", "gauge", "1 while the runner container runs.", func(r api.Runner) (float64, bool) {
		if r.State == "running" {
			return 1, true
		}
		return 0, true
	})
	runner("pumasi_runner_cpu_seconds", "_total", "counter", "CPU time of the runner cgroup.", func(r api.Runner) (float64, bool) { return r.CPUSec, true })
	runner("pumasi_runner_memory_peak_bytes", "", "gauge", "memory.peak of the runner cgroup.", func(r api.Runner) (float64, bool) { return float64(r.MemPeak), true })
	runner("pumasi_runner_io_bytes", "_total", "counter", "Bytes read and written by the runner cgroup.", func(r api.Runner) (float64, bool) {
		if r.IOBytes == nil {
			return 0, false
		}
		return float64(*r.IOBytes), true
	})
	family("pumasi_runner_pressure_seconds", "counter", "PSI some stall time of the runner cgroup.")
	for _, s := range snaps {
		for _, r := range s.Runners {
			for _, res := range sortedKeys(r.PressureSec) {
				sample("pumasi_runner_pressure_seconds_total", strings.Join([]string{label("host", s.Host), label("class", r.Class),
					label("runner", r.Name), label("resource", res)}, ","), r.PressureSec[res])
			}
		}
	}
	runner("pumasi_runner_provision_seconds", "", "gauge", "From start request to the runner listening for jobs.", func(r api.Runner) (float64, bool) {
		return float64(r.ProvisionMs) / 1000, r.ListeningAt != nil
	})
	fmt.Fprintln(w, "# EOF")
}

func label(k, v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return k + `="` + r.Replace(v) + `"`
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
