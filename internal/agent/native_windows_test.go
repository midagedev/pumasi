package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
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
	// The first agent goes away without ending anything. A later agent finds
	// the job again by name: the pinging child is still in it.
	pid, born := tr.pid, tr.born
	tr.close()
	ad := adoptTree(dir, meta{Pid: pid, Born: born})
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
	ad.kill()
	// The name goes when the last dying process has let go of its handle.
	for i := 0; ; i++ {
		h, _, _ := procOpenJobObject.Call(jobAllAccess, 0, uintptr(unsafe.Pointer(jobName(dir))))
		if h == 0 {
			break
		}
		var acct jobAccounting
		_ = windows.QueryInformationJobObject(windows.Handle(h), windows.JobObjectBasicAndIoAccountingInformation,
			uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil)
		windows.CloseHandle(windows.Handle(h))
		if acct.Active != 0 || i == 30 {
			t.Fatalf("the job still exists after kill, with %d processes in it", acct.Active)
		}
		time.Sleep(100 * time.Millisecond)
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

// An agent that takes over a running runner still hears the job refuse memory.
func TestJobMemoryLimitAfterAdopt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "runner"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "@echo off\r\nping -n 4 127.0.0.1 > nul\r\npowershell -NoProfile -Command \"$a = New-Object byte[] 600MB; Start-Sleep 1\"\r\nexit /b 0\r\n"
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
	pid, born := tr.pid, tr.born
	tr.close() // the first agent is gone, its completion port with it
	ad := adoptTree(dir, meta{Pid: pid, Born: born})
	defer ad.kill()
	if ad.job == 0 || ad.port == 0 || !ad.alive() {
		t.Fatalf("adopted: job %v port %v alive %v, want all of them", ad.job != 0, ad.port != 0, ad.alive())
	}
	for i := 0; i < 300 && ad.alive(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	ad.refresh()
	if u := ad.usage(); u.OOMKills == 0 || u.CPUUsec == 0 {
		t.Errorf("adopted: usage %+v, want a refusal and cpu time", u)
	}
}

// With PUMASI_TEST_IMAGE=ghcr.io/actions/actions-runner:<version> this fetches
// the real runner, unpacks it and clones it the way a runner start does.
func TestRunnerDistAndClone(t *testing.T) {
	image := os.Getenv("PUMASI_TEST_IMAGE")
	if image == "" {
		t.Skip("PUMASI_TEST_IMAGE is not set")
	}
	n := &Native{Root: t.TempDir()}
	began := time.Now()
	dist, err := n.dist(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fetched and unpacked in %s", time.Since(began).Round(time.Millisecond))
	began = time.Now()
	clone := filepath.Join(n.Root, "runners", "t", "runner")
	if err := cloneTree(dist, clone); err != nil {
		t.Fatal(err)
	}
	t.Logf("cloned in %s", time.Since(began).Round(time.Millisecond))
	same := func(rel string) bool {
		a, err1 := os.Stat(filepath.Join(dist, rel))
		b, err2 := os.Stat(filepath.Join(clone, rel))
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: %v %v", rel, err1, err2)
		}
		return os.SameFile(a, b)
	}
	if !same(filepath.Join("bin", "Runner.Listener.exe")) {
		t.Error("bin/Runner.Listener.exe is a copy, want a hard link")
	}
	if same("run.cmd") {
		t.Error("run.cmd is a hard link, want a copy: the runner rewrites files at the top")
	}
}
