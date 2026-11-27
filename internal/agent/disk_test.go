package agent

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const gib64 = int64(1) << 30

// fakeDisks: path -> filesystem. A path not listed is not there.
type fakeDisks map[string]diskFree

func (f fakeDisks) stat(p string) (diskFree, error) {
	if d, ok := f[p]; ok {
		return d, nil
	}
	return diskFree{}, fs.ErrNotExist
}

// The host's free disk is the fullest of the filesystems runners live on,
// each counted once, named by the first path given on it; a path not made yet
// is measured at its parent.
func TestFullestDisk(t *testing.T) {
	hdd := diskFree{free: 810 * gib64, total: 1000 * gib64, id: "8:1"}
	nvme := diskFree{free: 6500 << 20, total: 400 * gib64, id: "259:2"}
	disks := fakeDisks{"/srv/pumasi": hdd, "/var/tmp/pumasi-1000": hdd, "/nvme/ci": nvme, "/nvme/ci/work": nvme, "/": hdd}

	path, free, total := fullest([]string{"/srv/pumasi", "/var/tmp/pumasi-1000", "/nvme/ci/work", "/nvme/ci/home/3"}, disks.stat)
	if path != "/nvme/ci/work" || free != nvme.free || total != nvme.total {
		t.Fatalf("fullest %q %d/%d, want the NVMe filesystem named by /nvme/ci/work", path, free, total)
	}
	// The slot's directory not made yet: measured at /nvme/ci, its nearest parent.
	if path, _, _ := fullest([]string{"/srv/pumasi", "/nvme/ci/home/3"}, disks.stat); path != "/nvme/ci/home/3" {
		t.Fatalf("a path not there yet: %q", path)
	}
	// One filesystem: the first path names it, and it is measured once — a
	// second look a moment later (a file written in between) does not move it.
	later := hdd
	later.free -= 4096
	disks["/var/tmp/pumasi-1000"] = later
	if path, free, _ := fullest([]string{"/srv/pumasi", "/var/tmp/pumasi-1000", ""}, disks.stat); path != "/srv/pumasi" || free != hdd.free {
		t.Fatalf("one disk: %q %d", path, free)
	}
	// Two equally full by share: the one with fewer bytes free.
	small := fakeDisks{"/a": {free: 10, total: 100, id: "a"}, "/b": {free: 1, total: 10, id: "b"}}
	if path, _, _ := fullest([]string{"/a", "/b"}, small.stat); path != "/b" {
		t.Fatalf("tie: %q", path)
	}
	// Nothing measurable: unknown, which never closes a host.
	broken := func(string) (diskFree, error) { return diskFree{}, fs.ErrPermission }
	if path, free, total := fullest([]string{"/srv/pumasi"}, broken); path != "" || free != 0 || total != 0 {
		t.Fatalf("unmeasurable: %q %d/%d", path, free, total)
	}
}

// On this machine: two directories of one filesystem are one disk (the same
// device id), and a directory not made yet is measured at its parent.
func TestDiskStatDedupesByDevice(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "work")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := diskStat(dir)
	if err != nil || a.total <= 0 || a.id == "" {
		t.Fatalf("stat %s: %+v %v", dir, a, err)
	}
	b, err := diskStat(sub)
	if err != nil || b.id != a.id {
		t.Fatalf("stat %s: %+v %v, want id %q", sub, b, err, a.id)
	}
	if _, err := diskStat(filepath.Join(dir, "none")); !os.IsNotExist(err) {
		t.Fatalf("a path not there: %v", err)
	}
	path, _, total := fullest([]string{dir, sub, filepath.Join(sub, "3", "x")}, diskStat)
	if path != dir || total != a.total {
		t.Fatalf("fullest %q %d", path, total)
	}
}

// diskDriver: a fake driver with directories of its own besides DataDir.
type diskDriver struct {
	fakeDriver
	data string
	own  []string
}

func (d *diskDriver) DataDir(context.Context) string { return d.data }
func (d *diskDriver) DiskDirs() []string             { return d.own }

// The agent reports the fullest of: the driver's data, its other directories
// and the work and slot directories of the runners it lists (started or
// adopted), and names the path. A data dir on a big disk with 81% free no
// longer hides the NVMe the runners work on at 98% full.
func TestAgentReportsTheFullestDiskRunnersLiveOn(t *testing.T) {
	hdd := diskFree{free: 810 * gib64, total: 1000 * gib64, id: "hdd"}
	nvme := diskFree{free: 6500 << 20, total: 400 * gib64, id: "nvme"}
	disks := fakeDisks{"/srv/pumasi": hdd, "/var/tmp/pumasi-1000": hdd, "/nvme/ci": nvme}
	drv := &diskDriver{fakeDriver: fakeDriver{cs: map[string]Container{}}, data: "/srv/pumasi", own: []string{"/srv/pumasi", "/var/tmp/pumasi-1000"}}
	a := New(Config{Host: "server1"}, drv, &fakeMeter{mem: 64 * gib64}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.statDisk = disks.stat
	ctx := context.Background()

	a.Sample(ctx)
	if s := a.Snapshot(); s.DiskPath != "/srv/pumasi" || s.DiskFree != hdd.free || s.DiskTotal != hdd.total {
		t.Fatalf("no runners: %q %d/%d", s.DiskPath, s.DiskFree, s.DiskTotal)
	}
	// A runner adopted from an earlier agent, its slot's work on the NVMe (not made yet).
	drv.cs["r1"] = Container{Name: "r1", Class: "c", State: "running", Dirs: []string{"/nvme/ci/work/1"}}
	a.Sample(ctx)
	s := a.Snapshot()
	if s.DiskPath != "/nvme/ci/work/1" || s.DiskFree != nvme.free || s.DiskTotal != nvme.total {
		t.Fatalf("with a runner on the NVMe: %q %d/%d", s.DiskPath, s.DiskFree, s.DiskTotal)
	}
	// It goes: the NVMe is no longer where runners live.
	delete(drv.cs, "r1")
	a.Sample(ctx)
	if s := a.Snapshot(); s.DiskPath != "/srv/pumasi" {
		t.Fatalf("runner gone: %q", s.DiskPath)
	}
}

// A runner's work and slot directories are where it writes besides its own:
// the class's work with its slot, and the {slot} directories of env and path.
func TestSpecDiskDirs(t *testing.T) {
	s := Spec{Name: "r1", Class: "c", Slot: 3, WorkDir: "/nvme/ci/work/{slot}",
		Env: map[string]string{"HOME": "/nvme/ci/home/{slot}", "X": "{runner}/tmp"}, Path: []string{"/opt/tools/bin"}}
	got := s.diskDirs()
	if len(got) != 2 || got[0] != "/nvme/ci/work/3" || got[1] != "/nvme/ci/home/3" {
		t.Fatalf("dirs %q", got)
	}
	if got := (Spec{Name: "r1", WorkDir: "/nvme/ci/work/{slot}"}).diskDirs(); len(got) != 0 {
		t.Fatalf("no slot yet: %q", got)
	}
	// A container runner's work directory is a literal path, labelled on the container.
	c := inspected{labels: map[string]string{labelWork: "/srv/work/r1"}}.container()
	if len(c.Dirs) != 1 || c.Dirs[0] != "/srv/work/r1" {
		t.Fatalf("container dirs %q", c.Dirs)
	}
}
