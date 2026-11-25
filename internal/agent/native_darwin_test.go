package agent

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInputIdleIsKnownOnAMac(t *testing.T) {
	d := inputIdle()
	if d == nil || *d < 0 || *d > 365*24*time.Hour {
		t.Fatalf("input idle %v", d)
	}
	t.Logf("last input %s ago", d.Round(time.Second))
}

// The runners of a tree go into the background band while someone is at the
// machine and come back out of it afterwards.
func TestYieldPutsRunnersInTheBackground(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	// The kernel's priority of the process as ps shows it: 31 normally, 4 in the background band.
	band := func() int {
		out, err := exec.Command("ps", "-o", "pri=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatal(err)
		}
		pri, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		if pri <= 4 {
			return 1
		}
		return 0
	}
	ts := map[string]*tree{"r": {pid: pid, known: map[int]int64{pid: 1}}}
	if band() != 0 {
		t.Fatalf("starts in band %d", band())
	}
	yield(ts, false) // nobody there and never yielded: nothing to do
	if band() != 0 || ts["r"].bg {
		t.Fatalf("band %d", band())
	}
	yield(ts, true)
	if band() != 1 || !ts["r"].bg {
		t.Fatalf("someone is there: band %d, want 1 (background)", band())
	}
	yield(ts, false)
	if band() != 0 || ts["r"].bg {
		t.Fatalf("they left: band %d, want 0", band())
	}
}
