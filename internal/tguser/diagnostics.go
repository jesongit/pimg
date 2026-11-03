package tguser

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tgerr"
	"golang.org/x/net/http/httpproxy"
	"pimg/internal/proxytraffic"
)

// All text below is a fixed label or a validated protocol identifier. Never
// retain upstream error strings, addresses, RPC inputs or proxy credentials.
type ConnectionFailure struct {
	Time       int64  `json:"time"`
	Stage      string `json:"stage"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type ConnectionDiagnostics struct {
	Running         bool                `json:"running"`
	Connected       bool                `json:"connected"`
	TransportState  string              `json:"transport_state"`
	ConnectingSince int64               `json:"connecting_since,omitempty"`
	LastReadyAt     int64               `json:"last_ready_at,omitempty"`
	LastDialAt      int64               `json:"last_dial_at,omitempty"`
	LastDialMS      int64               `json:"last_dial_ms"`
	DialAttempts    int64               `json:"dial_attempts"`
	DialSuccesses   int64               `json:"dial_successes"`
	Disconnects     int64               `json:"disconnects"`
	InFlightDials   int64               `json:"in_flight_dials"`
	ProxyMode       string              `json:"proxy_mode"`
	ProxyScheme     string              `json:"proxy_scheme,omitempty"`
	LastFailure     *ConnectionFailure  `json:"last_failure,omitempty"`
	RecentFailures  []ConnectionFailure `json:"recent_failures"`
}

// transportFailure preserves the cause for errors.Is/As without making its
// potentially credential-bearing Error() text visible to callers or logs.
type transportFailure struct {
	stage, message string
	status         int
	cause          error
}

func (e *transportFailure) Error() string { return e.message }
func (e *transportFailure) Unwrap() error { return e.cause }

func failureDetails(stage string, err error) ConnectionFailure {
	f := ConnectionFailure{Time: time.Now().UnixMilli(), Stage: stage, Kind: "transport_error", Message: "Telegram 连接或协议请求失败"}
	if stage == "proxy_configuration" {
		f.Kind, f.Message = "proxy_configuration", "Telegram 代理配置无效，请检查代理设置"
		return f
	}
	var transport *transportFailure
	if errors.As(err, &transport) {
		f.Stage, f.HTTPStatus = transport.stage, transport.status
		if transport.status != 0 {
			f.Kind, f.Message = "proxy_rejected", "代理拒绝建立 Telegram 隧道"
			if transport.status == 407 {
				f.Kind, f.Message = "proxy_auth_required", "代理认证失败，请检查代理账号和密码"
			}
			return f
		}
	}
	var dns *net.DNSError
	var network net.Error
	var certificate *tls.CertificateVerificationError
	switch {
	case errors.Is(err, proxytraffic.ErrLimit):
		f.Kind, f.Message = "proxy_quota", "代理流量已达上限，等待额度恢复"
	case errors.Is(err, errSessionInvalid), errors.Is(err, errSessionKey):
		f.Stage, f.Kind, f.Message = "session", "session_invalid", "本地 Telegram 会话无法解密或格式无效"
	case errors.Is(err, errSessionRead), errors.Is(err, errSessionWrite), errors.Is(err, errSessionPath):
		f.Stage, f.Kind, f.Message = "session", "session_storage", "本地 Telegram 会话存储不可用，请检查权限"
	case errors.Is(err, ErrNotAuthorized):
		f.Stage, f.Kind, f.Message = "authorization", "session_unauthorized", "Telegram 会话需要重新授权"
	case errors.Is(err, ErrAccountMismatch):
		f.Stage, f.Kind, f.Message = "authorization", "account_mismatch", "当前 Telegram 账号与归档账号不一致"
	case errors.As(err, &dns):
		f.Kind, f.Message = "dns", "Telegram 或代理地址解析失败"
	case errors.As(err, &certificate):
		f.Kind, f.Message = "tls_certificate", "代理 TLS 证书验证失败"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		f.Kind, f.Message = "timeout", "Telegram 连接或请求超时"
	case errors.Is(err, syscall.ECONNREFUSED):
		f.Kind, f.Message = "connection_refused", "Telegram 或代理拒绝 TCP 连接"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		f.Kind, f.Message = "network_unreachable", "Telegram 或代理网络不可达"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		f.Kind, f.Message = "connection_closed", "Telegram 连接被关闭"
	default:
		if rpc, ok := tgerr.As(err); ok && safeProtocolIdentifier(rpc.Type) {
			f.Stage, f.Kind, f.Message = "authorization", rpc.Type, "Telegram 返回协议错误："+rpc.Type
		}
	}
	return f
}

func safeProtocolIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func (c *Client) connectionFailure(stage string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	f := failureDetails(stage, err)
	c.mu.Lock()
	// OnDead may repeat the same dial failure. Keep the original TCP/proxy
	// stage rather than replacing it with a less specific MTProto wrapper.
	if last := c.connection.LastFailure; stage == "mtproto" && last != nil && f.Time-last.Time < 1000 && f.Kind == last.Kind {
		c.mu.Unlock()
		return
	}
	c.connection.LastFailure = &f
	c.connection.RecentFailures = append(c.connection.RecentFailures, f)
	if len(c.connection.RecentFailures) > 16 {
		c.connection.RecentFailures = append([]ConnectionFailure(nil), c.connection.RecentFailures[len(c.connection.RecentFailures)-16:]...)
	}
	if c.status.State == "connecting" {
		c.status.Error = f.Message
	}
	// The retry loop can hit the same failure many times per minute (a blocked
	// proxy quota above all). Diagnostics keep every occurrence; the runtime
	// log records only the first of a kind or one line per ten minutes, so a
	// stuck connection cannot flood the log ring and database writes.
	log := c.loggedKind != f.Kind || time.Since(c.loggedAt) >= 10*time.Minute
	if log {
		c.loggedKind, c.loggedAt = f.Kind, time.Now()
	}
	c.mu.Unlock()
	if log && c.opts.Logger != nil {
		c.opts.Logger.Warn("telegram_connection_failure", "stage", f.Stage, "error_type", f.Kind, "http_status", f.HTTPStatus)
	}
}

func proxyRoute(explicit, target string) (mode, scheme string) {
	var selected *url.URL
	if strings.TrimSpace(explicit) != "" {
		mode = "configured"
		selected, _ = url.Parse(explicit)
	} else {
		cfg := httpproxy.FromEnvironment()
		protocol := "https"
		if strings.TrimSpace(cfg.HTTPSProxy) == "" {
			protocol = "http"
		}
		selected, _ = cfg.ProxyFunc()(&url.URL{Scheme: protocol, Host: target})
		if selected != nil {
			mode = "environment"
		} else {
			mode = "direct"
		}
	}
	if selected != nil {
		switch selected.Scheme {
		case "http", "https", "socks5", "socks5h":
			scheme = selected.Scheme
		default:
			scheme = "invalid"
		}
	}
	return mode, scheme
}

func (c *Client) diagnosticDial(dial dcs.DialFunc) dcs.DialFunc {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		started := time.Now()
		mode, scheme := proxyRoute(c.opts.ProxyURL, target)
		c.mu.Lock()
		c.connection.DialAttempts++
		c.connection.InFlightDials++
		c.connection.LastDialAt = started.UnixMilli()
		c.connection.ProxyMode, c.connection.ProxyScheme = mode, scheme
		c.mu.Unlock()
		if mode != "direct" {
			c.proxySeen.Store(true)
		}
		conn, err := dial(ctx, network, target)
		c.mu.Lock()
		c.connection.LastDialMS = time.Since(started).Milliseconds()
		c.connection.InFlightDials--
		if err == nil {
			c.connection.DialSuccesses++
		}
		c.mu.Unlock()
		if err != nil {
			stage := "tcp_connect"
			if mode != "direct" {
				stage = "proxy_connect"
			}
			c.connectionFailure(stage, err)
		}
		return conn, err
	}
}

func (c *Client) Diagnostics() ConnectionDiagnostics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d := c.connection
	d.Running, d.Connected = c.running, c.connected
	d.RecentFailures = append([]ConnectionFailure{}, d.RecentFailures...)
	if d.LastFailure != nil {
		value := *d.LastFailure
		d.LastFailure = &value
	}
	return d
}
