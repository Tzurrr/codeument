//go:build windows

package collect

import (
	"golang.org/x/sys/windows"
)

// diskUsage reports total and used space for a drive, in gigabytes.
func diskUsage(mount string) (totalGB, usedGB, percent float64, ok bool) {
	path, err := windows.UTF16PtrFromString(mount)
	if err != nil {
		return 0, 0, 0, false
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeToCaller, &total, &totalFree); err != nil || total == 0 {
		return 0, 0, 0, false
	}
	used := float64(total - totalFree)
	return round1(float64(total) / 1e9), round1(used / 1e9), round1(used / float64(total) * 100), true
}
