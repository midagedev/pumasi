//go:build !windows

package agent

import "syscall"

// diskSpace is the free and total bytes of the filesystem holding dir (0, 0 = unknown).
func diskSpace(dir string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}
