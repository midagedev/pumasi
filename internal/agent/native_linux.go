package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/midagedev/pumasi/internal/cgroup"
)

const distOS = "linux"

var cgroups = cgroup.DefaultReader()

// runnerRoot: /var/tmp is on disk (unlike many a /tmp) and outlives a reboot.
func runnerRoot() string { return "/var/tmp/pumasi-" + strconv.Itoa(os.Getuid()) }

// tree is one runner's processes, held by a systemd user scope: a cgroup of
// its own under the user's systemd, which holds them to the class's CPU and
// memory, counts what they use and ends them all at once. The scope belongs
// to systemd, not to the agent, so a running job outlives its agent.
type tree struct {
	dir  string
	unit string // pumasi-<runner>.scope
	cg   string // the scope's cgroup directory ("" = not found)
	pid  int    // the shell around run.sh
	born int64  // its start time, in clock ticks since boot
	u    Usage
}

func unitName(dir string) string { return "pumasi-" + filepath.Base(dir) + ".scope" }

// userEnv makes systemd's user instance reachable from a shell that was not
// given a login session's environment (ssh without PAM, wsl -e).
func userEnv(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			return env
		}
	}
	return append(env[:len(env):len(env)], "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
}

func systemctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.Env = userEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var systemdVersion = sync.OnceValue(func() int {
	out, _ := exec.Command("systemctl", "--version").Output() // "systemd 255 (255.4-1ubuntu8)"
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(f[1])
	return v
})

// procStat reads a process's start time and whether it is a zombie.
func procStat(pid int) (born int64, zombie, ok bool) {
	b, err := os.ReadFile(filepath.Join(cgroups.ProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false, false
	}
	// "pid (comm) state ppid ... starttime(22) ...": comm may hold spaces and parentheses.
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return 0, false, false
	}
	born, err = strconv.ParseInt(f[19], 10, 64)
	return born, f[0] == "Z", err == nil
}

