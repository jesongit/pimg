package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pimg/internal/proxytraffic"
)

const proxyTrafficKey = "storage.proxy_traffic.v1"
const proxyTrafficWait = "代理流量等待："

func (s *Service) initProxyTraffic() error {
	settings, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	state := proxytraffic.DefaultState()
	if raw := settings[proxyTrafficKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return fmt.Errorf("代理流量记录损坏: %w", err)
		}
	}
	m, err := proxytraffic.New(state, func(next proxytraffic.State) error {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return s.DB.SetSetting(proxyTrafficKey, string(b))
	})
	if err != nil {
		return err
	}
	s.proxyTraffic = m
	return nil
}

func (s *Service) ProxyTrafficView() proxytraffic.View { return s.proxyTraffic.View() }

func (s *Service) SaveProxyTraffic(p proxytraffic.Patch) error {
	if err := s.proxyTraffic.Update(p); err != nil {
		return err
	}
	return s.RefreshProxyTrafficTasks()
}

// Polling also runs without an open settings page, including after restarts.
func (s *Service) RefreshProxyTrafficTasks() error {
	v := s.proxyTraffic.View()
	if v.Blocked {
		return nil
	}
	return s.wakeProxyTrafficTasks()
}

func (s *Service) wakeProxyTrafficTasks() error {
	_, err := s.DB.Exec(`UPDATE tasks SET run_after=unixepoch(),last_error=NULL,updated_at=unixepoch() WHERE state='pending' AND last_error LIKE ?`, proxyTrafficWait+"%")
	return err
}

func (s *Service) PostponeProxyTrafficTask(id int64) error {
	v := s.proxyTraffic.View()
	after := v.ResumeAt
	if after <= time.Now().Unix() {
		after = time.Now().Add(2 * time.Second).Unix()
	}
	_, err := s.DB.Exec(`UPDATE tasks SET state=CASE WHEN cancel_requested=1 THEN 'canceled' ELSE 'pending' END,attempts=MAX(0,attempts-1),run_after=?,lease_until=NULL,last_error=?,updated_at=unixepoch() WHERE id=?`, after, proxyTrafficWait+"额度恢复后自动继续", id)
	return err
}

func (s *Service) setArchiveFailure(ctx context.Context, id string, err error) error {
	if errors.Is(err, proxytraffic.ErrLimit) || errors.Is(context.Cause(ctx), proxytraffic.ErrLimit) || proxytraffic.PauseError(ctx) != nil {
		return s.DB.SetArchiveState(id, "pending", proxyTrafficWait+"额度恢复后自动继续")
	}
	return s.DB.SetArchiveState(id, "failed", err.Error())
}
