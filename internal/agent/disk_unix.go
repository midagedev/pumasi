//go:build !windows

package agent

import (
	"strconv"
	"syscall"
)

// diskStat is the free and total bytes of the filesystem holding p, named by
// its device number.
func diskStat(p string) (diskFree, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return diskFree{}, err
	}
	var s syscall.Stat_t
	if err := syscall.Stat(p, &s); err != nil {
		return diskFree{}, err
	}
	return diskFree{free: int64(st.Bavail) * int64(st.Bsize), total: int64(st.Blocks) * int64(st.Bsize),
		id: strconv.FormatUint(uint64(s.Dev), 10)}, nil
}
