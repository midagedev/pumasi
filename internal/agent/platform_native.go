//go:build darwin || windows

package agent

// Platform is how this OS runs and measures runners: as processes, kept in
// dir. tag is unused here.
func Platform(tag, dir string) (Driver, Meter) {
	n := &Native{Root: dir, Runners: RunnerRoot(dir)}
	return n, n
}
