package agent

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	distOS  = "win"
	distExt = ".zip"
)

// tree is one runner's processes, held by a Job Object: every process the
// runner starts is in the job, the job holds them to the class's memory and
// CPU, counts what they use, and ends them all when pumasi terminates it.
//
// The job has a name and no kill-on-close: it lives as long as a process is
// in it, so a running job survives its agent and the next agent opens the job
// again by name. Only Remove ends it. The name lasts as long as some process
// holds a handle to the job, so the runner's processes inherit one.
type tree struct {
	dir             string
	job, proc, port windows.Handle
	pid             int
	born            int64
	denied          int // times the job was refused memory at its limit
	u               Usage
}

// Not in golang.org/x/sys/windows.
type (
	jobAccounting struct { // JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION
		TotalUserTime, TotalKernelTime           int64 // 100ns
		ThisPeriodUserTime, ThisPeriodKernelTime int64
		PageFaults, Processes, Active, Ended     uint32
		IO                                       windows.IO_COUNTERS
	}
	jobCPURate struct { // JOBOBJECT_CPU_RATE_CONTROL_INFORMATION
		ControlFlags uint32
		CPURate      uint32 // hundredths of a percent of all processors
	}
	jobPort struct { // JOBOBJECT_ASSOCIATE_COMPLETION_PORT
		Key  uintptr
		Port windows.Handle
	}
	jobPids struct { // JOBOBJECT_BASIC_PROCESS_ID_LIST
		Assigned, InList uint32
		Pids             [1024]uintptr
	}
	processMemory struct { // PROCESS_MEMORY_COUNTERS
		Size, PageFaults                 uint32
		PeakWorkingSet, WorkingSet       uintptr
		QuotaPeakPaged, QuotaPaged       uintptr
		QuotaPeakNonPaged, QuotaNonPaged uintptr
		PagefileUsage, PeakPagefileUsage uintptr // commit charge: what the job limit counts
	}
	memoryStatus struct { // MEMORYSTATUSEX
		Length, Load                             uint32
		TotalPhys, AvailPhys                     uint64
		TotalPageFile, AvailPageFile             uint64 // the commit limit and what is left of it
		TotalVirtual, AvailVirtual, AvailExtened uint64
	}
)

const (
	cpuRateEnable, cpuRateHardCap      = 0x1, 0x4
	msgProcessMemLimit, msgJobMemLimit = 9, 10
	stillActive                        = 259
	jobAllAccess                       = 0x1F001F
)

// jobName is the Job Object of a runner directory. The name lives in the
// logon session's namespace: an agent started in another session does not
// find it and reports the runner as exited.
func jobName(dir string) *uint16 {
	p, _ := windows.UTF16PtrFromString("pumasi-" + filepath.Base(dir))
	return p
}

// procBorn is a process's creation time, which tells it from a reused pid.
func procBorn(h windows.Handle) int64 {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime)
}

// watch has the job tell through a completion port when it refuses memory at the limit.
func (t *tree) watch() error {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return fmt.Errorf("completion port: %w", err)
	}
	set := func(p windows.Handle) error {
		assoc := jobPort{Port: p}
		_, err := windows.SetInformationJobObject(t.job, windows.JobObjectAssociateCompletionPortInformation,
			uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc)))
		return err
	}
	if err = set(port); err != nil {
		// The job of an earlier agent still points at that agent's port: let go of it first.
		if err = set(0); err == nil {
			err = set(port)
		}
	}
	if err != nil {
		windows.CloseHandle(port)
		return fmt.Errorf("job port: %w", err)
	}
	t.port = port
	return nil
}

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessMemory   = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
	procOpenJobObject      = kernel32.NewProc("OpenJobObjectW")
)

func startTree(dir string, s Spec, env []string, log *os.File) (t *tree, err error) {
	t = &tree{dir: dir}
	defer func() {
		if err != nil {
			t.kill()
		}
	}()
	if t.job, err = windows.CreateJobObject(nil, jobName(dir)); err != nil {
		return nil, fmt.Errorf("job object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{JobMemoryLimit: uintptr(s.MemBytes)}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, fmt.Errorf("job limits: %w", err)
	}
	rate := jobCPURate{ControlFlags: cpuRateEnable | cpuRateHardCap,
		CPURate: uint32(min(10000, max(1, s.CPUs/float64(runtime.NumCPU())*10000)))}
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectCpuRateControlInformation,
		uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate))); err != nil {
		return nil, fmt.Errorf("job cpu rate: %w", err)
	}
	if err = t.watch(); err != nil {
		return nil, err
	}

	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	// The runner's shell inherits a handle to its own job. A job's name goes
	// away with the last handle to it, even while processes are still in the
	// job: without this a later agent could not open the job by name.
	for _, h := range []windows.Handle{windows.Handle(null.Fd()), windows.Handle(log.Fd()), t.job} {
		if err = windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, err
		}
		defer windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0)
	}
	si := windows.StartupInfo{Flags: windows.STARTF_USESTDHANDLES,
		StdInput: windows.Handle(null.Fd()), StdOutput: windows.Handle(log.Fd()), StdErr: windows.Handle(log.Fd())}
	si.Cb = uint32(unsafe.Sizeof(si))
	// The shell around run.cmd leaves the exit code in a file: a later agent
	// may find the process gone and has nobody to ask.
	cmdline, err := windows.UTF16PtrFromString(`cmd.exe /d /v:on /c "call run.cmd & echo !errorlevel!> ..\exit"`)
	if err != nil {
		return nil, err
	}
	cwd, err := windows.UTF16PtrFromString(filepath.Join(dir, "runner"))
	if err != nil {
		return nil, err
	}
	var block []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			continue // a variable with a NUL in it
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	// Suspended until it is in the job: nothing it starts can escape.
	var pi windows.ProcessInformation
	if err = windows.CreateProcess(nil, cmdline, nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.CREATE_NEW_PROCESS_GROUP,
		&block[0], cwd, &si, &pi); err != nil {
		return nil, fmt.Errorf("run.cmd: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	t.proc, t.pid, t.born = pi.Process, int(pi.ProcessId), procBorn(pi.Process)
	if err = windows.AssignProcessToJobObject(t.job, t.proc); err != nil {
		_ = windows.TerminateProcess(t.proc, 1)
		return nil, fmt.Errorf("assign to job: %w", err)
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}
	return t, nil
}

// adoptTree takes over a runner started by an earlier agent: its job by name,
// its shell by pid if that is still the same process.
func adoptTree(dir string, m meta) *tree {
	t := &tree{dir: dir, pid: m.Pid, born: m.Born}
	if h, _, _ := procOpenJobObject.Call(jobAllAccess, 0, uintptr(unsafe.Pointer(jobName(dir)))); h != 0 {
		t.job = windows.Handle(h)
		_ = t.watch() // the earlier agent's port is gone with it
	}
	if h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(m.Pid)); err == nil {
		if t.job != 0 && procBorn(h) == m.Born {
			t.proc = h
		} else {
			windows.CloseHandle(h)
		}
	}
	return t
}

