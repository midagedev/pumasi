package agent

import (
	"syscall"
	"unsafe"
)

// macOS has no cgroups: what a process used is asked from libproc, and the
// host's CPU time from Mach. Both live in libSystem, which every Go binary on
// macOS links dynamically even with cgo off, so they are called the way
// golang.org/x/sys/unix calls libc: through a trampoline (libproc_darwin.s).

//go:cgo_import_dynamic libc_proc_pid_rusage proc_pid_rusage "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics host_statistics "/usr/lib/libSystem.B.dylib"

var (
	libc_proc_pid_rusage_trampoline_addr    uintptr
	libc_mach_timebase_info_trampoline_addr uintptr
	libc_mach_host_self_trampoline_addr     uintptr
	libc_host_statistics_trampoline_addr    uintptr
)

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// rusageInfo is struct rusage_info_v2 of <sys/resource.h>.
type rusageInfo struct {
	UUID                [16]byte
	UserTime            uint64 // Mach time units, like every time below
	SystemTime          uint64
	PkgIdleWkups        uint64
	InterruptWkups      uint64
	Pageins             uint64
	WiredSize           uint64
	ResidentSize        uint64
	PhysFootprint       uint64 // what Activity Monitor calls Memory
	ProcStartAbstime    uint64
	ProcExitAbstime     uint64
	ChildUserTime       uint64 // of children this process has already waited for
	ChildSystemTime     uint64
	ChildPkgIdleWkups   uint64
	ChildInterruptWkups uint64
	ChildPageins        uint64
	ChildElapsedAbstime uint64
	DiskioBytesRead     uint64
	DiskioBytesWritten  uint64
}

// procRusage asks for one process. It fails for processes of other users.
func procRusage(pid int) (rusageInfo, error) {
	var ri rusageInfo
	const rusageInfoV2 = 2
	r, _, e := syscall_syscall(libc_proc_pid_rusage_trampoline_addr, uintptr(pid), rusageInfoV2, uintptr(unsafe.Pointer(&ri)))
	if int32(r) != 0 {
		return ri, e
	}
	return ri, nil
}

// machTimeUsec converts Mach time units to microseconds (1:1 ns on Intel, 125/3 ns on Apple silicon).
var machTimeUsec = func() func(uint64) uint64 {
	var tb struct{ numer, denom uint32 }
	syscall_syscall(libc_mach_timebase_info_trampoline_addr, uintptr(unsafe.Pointer(&tb)), 0, 0)
	if tb.numer == 0 || tb.denom == 0 {
		tb.numer, tb.denom = 1, 1
	}
	return func(t uint64) uint64 { return t / 1000 * uint64(tb.numer) / uint64(tb.denom) }
}()

// hostCPUUsec is the CPU time all processors have spent not idle since boot.
func hostCPUUsec() (uint64, error) {
	const hostCPULoadInfo, stateIdle, ticksPerSec = 3, 2, 100
	var ticks [4]uint32 // user, system, idle, nice
	count := uint32(len(ticks))
	host, _, _ := syscall_syscall(libc_mach_host_self_trampoline_addr, 0, 0, 0)
	r, _, _ := syscall_syscall6(libc_host_statistics_trampoline_addr, host, hostCPULoadInfo,
		uintptr(unsafe.Pointer(&ticks)), uintptr(unsafe.Pointer(&count)), 0, 0)
	if int32(r) != 0 {
		return 0, syscall.Errno(r)
	}
	var busy uint64
	for i, t := range ticks {
		if i != stateIdle {
			busy += uint64(t)
		}
	}
	return busy * (1e6 / ticksPerSec), nil
}
