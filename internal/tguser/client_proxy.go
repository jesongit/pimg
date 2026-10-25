package tguser

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pimg/internal/proxytraffic"

	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

// A nil dialer preserves the SDK's default resolver when no proxy is set.
func clientProxyDial(explicit string, meters ...*proxytraffic.Meter) (dcs.DialFunc, error) {
	if strings.TrimSpace(explicit) != "" {
		return proxyDial(explicit, meters...)
	}
	var direct net.Dialer
	return environmentProxyDial(httpproxy.FromEnvironment(), direct.DialContext, meters...)
}

func environmentProxyDial(cfg *httpproxy.Config, direct dcs.DialFunc, meters ...*proxytraffic.Meter) (dcs.DialFunc, error) {
	// MTProto has no HTTP URL scheme. Prefer the HTTPS proxy for its encrypted
	// TCP connection, falling back to HTTP_PROXY when HTTPS_PROXY is unset.
	scheme, raw := "https", cfg.HTTPSProxy
	if strings.TrimSpace(raw) == "" {
		scheme, raw = "http", cfg.HTTPProxy
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	// Match httpproxy's host[:port] shorthand, but fail closed if a configured
	// proxy is malformed instead of letting its best-effort parsing bypass it.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u, err = url.Parse("http://" + raw)
	}
	if err != nil {
		return nil, errors.New("Telegram 环境代理地址无效")
	}
	dial, err := proxyDial(u.String(), meters...)
	if err != nil {
		return nil, err
	}
	selectProxy := cfg.ProxyFunc()
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		selected, err := selectProxy(&url.URL{Scheme: scheme, Host: target})
		if err != nil {
			return nil, errors.New("Telegram 环境代理配置不可用")
		}
		if selected == nil {
			return direct(ctx, network, target)
		}
		return dial(ctx, network, target)
	}, nil
}

func proxyDial(raw string, meters ...*proxytraffic.Meter) (dcs.DialFunc, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("Telegram 代理地址无效")
	}
	scheme := strings.ToLower(u.Scheme)
	d := &meteredDialer{}
	if len(meters) > 0 {
		d.meter = meters[0]
	}
	switch scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		port := u.Port()
		if port == "" {
			port = "1080"
		}
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(u.Hostname(), port), auth, d)
		if err != nil {
			return nil, errors.New("Telegram SOCKS5 代理配置无效")
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("Telegram SOCKS5 代理不支持取消")
		}
		return contextDialer.DialContext, nil
	case "http", "https":
		port := u.Port()
		if port == "" {
			port = "80"
			if scheme == "https" {
				port = "443"
			}
		}
		address := net.JoinHostPort(u.Hostname(), port)
		return func(ctx context.Context, network, target string) (net.Conn, error) {
			conn, err := d.DialContext(ctx, "tcp", address)
			if err != nil {
				if errors.Is(err, proxytraffic.ErrLimit) {
					return nil, err
				}
				return nil, errors.New("Telegram 代理连接失败")
			}
			rawConn := conn
			closeOnCancel := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
			defer closeOnCancel()
			ok := false
			defer func() {
				if !ok {
					_ = conn.Close()
				}
			}()
			if deadline, has := ctx.Deadline(); has {
				_ = conn.SetDeadline(deadline)
			} else {
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			}
			if scheme == "https" {
				secure := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
				if err := secure.HandshakeContext(ctx); err != nil {
					return nil, errors.New("Telegram 代理 TLS 连接失败")
				}
				conn = secure
			}
			req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
			if u.User != nil {
				password, _ := u.User.Password()
				req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
			}
			if err := req.Write(conn); err != nil {
				return nil, errors.New("Telegram 代理请求失败")
			}
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, req)
			if err != nil {
				return nil, errors.New("Telegram 代理响应无效")
			}
			if response.StatusCode != http.StatusOK {
				return nil, errors.New("Telegram 代理拒绝连接")
			}
			_ = conn.SetDeadline(time.Time{})
			ok = true
			if reader.Buffered() > 0 {
				return &bufferedConn{Conn: conn, reader: reader}, nil
			}
			return conn, nil
		}, nil
	default:
		return nil, errors.New("Telegram 代理仅支持 http、https 或 socks5")
	}
}

type meteredDialer struct {
	net.Dialer
	meter *proxytraffic.Meter
}

func (d *meteredDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d *meteredDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := proxytraffic.Mark(ctx, d.meter); err != nil {
		return nil, err
	}
	c, err := d.Dialer.DialContext(ctx, network, address)
	if err == nil {
		c = d.meter.Wrap(c)
	}
	return c, err
}

// Resolve the environment for the Telegram target, honoring NO_PROXY just as
// the actual dialer does. Used to fail RPCs promptly while the socket is paused.
func (c *Client) proxyTrafficError() error {
	if c.opts.ProxyTraffic == nil {
		return nil
	}
	if strings.TrimSpace(c.opts.ProxyURL) == "" && !c.proxySeen.Load() {
		return nil
	}
	return c.opts.ProxyTraffic.Check()
}

func (c *Client) ProxyTrafficError() error { return c.proxyTrafficError() }

func proxySelected(explicit, target string) bool {
	if strings.TrimSpace(explicit) != "" {
		return true
	}
	cfg := httpproxy.FromEnvironment()
	scheme := "https"
	if strings.TrimSpace(cfg.HTTPSProxy) == "" {
		scheme = "http"
	}
	u, err := cfg.ProxyFunc()(&url.URL{Scheme: scheme, Host: target})
	return err == nil && u != nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
