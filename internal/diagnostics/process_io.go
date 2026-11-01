package diagnostics

import (
	"strconv"
	"strings"
)

// Fixed numeric counters only; never expose proc contents or filesystem paths.
func addProcessIO(values map[string]any, raw string) {
	keys := map[string]string{"read_bytes": "process_disk_read_bytes", "write_bytes": "process_disk_write_bytes", "rchar": "process_io_read_chars", "wchar": "process_io_write_chars"}
	for _, line := range strings.Split(raw, "\n") {
		name, value, found := strings.Cut(line, ":")
		key, ok := keys[name]
		if !found || !ok {
			continue
		}
		if count, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
			values[key] = count
		}
	}
}
