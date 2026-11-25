//go:build darwin || linux

package agent

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// slotsUnsupported is why this OS keeps no registered runners ("" = it does).
const slotsUnsupported = ""

const configScript = "config.sh"

// ListenerPid is the pid of the runner listener that runs from runnerDir (0 = none).
func ListenerPid(runnerDir string) int {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	dirs := []string{runnerDir}
	if real, err := filepath.EvalSymlinks(runnerDir); err == nil && real != runnerDir {
		dirs = append(dirs, real)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		for _, d := range dirs {
			if f[1] == filepath.Join(d, "bin", "Runner.Listener") {
				pid, _ := strconv.Atoi(f[0])
				return pid
			}
		}
	}
	return 0
}

// stopIdleListener ends the listener that runs from runnerDir unless a job is
// on its way to it. The listener is frozen first, so that what sign reads of
// its trace cannot go stale before the signal arrives: a listener that has
// not acknowledged a job request when it is frozen has not begun to acquire
// one, and the interrupt it wakes up to makes it close its session instead.
// hard kills it instead of interrupting it (a listener that ignored the
// interrupt). It reports whether the signal was sent.
func stopIdleListener(runnerDir string, sign func() bool, hard bool) (bool, error) {
	pid := ListenerPid(runnerDir)
	if pid == 0 {
		return false, nil // not up yet, or gone already
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		return false, nil
	}
	defer syscall.Kill(pid, syscall.SIGCONT)
	time.Sleep(50 * time.Millisecond) // until every thread of it stands still
	if sign() {
		return false, nil
	}
	sig := syscall.SIGINT
	if hard {
		sig = syscall.SIGKILL
	}
	return true, syscall.Kill(pid, sig)
}
