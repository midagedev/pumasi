package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These run only on a Linux machine whose user has a systemd session with cpu
// and memory delegated: a runner script inside a real scope.

func scopeRunner(t *testing.T, script string) (dir string, log *os.File) {
	t.Helper()
	if p := findNativeProblem(); p != "" {
		t.Skip(p)
	}
	dir = filepath.Join(t.TempDir(), "t"+time.Now().Format("150405.000000"))
	if err := os.MkdirAll(filepath.Join(dir, "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runner", "run.sh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return dir, log
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestScopeTree(t *testing.T) {
	dir, log := scopeRunner(t, "sleep 600 &\ni=0; while [ $i -lt 300000 ]; do i=$((i+1)); done\nsleep 1\nexit 7\n")
	tr, err := startTree(dir, Spec{Name: "t", CPUs: 1.5, MemBytes: 256 << 20}, runnerEnv(os.Environ()), log)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.kill()
	if !tr.alive() || tr.cg == "" {
		t.Fatalf("alive %v, cgroup %q", tr.alive(), tr.cg)
	}
	if got := read(t, filepath.Join(tr.cg, "memory.max")); got != "268435456" {
		t.Errorf("memory.max %s", got)
	}
	if got := read(t, filepath.Join(tr.cg, "cpu.max")); got != "150000 100000" {
		t.Errorf("cpu.max %s", got)
	}
	for i := 0; i < 200 && tr.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if code, ok := tr.exitCode(); !ok || code != 7 {
		t.Fatalf("exit code %d %v, want 7", code, ok)
	}
	// The shell is gone, the sleep it left is still in the scope. A later agent
	// finds the scope through systemd and reads what the whole runner used.
	ad := adoptTree(dir, meta{Pid: tr.pid, Born: tr.born})
	if ad.cg != tr.cg || ad.alive() {
		t.Fatalf("adopted: cgroup %q (want %q), alive %v", ad.cg, tr.cg, ad.alive())
	}
	ad.refresh()
	if u := ad.usage(); u.CPUUsec < 100_000 || u.MemPeak <= 0 || u.MemCurrent <= 0 {
		t.Errorf("adopted: usage %+v, want the loop's cpu time and the sleep's memory", u)
	}
	procs := read(t, filepath.Join(ad.cg, "cgroup.procs"))
	if procs == "" {
		t.Fatal("the scope is empty: the sleep should still be there")
	}
	ad.kill()
	if _, err := os.Stat(ad.cg); err == nil {
		t.Error("the scope's cgroup is still there after kill")
	}
	for _, pid := range strings.Fields(procs) {
		if _, err := os.Stat("/proc/" + pid); err == nil {
			t.Errorf("process %s outlived its runner", pid)
		}
	}
}

// A process of the job that goes over the class's memory is killed by the
// kernel and counted; the runner itself goes on and reports.
func TestScopeMemoryLimit(t *testing.T) {
	dir, log := scopeRunner(t, "head -c 600M /dev/zero | tail > /dev/null\necho after\nsleep 2\nexit 3\n")
	tr, err := startTree(dir, Spec{Name: "t", CPUs: 1, MemBytes: 200 << 20}, runnerEnv(os.Environ()), log)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.kill()
	for i := 0; i < 300 && tr.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
		tr.refresh()
	}
	u := tr.usage()
	code, _ := tr.exitCode()
	t.Logf("exit %d, usage %+v, output: %s", code, u, read(t, filepath.Join(dir, "log")))
	if u.OOMKills == 0 {
		t.Error("no process was killed at the memory limit")
	}
	if u.MemPeak > 200<<20 {
		t.Errorf("peak %d is over the 200M limit", u.MemPeak)
	}
	if systemdVersion() >= 253 && code != 3 {
		t.Errorf("exit code %d, want 3: the runner's shell must outlive the killed process", code)
	}
}

// Without a systemd user session a runner is not started at all, and the
// host says why.
func TestNoUserSessionNoRunner(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	problem.at = time.Time{}
	defer func() { problem.at = time.Time{} }()
	d, _ := Platform("pumasi-test", t.TempDir())
	err := d.Start(context.Background(), Spec{Name: "t", CPUs: 1, MemBytes: 1 << 30, Driver: "native", Image: "x:2.338.2"})
	if err == nil || !strings.Contains(err.Error(), "no systemd") {
		t.Fatalf("err %v, want the missing systemd user session", err)
	}
	l := d.(*linux)
	if p := l.NativeProblem(); !strings.Contains(p, "no systemd") {
		t.Errorf("problem %q", p)
	}
	if es, _ := os.ReadDir(filepath.Join(l.native.Root, "runners")); len(es) != 0 {
		t.Errorf("%d runner directories were made", len(es))
	}
}