// startTree runs the runner inside a new scope. systemd-run puts itself into
// the scope and then becomes the command, so the pid started here is the
// runner's shell, already under its limits before it runs anything.
func startTree(dir string, s Spec, env []string, log *os.File) (*tree, error) {
	t := &tree{dir: dir, unit: unitName(dir)}
	args := []string{"--user", "--scope", "--quiet", "--collect", "--unit=" + t.unit,
		"--description=pumasi runner " + s.Name,
		"-p", "MemorySwapMax=0"}
	// The limits protect the host; they are not the size the runner was placed with.
	if s.LimitCPUs > 0 {
		args = append(args, "-p", fmt.Sprintf("CPUQuota=%d%%", max(1, int(math.Round(s.LimitCPUs*100)))))
	}
	if s.LimitMem > 0 {
		args = append(args, "-p", fmt.Sprintf("MemoryMax=%d", s.LimitMem))
	}
	if systemdVersion() >= 253 {
		// A process of the job killed at the memory limit must not take the
		// runner with it: the runner reports the failed step.
		args = append(args, "-p", "OOMPolicy=continue")
	}
	cmd := exec.Command("systemd-run", append(args, "--", "/bin/sh", "-c", runScript, "sh", dir)...)
	cmd.Env = userEnv(env)
	cmd.Stdout, cmd.Stderr = log, log
	// Its own session: a hangup of the agent's terminal does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reap it; the exit code is in the file
	t.pid = cmd.Process.Pid
	for deadline := time.Now().Add(15 * time.Second); ; {
		if cg, err := cgroups.PathOfPid(t.pid); err == nil && filepath.Base(cg) == t.unit {
			t.cg = cg
			break
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(log.Name())
			return nil, fmt.Errorf("systemd-run: %s", lastLine(b))
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("systemd did not put the runner into %s", t.unit)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.born, _, _ = procStat(t.pid)
	return t, nil
}

// adoptTree takes over a runner started by an earlier agent. systemd knows
// where the scope's cgroup is, also when the runner's shell is gone and only
// processes it left behind are in it.
func adoptTree(dir string, m meta) *tree {
	t := &tree{dir: dir, unit: unitName(dir), pid: m.Pid, born: m.Born}
	if rel, err := systemctl("show", "-p", "ControlGroup", "--value", t.unit); err == nil && rel != "" {
		cg := filepath.Join(cgroups.CgroupRoot, rel)
		if _, err := os.Stat(cg); err == nil {
			t.cg = cg
		}
	}
	return t
}

func refreshTrees(ts map[string]*tree) {
	for _, t := range ts {
		t.refresh()
	}
}

// refresh reads the scope's cgroup. Once the scope is empty systemd removes
// it, and the last values read stay.
func (t *tree) refresh() {
	if t.cg == "" {
		return
	}
	s, err := cgroups.Read(t.cg)
	if err != nil {
		return
	}
	u := Usage{CPUUsec: s.CPUUsec, MemCurrent: int64(s.MemCurrent), MemPeak: int64(s.MemPeak), MemWork: int64(s.MemWork), OOMKills: int(s.OOMKills)}
	// The io controller is often not delegated to users: then nothing is known.
	if _, err := os.Stat(filepath.Join(t.cg, "io.stat")); err == nil {
		io := int64(s.IOBytes)
		u.IOBytes = &io
	}
	if len(s.Pressure) > 0 {
		u.Pressure = s.Pressure
	}
	t.u = u
}

func (t *tree) alive() bool {
	born, zombie, ok := procStat(t.pid)
	return ok && born == t.born && !zombie
}

func (t *tree) usage() Usage { return t.u }

// kill ends every process in the scope at once and waits until it is empty.
func (t *tree) kill() {
	if t.cg != "" && filepath.Base(t.cg) == t.unit {
		_ = os.WriteFile(filepath.Join(t.cg, "cgroup.kill"), []byte("1"), 0)
	}
	// systemd's way too: kernels before 5.14 have no cgroup.kill, and a scope
	// whose cgroup was not found is still known to systemd by name.
	_, _ = systemctl("kill", "--signal=SIGKILL", t.unit)
	for i := 0; i < 50 && t.cg != ""; i++ {
		if b, err := os.ReadFile(filepath.Join(t.cg, "cgroup.procs")); err != nil || len(strings.TrimSpace(string(b))) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, _ = systemctl("stop", t.unit)
}

// The unpacked runner is shared by hard links and kept read-only (native_link.go).
func cloneTree(src, dst string) error { return linkTree(src, dst) }
func sealDist(dir string) error       { return sealShared(dir) }

func hostUsage() (HostUsage, error) { return (&CgroupMeter{R: cgroups}).Host() }

var problem struct {
	sync.Mutex
	at   time.Time
	text string
}

// nativeProblem is why this host cannot run runners as processes: without a
// cgroup of its own a runner would have no limit, would not be measured and
// could not be cleaned up, so it is not started at all. Asked at most twice a
// minute.
func nativeProblem() string {
	problem.Lock()
	defer problem.Unlock()
	if time.Since(problem.at) < 30*time.Second {
		return problem.text
	}
	problem.at, problem.text = time.Now(), findNativeProblem()
	return problem.text
}

func findNativeProblem() string {
	name := strconv.Itoa(os.Getuid())
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "no systemd here: a native runner needs a systemd user session to hold it to its class"
	}
	rel, err := systemctl("show", "-p", "ControlGroup", "--value")
	if err != nil || rel == "" {
		return fmt.Sprintf("no systemd user session for %s (loginctl enable-linger %s starts one that stays)", name, name)
	}
	b, err := os.ReadFile(filepath.Join(cgroups.CgroupRoot, rel, "cgroup.controllers"))
	have := strings.Fields(string(b))
	for _, want := range []string{"cpu", "memory"} {
		if err != nil || !slices.Contains(have, want) {
			return fmt.Sprintf("the systemd user session of %s may not limit %s (cgroup controllers delegated to it: %s)", name, want, strings.Join(have, " "))
		}
	}
	if _, err := os.Stat("/var/lib/systemd/linger/" + name); err != nil {
		return fmt.Sprintf("runners would end when %s logs out: run loginctl enable-linger %s", name, name)
	}
	if miss := MissingRunnerLibs(); len(miss) > 0 {
		return "the runner cannot start here, it needs " + strings.Join(miss, ", ") + " (as root: the distribution's packages, or bin/installdependencies.sh of an unpacked actions/runner)"
	}
	return ""
}

// MissingRunnerLibs names the shared libraries a process runner needs and the
// loader here does not know. Nothing is named when the loader cannot be asked.
func MissingRunnerLibs() []string {
	ldconfig := "ldconfig"
	if _, err := exec.LookPath(ldconfig); err != nil {
		ldconfig = "/sbin/ldconfig" // not in a user's PATH on some distributions
	}
	out, err := exec.Command(ldconfig, "-p").Output()
	if err != nil {
		return nil
	}
	return missingLibs(string(out))
}

func yield(map[string]*tree, bool) {}

var idle struct {
	sync.Mutex
	at  time.Time
	val *time.Duration
}

// inputIdle asks logind about the graphical sessions at this machine's own
// screen. Without one (a server, WSL) nothing is known. Asked a few times a minute.
func inputIdle() *time.Duration {
	idle.Lock()
	defer idle.Unlock()
	if time.Since(idle.at) < 20*time.Second {
		return idle.val
	}
	idle.at, idle.val = time.Now(), nil
	out, err := exec.Command("loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return nil
	}
	var shown []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			if s, err := exec.Command("loginctl", "show-session", f[0], "-p", "Type", "-p", "Active", "-p", "Remote", "-p", "IdleHint", "-p", "IdleSinceHint").Output(); err == nil {
				shown = append(shown, string(s))
			}
		}
	}
	idle.val = sessionsIdle(shown, time.Now())
	return idle.val
}

// sessionsIdle reads `loginctl show-session` outputs: the shortest idle time
// among the active graphical sessions at the local screen. A desktop sets
// IdleHint only after minutes without input; until then the person counts as
// there right now.
func sessionsIdle(shown []string, now time.Time) *time.Duration {
	var best *time.Duration
	for _, s := range shown {
		kv := map[string]string{}
		for _, line := range strings.Split(s, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				kv[k] = v
			}
		}
		if (kv["Type"] != "x11" && kv["Type"] != "wayland" && kv["Type"] != "mir") || kv["Active"] != "yes" || kv["Remote"] == "yes" {
			continue
		}
		d := time.Duration(0)
		if kv["IdleHint"] == "yes" {
			if usec, err := strconv.ParseInt(kv["IdleSinceHint"], 10, 64); err == nil && usec > 0 {
				d = max(0, now.Sub(time.UnixMicro(usec)))
			}
		}
		if best == nil || d < *best {
			best = &d
		}
	}
	return best
}
