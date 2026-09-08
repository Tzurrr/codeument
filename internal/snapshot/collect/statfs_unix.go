//go:build !windows

package collect

import "syscall"

// diskUsage reports total and used space for a mount point, in gigabytes.
func diskUsage(mount string) (totalGB, usedGB, percent float64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil || st.Blocks == 0 {
		return 0, 0, 0, false
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	if total <= 0 {
		return 0, 0, 0, false
	}
	return round1(total / 1e9), round1((total - free) / 1e9), round1((total - free) / total * 100), true
}
