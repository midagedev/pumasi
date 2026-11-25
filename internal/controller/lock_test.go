package controller

import (
	"strings"
	"testing"
)

func TestLockAllowsOneHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil || !strings.Contains(err.Error(), "already running (pid ") {
		t.Fatalf("second holder: %v", err)
	}
	release()
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
