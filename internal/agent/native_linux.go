package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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
		"-p", fmt.Sprintf("CPUQuota=%d%%", max(1, int(math.Round(s.CPUs*100)))),
		"-p", fmt.Sprintf("MemoryMax=%d", s.MemBytes), "-p", "MemorySwapMax=0"}
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
	u := Usage{CPUUsec: s.CPUUsec, MemCurrent: int64(s.MemCurrent), MemPeak: int64(s.MemPeak), OOMKills: int(s.OOMKills)}
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

// cloneTree makes a runner directory out of hard links to the unpacked
// runner's bin and externals (hundreds of megabytes that are only read). The
// few files at the top are copied: the runner rewrites some of them.
func cloneTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case !d.Type().IsRegular():
			return nil
		case strings.ContainsRune(rel, filepath.Separator) && os.Link(p, to) == nil:
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

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
	return ""
}
