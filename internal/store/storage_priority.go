package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

type StorageHealth struct {
	Available      *bool   `json:"available"`
	LatencyMS      float64 `json:"latency_ms"`
	BytesPerSecond float64 `json:"bytes_per_second"`
	CheckedAt      int64   `json:"checked_at"`
	Error          string  `json:"error,omitempty"`
}

// The selected publishing/temporary instance remains the first choice.
// Other eligible instances are fallbacks, ordered by health, priority and
// observed transfer speed. Probes never mutate the user's selection.
func (s *Service) hotCandidates(role, preferred string) []string {
	s.storages.mu.RLock()
	defer s.storages.mu.RUnlock()
	var out []string
	for id, i := range s.storages.instances {
		if !i.row.Enabled || i.hot == nil {
			continue
		}
		if role == "permanent" && i.hot.PublicURL(".capability") == "" {
			continue
		}
		if role == "temporary" && id == "r2" && i.config.Bootstrap {
			continue
		}
		out = append(out, id)
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := s.storages.scores[out[a]], s.storages.scores[out[b]]
		xbad, ybad := x.available != nil && !*x.available, y.available != nil && !*y.available
		if xbad != ybad {
			return !xbad
		}
		if out[a] == preferred {
			return true
		}
		if out[b] == preferred {
			return false
		}
		return s.storageLessLocked(out[a], out[b])
	})
	return out
}

func (s *Service) restoreStorageHealth() error {
	rows, err := s.DB.Query(`SELECT instance_id,available,latency_ms,bytes_per_second,checked_at,error FROM storage_health`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.storages.mu.Lock()
	defer s.storages.mu.Unlock()
	for rows.Next() {
		var id string
		var available *bool
		var v sourceScore
		var at int64
		if err = rows.Scan(&id, &available, &v.latencyMS, &v.bytesPerSecond, &at, &v.lastError); err != nil {
			return err
		}
		v.available = available
		v.at = time.Unix(at, 0)
		// Older versions persisted interrupted requests as outages. Treat only
		// those canceled observations as unknown until the next completed check;
		// do not rewrite historical rows or claim that the provider is healthy.
		if v.lastError == context.Canceled.Error() || strings.HasSuffix(v.lastError, ": "+context.Canceled.Error()) {
			v.available = nil
			v.lastError = ""
			v.at = time.Unix(0, 0)
		}
		s.storages.scores[id] = v
	}
	return rows.Err()
}

func (s *Service) checkStorage(ctx context.Context, id string) error {
	i, err := s.instance(id)
	if err != nil {
		return err
	}
	if i.cold != nil {
		return i.cold.Check(ctx)
	}
	if checker, ok := i.hot.(interface{ Check(context.Context) error }); ok {
		return checker.Check(ctx)
	}
	// Hot adapters also expose a cold view in the built-in drivers. Unknown
	// adapters keep an unknown state rather than reporting a fake successful probe.
	return errors.New("存储尚未配置或不支持可用性检测")
}
