//go:build !darwin && !windows

package agent

import (
	"os"
	"strings"

	"github.com/midagedev/pumasi/internal/cgroup"
)

// Platform is how this OS runs and measures runners: docker containers and
// their cgroups. dir is unused here.
func Platform(tag, dir string) (Driver, Meter) {
	return Docker{Tag: tag}, &CgroupMeter{R: cgroup.DefaultReader()}
}

// BootID changes when the host has rebooted.
func BootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}
