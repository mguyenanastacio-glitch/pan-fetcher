//go:build !windows

package server

import "syscall"

func getDiskUsage(path string) (free, total uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err == nil {
		return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize)
	}
	return 0, 0
}
