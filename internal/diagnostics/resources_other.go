//go:build !linux && !windows

package diagnostics

func platformResources(directory string) map[string]any {
	return map[string]any{"platform_resource_metrics_available": false}
}
