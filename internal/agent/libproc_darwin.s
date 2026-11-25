#include "textflag.h"

TEXT libc_proc_pid_rusage_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_pid_rusage(SB)
GLOBL	·libc_proc_pid_rusage_trampoline_addr(SB), RODATA, $8
DATA	·libc_proc_pid_rusage_trampoline_addr(SB)/8, $libc_proc_pid_rusage_trampoline<>(SB)

TEXT libc_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_timebase_info(SB)
GLOBL	·libc_mach_timebase_info_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_timebase_info_trampoline_addr(SB)/8, $libc_mach_timebase_info_trampoline<>(SB)

TEXT libc_mach_host_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·libc_mach_host_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_host_self_trampoline_addr(SB)/8, $libc_mach_host_self_trampoline<>(SB)

TEXT libc_host_statistics_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics(SB)
GLOBL	·libc_host_statistics_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics_trampoline_addr(SB)/8, $libc_host_statistics_trampoline<>(SB)
