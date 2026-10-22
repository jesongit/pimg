// Package storagehttp configures outbound storage connections without changing
// the process-wide transport or the HTTP server used by visitors.
package storagehttp

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
