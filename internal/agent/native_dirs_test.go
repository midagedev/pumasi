//go:build darwin || linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A native runner's work and slot directories are kept in its pumasi.json, so
// an agent that adopts it after a restart still measures the disk there.
func TestNativeRunnerDirsSurviveAdoption(t *testing.T) {
	root := t.TempDir()
	n := &Native{Root: filepath.Join(root, "cache"), Runners: filepath.Join(root, "runners")}
	dir := n.dir("r1")
	m := meta{Name: "r1", Class: "c", CPUs: 1, MemBytes: 1 << 30, Pid: 999999, Born: 1, StartedAt: time.Now(),
		Dirs: Spec{Slot: 2, WorkDir: "/nvme/ci/work/{slot}", Env: map[string]string{"HOME": "/nvme/ci/home/{slot}"}}.diskDirs()}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	later := &Native{Root: n.Root, Runners: n.Runners} // a new agent
	cs, err := later.List(context.Background())
	if err != nil || len(cs) != 1 {
		t.Fatalf("list %+v %v", cs, err)
	}
	if want := []string{"/nvme/ci/work/2", "/nvme/ci/home/2"}; !slices.Equal(cs[0].Dirs, want) {
		t.Fatalf("dirs %q, want %q", cs[0].Dirs, want)
	}
	if got := later.DiskDirs(); !slices.Equal(got, []string{n.Root, n.Runners}) {
		t.Fatalf("driver dirs %q", got)
	}
}
