package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
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
	// A later agent finds the job again by name: the pinging child is still in it.
	ad := adoptTree(dir, meta{Pid: tr.pid, Born: tr.born})
	if ad.job == 0 {
		t.Fatal("the job was not found again by name")
	}
	if code, ok := ad.exitCode(); !ok || code != 7 {
		t.Errorf("adopted: exit code %d %v, want 7 from the exit file", code, ok)
	}
	ad.refresh()
	if ad.usage().MemCurrent <= 0 {
		t.Errorf("adopted: usage %+v, want the child's memory", ad.usage())
	}
	tr.close() // the first agent goes away without ending anything
	ad.kill()
	if h, _, _ := procOpenJobObject.Call(jobAllAccess, 0, uintptr(unsafe.Pointer(jobName(dir)))); h != 0 {
		t.Error("the job still exists after kill: a process survived")
	}
}

// A runner that asks for more memory than its class is refused by the job,
// and the refusal is counted.
func TestJobMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "@echo off\r\npowershell -NoProfile -Command \"$a = New-Object byte[] 600MB; Start-Sleep 1\"\r\nexit /b %errorlevel%\r\n"
	if err := os.WriteFile(filepath.Join(dir, "runner", "run.cmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	tr, err := startTree(dir, Spec{Name: "t", CPUs: 1, MemBytes: 300 << 20}, runnerEnv(os.Environ()), log)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.kill()
	for i := 0; i < 300 && tr.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	tr.refresh()
	u := tr.usage()
	code, _ := tr.exitCode()
	out, _ := os.ReadFile(filepath.Join(dir, "log"))
	t.Logf("exit %d, usage %+v, output: %s", code, u, out)
	if u.OOMKills == 0 {
		t.Error("the job did not report a refusal at its memory limit")
	}
	if u.MemPeak > 300<<20 {
		t.Errorf("peak %d is over the 300M limit", u.MemPeak)
	}
}
