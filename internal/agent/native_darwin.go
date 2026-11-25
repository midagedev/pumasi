package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const distOS = "osx"

// tree is one runner's processes. macOS has nothing like a cgroup to hold
// them, so the tree is found again at every look: the runner's process group,
// every descendant of a known member, and the members already known. That
// follows a child that left the group (setsid, nohup) as long as it was seen
// once while its parent lived.
type tree struct {
	dir  string
	pid  int   // the shell around run.sh: leader of its own session and process group
	born int64 // its start time in microseconds

	known   map[int]int64 // members at the last look: pid -> start time
	left    string        // the members outside the process group, as written to <dir>/pids
	leader  bool          // the shell itself is still running
	bg      bool          // its processes were put into the background band
	cpuUsec uint64
	mem     int64
	peak    int64
}

// startTree runs the runner in its own session: a Ctrl-C in the agent's
// terminal does not reach it and it goes on when the agent stops. The shell
// around run.sh leaves the exit code in a file, because a later agent is not
// its parent and cannot wait for it.
func startTree(dir string, _ Spec, env []string, log *os.File) (*tree, error) {
	cmd := exec.Command("/bin/sh", "-c", runScript, "sh", dir)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }() // reap it; the exit code is in the file
	t := &tree{dir: dir, pid: cmd.Process.Pid, leader: true}
	for i := 0; i < 50 && t.born == 0; i++ {
		if ps, err := processes(); err == nil {
			t.born = ps[t.pid].born
		}
		if t.born == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if t.born == 0 {
		t.born = time.Now().UnixMicro()
	}
	t.known = map[int]int64{t.pid: t.born}
	return t, nil
}

// adoptTree takes over a runner started by an earlier agent.
func adoptTree(dir string, m meta) *tree {
	t := &tree{dir: dir, pid: m.Pid, born: m.Born, known: map[int]int64{m.Pid: m.Born}}
	// Members that had left the process group, as the earlier agent last saw them.
	if b, err := os.ReadFile(filepath.Join(dir, "pids")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			var pid int
			var born int64
			if _, err := fmt.Sscan(line, &pid, &born); err == nil {
				t.known[pid] = born
			}
		}
	}
	return t
}

type process struct {
	ppid, pgid int
	born       int64
	zombie     bool
}

func processes() (map[int]process, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	ps := make(map[int]process, len(ks))
	for _, k := range ks {
		const szomb = 5
		ps[int(k.Proc.P_pid)] = process{ppid: int(k.Eproc.Ppid), pgid: int(k.Eproc.Pgid),
			born: k.Proc.P_starttime.Sec*1e6 + int64(k.Proc.P_starttime.Usec), zombie: k.Proc.P_stat == szomb}
	}
	return ps, nil
}

// refreshTrees reads the process table once and updates every tree from it.
func refreshTrees(ts map[string]*tree) {
	ps, err := processes()
	if err != nil {
		return
	}
	children := map[int][]int{}
	for pid, p := range ps {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	for _, t := range ts {
		t.refresh(ps, children)
	}
}

func (t *tree) refresh(ps map[int]process, children map[int][]int) {
	is := func(pid int, born int64) bool { p, ok := ps[pid]; return ok && p.born == born && !p.zombie }
	t.leader = is(t.pid, t.born)

	members := map[int]int64{}
	groupLives := false // a pid is reused only after its process group is empty
	for pid, born := range t.known {
		if is(pid, born) {
			members[pid] = born
			groupLives = groupLives || ps[pid].pgid == t.pid
		}
	}
	if groupLives {
		for pid, p := range ps {
			if p.pgid == t.pid && p.born >= t.born && !p.zombie {
				members[pid] = p.born
			}
		}
	}
	queue := make([]int, 0, len(members))
	for pid := range members {
		queue = append(queue, pid)
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if _, ok := members[c]; !ok && !ps[c].zombie && ps[c].born >= members[pid] {
				members[c] = ps[c].born
				queue = append(queue, c)
			}
		}
	}

	var left []string // members outside the process group, for an agent that restarts
	for pid, born := range members {
		if ps[pid].pgid != t.pid {
			left = append(left, fmt.Sprintf("%d %d", pid, born))
		}
	}
	sort.Strings(left)
	if s := strings.Join(left, "\n"); s != t.left {
		t.left = s
		_ = os.WriteFile(filepath.Join(t.dir, "pids"), []byte(s), 0o600)
	}
	t.known = members

	// CPU time: each live member's own plus that of the children it has waited
	// for, so a compiler that came and went between two looks is still counted.
	// Parents are read first: a child reaped in between is then missed for one
	// look rather than counted twice.
	pids := make([]int, 0, len(members))
	for pid := range members {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return members[pids[i]] < members[pids[j]] })
	var cpu, mem uint64
	for _, pid := range pids {
		ri, err := procRusage(pid)
		if err != nil {
			continue
		}
		cpu += ri.UserTime + ri.SystemTime + ri.ChildUserTime + ri.ChildSystemTime
		mem += ri.PhysFootprint
	}
	t.cpuUsec = max(t.cpuUsec, machTimeUsec(cpu))
	t.mem = int64(mem)
	t.peak = max(t.peak, t.mem)
}

