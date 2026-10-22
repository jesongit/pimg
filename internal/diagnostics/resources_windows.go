//go:build windows

package diagnostics

import (
	"syscall"
	"unsafe"
)

var processMemoryProc = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
var diskSpaceProc = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

type processMemoryCounters struct {
	Size                                                                                            uint32
	Faults                                                                                          uint32
	PeakWorking, Working, PeakPaged, Paged, PeakNonpaged, Nonpaged, Pagefile, PeakPagefile, Private uintptr
}

func platformResources(directory string) map[string]any {
	v := map[string]any{}
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return v
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(process, &created, &exited, &kernel, &user) == nil {
		ticks := (uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)) + (uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime))
		v["process_cpu_seconds"] = float64(ticks) / 1e7
	}
	m := processMemoryCounters{}
	m.Size = uint32(unsafe.Sizeof(m))
	if ok, _, _ := processMemoryProc.Call(uintptr(process), uintptr(unsafe.Pointer(&m)), uintptr(m.Size)); ok != 0 {
		v["process_rss_bytes"] = uint64(m.Working)
	}
	if path, err := syscall.UTF16PtrFromString(directory); err == nil {
		var available, total, free uint64
		if ok, _, _ := diskSpaceProc.Call(uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&available)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free))); ok != 0 {
			v["disk_available_bytes"] = available
		}
	}
	return v
}
