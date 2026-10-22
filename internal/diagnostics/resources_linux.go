//go:build linux

package diagnostics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func platformResources(directory string) map[string]any {
	v := map[string]any{}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		v["process_cpu_seconds"] = float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) > 1 {
			if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				v["process_rss_bytes"] = n * uint64(os.Getpagesize())
			}
		}
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(directory, &disk) == nil {
		v["disk_available_bytes"] = disk.Bavail * uint64(disk.Bsize)
	}
	root := "/sys/fs/cgroup"
	if raw, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "0::") {
				candidate := filepath.Join(root, strings.TrimPrefix(filepath.Clean("/"+strings.TrimPrefix(line, "0::")), "/"))
				if _, err := os.Stat(filepath.Join(candidate, "cpu.stat")); err == nil {
					root = candidate
				}
				break
			}
		}
	}
	for _, name := range []string{"memory.current", "memory.max"} {
		if raw, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			if n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
				v["cgroup_"+strings.ReplaceAll(name, ".", "_")+"_bytes"] = n
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(root, "cpu.max")); err == nil {
		parts := strings.Fields(string(raw))
		if len(parts) == 2 {
			quota, e1 := strconv.ParseFloat(parts[0], 64)
			period, e2 := strconv.ParseFloat(parts[1], 64)
			if e1 == nil && e2 == nil && period > 0 {
				v["cgroup_cpu_limit_cores"] = quota / period
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(root, "cpu.stat")); err == nil {
		parts := strings.Fields(string(raw))
		for i := 0; i+1 < len(parts); i += 2 {
			if parts[i] == "nr_throttled" || parts[i] == "throttled_usec" {
				if n, err := strconv.ParseUint(parts[i+1], 10, 64); err == nil {
					v["cgroup_cpu_"+parts[i]] = n
				}
			}
		}
	}
	return v
}