func refreshTrees(ts map[string]*tree) {
	for _, t := range ts {
		t.refresh()
	}
}

func (t *tree) refresh() {
	if t.job == 0 {
		return
	}
	var acct jobAccounting
	if windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAndIoAccountingInformation,
		uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil) == nil {
		t.u.CPUUsec = uint64(acct.TotalUserTime+acct.TotalKernelTime) / 10
		io := int64(acct.IO.ReadTransferCount + acct.IO.WriteTransferCount)
		t.u.IOBytes = &io
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if windows.QueryInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil) == nil {
		// Windows counts a request it refused into the peak; the job never held more than its limit.
		t.u.MemPeak = int64(limits.PeakJobMemoryUsed)
		if l := int64(limits.JobMemoryLimit); l > 0 {
			t.u.MemPeak = min(t.u.MemPeak, l)
		}
	}
	var pids jobPids
	if windows.QueryInformationJobObject(t.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&pids)), uint32(unsafe.Sizeof(pids)), nil) == nil {
		var mem int64
		for _, pid := range pids.Pids[:min(int(pids.InList), len(pids.Pids))] {
			h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if err != nil {
				continue
			}
			pm := processMemory{}
			pm.Size = uint32(unsafe.Sizeof(pm))
			if r, _, _ := procGetProcessMemory.Call(uintptr(h), uintptr(unsafe.Pointer(&pm)), uintptr(pm.Size)); r != 0 {
				mem += int64(pm.PagefileUsage)
			}
			windows.CloseHandle(h)
		}
		t.u.MemCurrent = mem
	}
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if windows.GetQueuedCompletionStatus(t.port, &msg, &key, &ov, 0) != nil {
			break
		}
		if msg == msgJobMemLimit || msg == msgProcessMemLimit {
			t.denied++
		}
	}
	t.u.OOMKills = t.denied
}

func (t *tree) alive() bool {
	if t.proc == 0 {
		return false
	}
	ev, _ := windows.WaitForSingleObject(t.proc, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

func (t *tree) exitCode() (int, bool) {
	if b, err := os.ReadFile(filepath.Join(t.dir, "exit")); err == nil {
		if code, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			return code, true
		}
	}
	var code uint32
	if t.proc == 0 || windows.GetExitCodeProcess(t.proc, &code) != nil || code == stillActive {
		return 0, false
	}
	return int(int32(code)), true
}

func (t *tree) usage() Usage { return t.u }

// kill ends every process in the job and waits until the job is empty.
func (t *tree) kill() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		for i := 0; i < 50; i++ {
			var acct jobAccounting
			if windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAndIoAccountingInformation,
				uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil) != nil || acct.Active == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.close()
}

func (t *tree) close() {
	for _, h := range []*windows.Handle{&t.proc, &t.port, &t.job} {
		if *h != 0 {
			_ = windows.CloseHandle(*h)
			*h = 0
		}
	}
}

// cloneTree makes a runner directory out of hard links to the unpacked
// runner's bin and externals (hundreds of megabytes that are only read). The
// few files at the top are copied: the runner rewrites some of them.
func cloneTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case strings.ContainsRune(rel, filepath.Separator) && os.Link(p, to) == nil:
			return nil
		}
		return copyFile(p, to)
	})
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func unpack(arc *os.File, size int64, dir string) error {
	zr, err := zip.NewReader(arc, size)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		p, err := inside(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err == nil {
			_, err = io.Copy(out, in)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// hostUsage: Windows has no PSI. A commit charge close to its limit closes
// the host: beyond it allocations fail for everyone on the machine.
func hostUsage() (HostUsage, error) {
	var h HostUsage
	ms := memoryStatus{}
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, err := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return h, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	h.MemBytes, h.MemAvailable = int64(ms.TotalPhys), int64(ms.AvailPhys)
	if ms.TotalPageFile > 0 {
		if used := 100 - 100*ms.AvailPageFile/ms.TotalPageFile; used >= 90 {
			h.Closed = fmt.Sprintf("commit charge at %d%% of its limit", used)
		}
	}
	var idle, kernel, user windows.Filetime
	if r, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return h, fmt.Errorf("GetSystemTimes: %w", err)
	}
	ticks := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	h.CPUUsec = (ticks(kernel) + ticks(user) - ticks(idle)) / 10 // kernel time includes idle
	return h, nil
}

// BootID is not known on Windows: a runner lost to a reboot is reported as
// died, not as rebooted.
func BootID() string { return "" }