func (t *tree) alive() bool { return t.leader }

// usage: CPU time and memory are measured; disk I/O is not (libproc counts it
// per live process only, so short-lived processes would be missing) and there
// is no pressure accounting per process tree.
func (t *tree) usage() Usage {
	return Usage{CPUUsec: t.cpuUsec, MemCurrent: t.mem, MemPeak: t.peak}
}

// kill ends every process of the tree: SIGTERM, then SIGKILL after two
// seconds, looking again each round for what was forked meanwhile.
func (t *tree) kill() {
	for i := 0; i < 100; i++ {
		refreshTrees(map[string]*tree{"": t})
		if len(t.known) == 0 {
			return
		}
		sig := syscall.SIGTERM
		if i >= 20 {
			sig = syscall.SIGKILL
		}
		for pid := range t.known {
			_ = syscall.Kill(pid, sig)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runnerRoot: /Users/Shared is on the same volume as the home (clonefile
// works) and nothing sweeps it, unlike /tmp.
func runnerRoot() string { return "/Users/Shared/pumasi-" + strconv.Itoa(os.Getuid()) }

// cloneTree copies the unpacked runner. On APFS clonefile(2) makes the copy
// without copying data; elsewhere cp does it the slow way.
func cloneTree(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	out, err := exec.Command("cp", "-R", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastLine(out))
	}
	return nil
}

// hostUsage: macOS has no PSI. The kernel's own memory pressure level closes
// the host; what others use of the CPU shows in the host CPU time.
func hostUsage() (HostUsage, error) {
	var h HostUsage
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return h, err
	}
	h.MemBytes = int64(total)
	// The percentage of memory the kernel considers free for use (what `memory_pressure` prints).
	if level, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil {
		h.MemAvailable = int64(total) / 100 * int64(min(level, 100))
	}
	if level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		switch level {
		case 2:
			h.Closed = "memory pressure (warning)"
		case 4:
			h.Closed = "memory pressure (critical)"
		}
	}
	h.InputIdle = inputIdle()
	h.CPUUsec, err = hostCPUUsec()
	return h, err
}

var hidIdleRE = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// inputIdle asks the HID system how long ago a key or the pointer moved.
func inputIdle() *time.Duration {
	out, err := exec.Command("/usr/sbin/ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return nil
	}
	m := hidIdleRE.FindSubmatch(out)
	if m == nil {
		return nil
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return nil
	}
	d := time.Duration(ns)
	return &d
}

const (
	prioDarwinProcess = 4      // PRIO_DARWIN_PROCESS
	prioDarwinBG      = 0x1000 // PRIO_DARWIN_BG
)

// background moves a process into or out of the band macOS keeps for work
// nobody waits for (what `taskpolicy -b` does): low CPU priority, throttled
// disk, efficiency cores first. Children started afterwards inherit it.
func background(pid int, on bool) error {
	prio := 0
	if on {
		prio = prioDarwinBG
	}
	return unix.Setpriority(prioDarwinProcess, pid, prio)
}

// yield puts every runner's processes behind the person at the machine, and
// back when the person has gone.
func yield(ts map[string]*tree, present bool) {
	for _, t := range ts {
		if !present && !t.bg {
			continue
		}
		for pid := range t.known {
			_ = background(pid, present)
		}
		t.bg = present
	}
}

// BootID changes when the host has rebooted.
func BootID() string {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return ""
	}
	return strconv.FormatInt(tv.Sec, 10)
}

func nativeProblem() string { return "" }
