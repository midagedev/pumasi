package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Runs only on a Windows machine: a runner script inside a Job Object, with
// a child that must die with the job.
func TestJobObjectTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "@echo off\r\nstart /b ping -n 600 127.0.0.1 > nul\r\nping -n 3 127.0.0.1 > nul\r\nexit /b 7\r\n"
	if err := os.WriteFile(filepath.Join(dir, "runner", "run.cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	tr, err := startTree(dir, Spec{Name: "t", CPUs: 1, MemBytes: 256 << 20}, runnerEnv(os.Environ()), log)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.alive() {
		t.Fatal("not alive right after start")
	}
	for i := 0; i < 100 && tr.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if code, ok := tr.exitCode(); !ok || code != 7 {
		t.Fatalf("exit code %d %v, want 7", code, ok)
	}
	tr.refresh()
	u := tr.usage()
	if u.MemPeak <= 0 || u.IOBytes == nil || u.MemCurrent <= 0 {
		t.Errorf("usage %+v: want a memory peak, io bytes and the memory of the child that is still there", u)
	}
	tr.kill() // the pinging child is still in the job
	tr2 := &tree{}
	if tr2.alive() {
		t.Error("an adopted tree is never alive")
	}
}
