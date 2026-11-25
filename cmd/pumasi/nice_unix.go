//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// niceness is this process's nice value (0 when it cannot be read).
func niceness() int {
	out, err := exec.Command("ps", "-o", "nice=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}
