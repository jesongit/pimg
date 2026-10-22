package diagnostics

import (
	"math"
	"runtime"
	"runtime/metrics"
)

// NewResourceSampler is called only by the enabled collector's sampling loop.
// Optional platform values remain absent when unavailable instead of faking 0.
func NewResourceSampler(directory string) func() map[string]any {
	return func() map[string]any {
		values := platformResources(directory)
		names := []string{"/memory/classes/heap/objects:bytes", "/gc/heap/goal:bytes", "/sched/goroutines:goroutines", "/gc/cycles/total:gc-cycles", "/sched/pauses/total/gc:seconds"}
		samples := make([]metrics.Sample, len(names))
		for i, name := range names {
			samples[i].Name = name
		}
		metrics.Read(samples)
		labels := []string{"heap_alloc_bytes", "heap_goal_bytes", "goroutines", "gc_cycles_total"}
		for i, label := range labels {
			if samples[i].Value.Kind() == metrics.KindUint64 {
				values[label] = samples[i].Value.Uint64()
			}
		}
		if samples[4].Value.Kind() == metrics.KindFloat64Histogram {
			h := samples[4].Value.Float64Histogram()
			var count uint64
			for _, v := range h.Counts {
				count += v
			}
			values["gc_pause_count_total"] = count
			var sum uint64
			for i, v := range h.Counts {
				sum += v
				if count > 0 && float64(sum) >= float64(count)*.95 {
					bound := h.Buckets[i+1]
					if !math.IsInf(bound, 0) {
						values["gc_pause_p95_ms_lifetime_estimate"] = bound * 1000
					}
					break
				}
			}
		}
		values["gomaxprocs"] = runtime.GOMAXPROCS(0)
		return values
	}
}
