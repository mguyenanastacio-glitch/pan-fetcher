//go:build windows

package server

import (
	"syscall"
	"unsafe"
)

func getDiskUsage(path string) (free, total uint64) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	var freeBytes, totalBytes int64
	proc.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytes)),
		uintptr(unsafe.Pointer(&totalBytes)),
		0,
	)
	return uint64(freeBytes), uint64(totalBytes)
}
