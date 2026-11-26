package main

import "strings"

// sweepScopes ends the systemd scopes of process runners that have no record
// any more (their directory is gone, the scope still holds processes) and
// returns how many there were. The runners with a record went with the driver.
func sweepScopes() int {
	out, err := userSystemctl("list-units", "pumasi-*.scope", "--all", "--no-legend", "--plain").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "pumasi-") || !strings.HasSuffix(f[0], ".scope") {
			continue
		}
		_ = systemctl("kill", "--signal=SIGKILL", f[0])
		_ = systemctl("stop", f[0])
		n++
	}
	return n
}
