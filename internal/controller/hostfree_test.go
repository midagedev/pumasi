package controller

import (
	"testing"

	"github.com/midagedev/pumasi/internal/config"
	"github.com/midagedev/pumasi/internal/decide"
)

// The state says a host's free cpu as placement counts it: on a server whose
// cores runners share, cpuOvercommit times the cores; on a desktop, once.
func TestHostFreeCountsSharedCores(t *testing.T) {
	cfg := &config.Config{CPUOvercommit: 1.5, PSICPUThreshold: 40}
	h := decide.Host{Kind: "server", CPUs: 32, MemBytes: 128 << 30, NonRunnerCPU: 2, KeepMem: 4 << 30, CPUPressureKnown: true}
	cpu, mem, share := hostFree(cfg, h, 40, 64<<30)
	if share != 1.5 || cpu != 6 || mem != 60<<30 {
		t.Fatalf("server: %v cpu %v mem share %v", cpu, mem, share)
	}
	h.Kind = "desktop"
	if cpu, _, share := hostFree(cfg, h, 20, 64<<30); share != 0 || cpu != 10 {
		t.Fatalf("desktop: %v cpu share %v", cpu, share)
	}
}
