package cgroup

import "testing"

func TestKillsAreCountedOnlyAtTheCgroupsOwnLimit(t *testing.T) {
	for _, c := range []struct {
		name, events string
		want         uint64
	}{
		{"at its limit", "low 0\nhigh 0\nmax 41\noom 2\noom_kill 3\noom_group_kill 0\n", 3},
		{"killed when the host ran out", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 1\noom_group_kill 0\n", 0},
		{"nothing", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n", 0},
		{"no file", "", 0},
	} {
		if got := ParseMemoryEvents(c.events); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
