// Package storagehttp configures outbound storage connections without changing
// the process-wide transport or the HTTP server used by visitors.
package storagehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"pimg/internal/proxytraffic"
)

// ParseProxyURL validates explicit storage proxy settings. Errors deliberately
// omit the supplied URL because its userinfo can contain credentials.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("storage.proxy_url 需要有效的 http://、https://、socks5:// 或 socks5h:// 代理地址（不能含路径、查询或片段）")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, invalid
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, invalid
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	return u, nil
}

// NewTransport preserves environment proxy behavior when no explicit proxy is
// set. An explicit proxy applies to every storage request, including hosts that
// would otherwise be excluded by NO_PROXY. Failed proxies never fall back direct.
func NewTransport(raw string) (*http.Transport, error) {
	proxyURL, err := ParseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return transport, nil
}

// MeteredTransport keeps separate pools for proxy and direct requests, so an
// environment NO_PROXY match can never inherit a metered proxy connection.
type MeteredTransport struct {
	proxy, direct *http.Transport
	meter         *proxytraffic.Meter
}

func NewMeteredTransport(raw string, meter *proxytraffic.Meter) (*MeteredTransport, error) {
	t, err := NewTransport(raw)
	if err != nil {
		return nil, err
	}
	direct := t.Clone()
	direct.Proxy = nil
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := meter.Check(); err != nil {
			return nil, err
		}
		c, err := dial(ctx, network, address)
		if err == nil {
			c = meter.Wrap(c)
		}
		return c, err
	}
	return &MeteredTransport{proxy: t, direct: direct, meter: meter}, nil
}

func (t *MeteredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := t.proxy.Proxy(r)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return t.direct.RoundTrip(r)
	}
	if err := proxytraffic.Mark(r.Context(), t.meter); err != nil {
		return nil, err
	}
	response, err := t.proxy.RoundTrip(r)
	if err != nil {
		if paused := t.meter.Check(); paused != nil {
			return nil, paused
		}
	}
	return response, err
}

func (t *MeteredTransport) CloseIdleConnections() {
	t.proxy.CloseIdleConnections()
	t.direct.CloseIdleConnections()
}
