package runtimeguard

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each policy/crash check runs in a separate process so it cannot mutate the
// test runner's GC policy or redirect other packages' crash output.
func child(t *testing.T, directory, memory, mode string) (Profile, []byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeGuardChild$")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GOMEMLIMIT=") && !strings.HasPrefix(variable, "PIMG_RUNTIME_GUARD_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GOMEMLIMIT="+memory, "PIMG_RUNTIME_GUARD_MODE="+mode, "PIMG_RUNTIME_GUARD_DIR="+directory)
	output, err := cmd.CombinedOutput()
	var p Profile
	first, _, _ := strings.Cut(string(output), "\n")
	if decodeErr := json.Unmarshal([]byte(first), &p); decodeErr != nil {
		t.Fatalf("child did not report its policy: %v, %s", decodeErr, output)
	}
	return p, output, err
}

func TestRuntimeGuardDefaultsMemoryAndPreservesExplicitSetting(t *testing.T) {
	for _, test := range []struct {
		name, environment string
		want              int64
		defaulted         bool
	}{
		{"default", "", DefaultMemoryLimit, true},
		{"custom", "128MiB", 128 << 20, false},
		{"disabled", "off", math.MaxInt64, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			p, output, err := child(t, directory, test.environment, "ok")
			if err != nil || p.MemoryLimit != test.want || p.MemoryDefaulted != test.defaulted {
				t.Fatalf("profile=%+v err=%v output=%s", p, err, output)
			}
			if p.CrashLog != filepath.Join(directory, "diagnostics", "runtime-crash.log") {
				t.Fatalf("diagnostics escaped data directory: %q", p.CrashLog)
			}
		})
	}
}

func TestRuntimeGuardFatalOutputSurvivesRestart(t *testing.T) {
	directory := t.TempDir()
	p, output, err := child(t, directory, "128MiB", "panic")
	if err == nil || !strings.Contains(string(output), "runtime-guard-intentional-panic") {
		t.Fatalf("expected controlled child panic, got %v: %s", err, output)
	}
	before, err := os.ReadFile(p.CrashLog)
	if err != nil || !strings.Contains(string(before), "panic: runtime-guard-intentional-panic") || !strings.Contains(string(before), "TestRuntimeGuardChild") {
		t.Fatalf("fatal stack was not preserved: %v, %s", err, before)
	}
	_, output, err = child(t, directory, "128MiB", "ok")
	if err != nil {
		t.Fatalf("healthy restart failed: %v, %s", err, output)
	}
	after, err := os.ReadFile(p.CrashLog)
	if err != nil || !strings.HasPrefix(string(after), string(before)) || strings.Count(string(after), "--- pimg start ") != 2 {
		t.Fatalf("restart replaced the previous fatal report: %v, %s", err, after)
	}
}

func TestRuntimeGuardDiagnosticErrorKeepsMemoryPolicy(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, err := child(t, file, "", "diagnostic-error")
	if err != nil || p.MemoryLimit != DefaultMemoryLimit || !p.MemoryDefaulted {
		t.Fatalf("memory policy lost after diagnostic failure: %+v, %v", p, err)
	}
	contents, err := os.ReadFile(file)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("existing path was modified: %s, %v", contents, err)
	}
}

func TestRuntimeGuardChild(t *testing.T) {
	mode := os.Getenv("PIMG_RUNTIME_GUARD_MODE")
	if mode == "" {
		return
	}
	p, err := Configure(os.Getenv("PIMG_RUNTIME_GUARD_DIR"))
	_ = json.NewEncoder(os.Stdout).Encode(p)
	if mode == "diagnostic-error" {
		if err == nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	if mode == "panic" {
		panic("runtime-guard-intentional-panic")
	}
	os.Exit(0)
}
