package diagnostics

import "testing"

func TestProcessIOKeepsOnlyFixedNumericCounters(t *testing.T) {
	v:=map[string]any{}
	addProcessIO(v,"read_bytes: 123\nwrite_bytes: 456\nrchar: 789\nwchar: 12\nprivate_path: /private/file\nread_bytes: invalid\ncancelled_write_bytes: -1\n")
	if len(v)!=4 || v["process_disk_read_bytes"]!=uint64(123) || v["process_disk_write_bytes"]!=uint64(456) {t.Fatal(v)}
}
