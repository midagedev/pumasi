package agent

import "golang.org/x/sys/windows"

// diskSpace is the free and total bytes of the volume holding dir (0, 0 = unknown).
func diskSpace(dir string) (free, total int64) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0
	}
	var avail, all, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &all, &totalFree); err != nil {
		return 0, 0
	}
	return int64(avail), int64(all)
}
