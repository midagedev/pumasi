package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/midagedev/pumasi/internal/cgroup"
)

// A runner with a docker daemon of its own sits in a leaf of the container's
// cgroup; what the job's containers use is counted one above.
func TestInnerDockerRunnerIsMeasuredAtTheContainer(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "cg", "system.slice", "docker-abc.scope")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "proc", "7", "cgroup"), "0::/system.slice/docker-abc.scope/"+innerLeaf+"\n")
	write(filepath.Join(scope, "cpu.stat"), "usage_usec 9000000\n")
	write(filepath.Join(scope, "memory.current"), "1\n")
	write(filepath.Join(scope, "memory.peak"), "1073741824\n")
	write(filepath.Join(scope, "memory.events"), "oom 1\noom_kill 1\n")
	write(filepath.Join(scope, innerLeaf, "cpu.stat"), "usage_usec 1000000\n")
	write(filepath.Join(scope, innerLeaf, "memory.current"), "1\n")
	write(filepath.Join(scope, innerLeaf, "memory.peak"), "100\n")

	m := &CgroupMeter{R: cgroup.Reader{ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg")}}
	u, err := m.Runner(Container{Name: "r", Pid: 7})
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUUsec != 9000000 || u.MemPeak != 1<<30 || u.OOMKills != 1 {
		t.Errorf("measured the leaf, not the container: %+v", u)
	}
}
