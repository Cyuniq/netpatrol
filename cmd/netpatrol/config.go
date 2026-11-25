package main

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

type webConfig struct {
	adminPassword, noAuth, discoveryCIDRs, noAuthCIDRs string
}

func loadWebConfig(path string) (webConfig, error) {
	config := webConfig{
		adminPassword:  os.Getenv("NETPATROL_ADMIN_PASSWORD"),
		noAuth:         os.Getenv("NETPATROL_NO_AUTH"),
		discoveryCIDRs: os.Getenv("NETPATROL_DISCOVERY_CIDRS"),
		noAuthCIDRs:    os.Getenv("NETPATROL_NO_AUTH_CIDRS"),
	}
	if path == "" {
		return config, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	values := map[string]*string{
		"NETPATROL_ADMIN_PASSWORD":  &config.adminPassword,
		"NETPATROL_NO_AUTH":         &config.noAuth,
		"NETPATROL_DISCOVERY_CIDRS": &config.discoveryCIDRs,
		"NETPATROL_NO_AUTH_CIDRS":   &config.noAuthCIDRs,
	}
	lines := bufio.NewScanner(file)
	for line := 1; lines.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimPrefix(lines.Text(), "\ufeff"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		field := values[strings.TrimSpace(key)]
		if !ok || field == nil {
			return config, fmt.Errorf("配置 %s 第 %d 行无效", path, line)
		}
		*field = strings.TrimSpace(value)
	}
	return config, lines.Err()
}

func parseNoAuthSources(raw string) ([]netip.Prefix, error) {
	var sources []netip.Prefix
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool {
		return strings.ContainsRune(" ,，\r\n\t", r)
	}) {
		if address, err := netip.ParseAddr(value); err == nil && address.Zone() == "" {
			address = address.Unmap()
			sources = append(sources, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("NETPATROL_NO_AUTH_CIDRS 包含无效的来源地址或网段 %q", value)
		}
		sources = append(sources, prefix.Masked())
	}
	return sources, nil
}
