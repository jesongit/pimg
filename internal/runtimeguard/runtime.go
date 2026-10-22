// Package runtimeguard bounds the default Go heap target and preserves fatal
// runtime output independently of the launcher's replaceable console logs.
package runtimeguard

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

const DefaultMemoryLimit int64 = 512 << 20

type Profile struct {
	GoVersion       string `json:"go_version"`
	GC              string `json:"gc"`
	MemoryLimit     int64  `json:"memory_limit"`
	MemoryDefaulted bool   `json:"memory_defaulted"`
	CrashLog        string `json:"crash_log"`
}

// Close releases the duplicated crash descriptor before a data-directory swap.
func Close() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) }

// Configure must run before starting background workers. GOMEMLIMIT remains the
// operator override; the default is a soft Go memory target, not an RSS cap.
// Failure to install the diagnostic sink does not undo the memory policy.
func Configure(dataDir string) (Profile, error) {
	p := Profile{GoVersion: runtime.Version(), GC: "toolchain-default"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "GOEXPERIMENT" {
				for _, experiment := range strings.Split(setting.Value, ",") {
					if experiment == "nogreenteagc" {
						p.GC = "classic"
					}
				}
			}
		}
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(DefaultMemoryLimit)
		p.MemoryDefaulted = true
	}
	p.MemoryLimit = debug.SetMemoryLimit(-1)
	if strings.TrimSpace(dataDir) == "" {
		return p, fmt.Errorf("运行时日志需要有效的数据目录")
	}
	directory := filepath.Join(dataDir, "diagnostics")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return p, fmt.Errorf("创建运行时日志目录：%w", err)
	}
	p.CrashLog = filepath.Join(directory, "runtime-crash.log")
	f, err := os.OpenFile(p.CrashLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return p, fmt.Errorf("打开运行时日志：%w", err)
	}
	defer f.Close()
	// A minimal startup marker identifies appended reports without recording
	// configuration, environment contents, credentials, or heap data.
	if _, err = fmt.Fprintf(f, "\n--- pimg start %s pid=%d go=%s gc=%s memory_limit=%d ---\n", time.Now().UTC().Format(time.RFC3339Nano), os.Getpid(), p.GoVersion, p.GC, p.MemoryLimit); err != nil {
		return p, fmt.Errorf("写入运行时日志：%w", err)
	}
	// SetCrashOutput duplicates the descriptor; normal file Close is safe.
	if err = debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		return p, fmt.Errorf("启用运行时崩溃日志：%w", err)
	}
	return p, nil
}
