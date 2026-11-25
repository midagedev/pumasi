package agent

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/midagedev/pumasi/internal/api"
)

// WriteMetrics renders a snapshot in the OpenMetrics text format.
func WriteMetrics(w io.Writer, s api.Snapshot) {
	host := label("host", s.Host)
	family := func(name, typ, help string) {
		fmt.Fprintf(w, "# TYPE %s %s\n# HELP %s %s\n", name, typ, name, help)
	}
	sample := func(name, labels string, v float64) {
		fmt.Fprintf(w, "%s{%s} %s\n", name, labels, strconv.FormatFloat(v, 'g', -1, 64))
	}
	rl := func(r api.Runner) string {
		return strings.Join([]string{host, label("class", r.Class), label("runner", r.Name)}, ",")
	}

	family("pumasi_host_cpus", "gauge", "Logical CPUs on the host.")
	sample("pumasi_host_cpus", host, s.CPUs)
	family("pumasi_host_memory_bytes", "gauge", "MemTotal of the host.")
	sample("pumasi_host_memory_bytes", host, float64(s.MemBytes))
	family("pumasi_host_budget_cpus", "gauge", "CPUs pumasi may lend on this host (0 = unset).")
	sample("pumasi_host_budget_cpus", host, s.BudgetCPUs)
	family("pumasi_host_budget_memory_bytes", "gauge", "Memory pumasi may lend on this host (0 = unset).")
	sample("pumasi_host_budget_memory_bytes", host, float64(s.BudgetMem))
	family("pumasi_host_nonrunner_cpus", "gauge", "CPU cores used by everything except pumasi runners.")
	sample("pumasi_host_nonrunner_cpus", host, s.NonRunnerCPU)
	family("pumasi_host_nonrunner_memory_bytes", "gauge", "Memory used by everything except pumasi runners.")
	sample("pumasi_host_nonrunner_memory_bytes", host, float64(s.NonRunnerMem))
	family("pumasi_host_pressure_avg10", "gauge", "Host PSI some avg10, percent.")
	for _, res := range sortedKeys(s.PSI) {
		sample("pumasi_host_pressure_avg10", host+","+label("resource", res), s.PSI[res])
	}

	family("pumasi_runner_running", "gauge", "1 while the runner container runs.")
	for _, r := range s.Runners {
		v := 0.0
		if r.State == "running" {
			v = 1
		}
		sample("pumasi_runner_running", rl(r), v)
	}
	family("pumasi_runner_cpu_seconds", "counter", "CPU time of the runner cgroup.")
	for _, r := range s.Runners {
		sample("pumasi_runner_cpu_seconds_total", rl(r), r.CPUSec)
	}
	family("pumasi_runner_memory_peak_bytes", "gauge", "memory.peak of the runner cgroup.")
	for _, r := range s.Runners {
		sample("pumasi_runner_memory_peak_bytes", rl(r), float64(r.MemPeak))
	}
	family("pumasi_runner_io_bytes", "counter", "Bytes read and written by the runner cgroup.")
	for _, r := range s.Runners {
		sample("pumasi_runner_io_bytes_total", rl(r), float64(r.IOBytes))
	}
	family("pumasi_runner_pressure_seconds", "counter", "PSI some stall time of the runner cgroup.")
	for _, r := range s.Runners {
		for _, res := range sortedKeys(r.PressureSec) {
			sample("pumasi_runner_pressure_seconds_total", rl(r)+","+label("resource", res), r.PressureSec[res])
		}
	}
	family("pumasi_runner_provision_seconds", "gauge", "From start request to the runner listening for jobs.")
	for _, r := range s.Runners {
		if r.ListeningAt != nil {
			sample("pumasi_runner_provision_seconds", rl(r), float64(r.ProvisionMs)/1000)
		}
	}
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
