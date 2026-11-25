package agent

import "errors"

// slotsUnsupported is why this OS keeps no registered runners ("" = it does).
const slotsUnsupported = "registered runners (classes with slots:) are not there for Windows hosts yet"

const configScript = "config.cmd"

// ListenerPid is the pid of the runner listener that runs from runnerDir (0 = none).
func ListenerPid(string) int { return 0 }

func stopIdleListener(string, func() bool, bool) (bool, error) {
	return false, errors.New(slotsUnsupported)
}

func runArgs(Spec) string { return "" }
