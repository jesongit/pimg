package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// Diagnostics is deployment-local. Never restore network trust from a migrated
// startup.json; the target machine must configure its own allowlist explicitly.
type Diagnostics struct {
	AllowIPs       []string `yaml:"allow_ips"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}

func ParseIPRanges(values []string) ([]netip.Prefix, error) {
	if len(values) > 256 {
		return nil, fmt.Errorf("IP 白名单最多 256 项")
	}
	var out []netip.Prefix
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		p, err := netip.ParsePrefix(value)
		if err != nil {
			a, e := netip.ParseAddr(value)
			if e != nil || a.Zone() != "" {
				return nil, fmt.Errorf("无效 IP 或 CIDR: %q", value)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		} else if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("无效 IPv4 映射网段: %q", value)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
