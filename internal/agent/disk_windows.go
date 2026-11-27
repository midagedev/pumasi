package agent

import (
	"strings"

	"golang.org/x/sys/windows"
)

// diskStat is the free and total bytes of the volume holding p, named by the
// volume's mount point ("C:\", or the folder a volume is mounted at). A path
// that is not there fails with an error errors.Is(err, fs.ErrNotExist) knows.
func diskStat(p string) (diskFree, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return diskFree{}, err
	}
	var avail, all, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(u, &avail, &all, &totalFree); err != nil {
		return diskFree{}, err
	}
	id := strings.ToLower(p)
	vol := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(u, &vol[0], uint32(len(vol))); err == nil {
		id = strings.ToLower(windows.UTF16ToString(vol))
	}
	return diskFree{free: int64(avail), total: int64(all), id: id}, nil
}
