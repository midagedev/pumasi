package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Lock makes sure one pumasi process works on a state directory at a time. Two
// controllers on the same scale sets fight over the message session, replace
// each other's agents and remove each other's status address. The lock is an
// flock, so it goes away with the process however it ends.
func Lock(stateDir string) (release func(), err error) {
	path := filepath.Join(stateDir, "pumasi.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		return nil, fmt.Errorf("another pumasi up or down is already running (pid %s); stop it first", strings.TrimSpace(string(b)))
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return func() { f.Close() }, nil
}
