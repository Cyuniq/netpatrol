package monitor

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

func ParseTarget(address string, port int) (Target, error) {
	ip, err := ParseAddress(address)
	if err != nil {
		return Target{}, err
	}
	if port < 1 || port > 65535 {
		return Target{}, errors.New("TCP port must be between 1 and 65535")
	}
	return Target{IP: ip, Port: uint16(port)}, nil
}

func ParseAddress(address string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("IP address: %w", err)
	}
	if ip.Is4In6() && ip.Zone() != "" {
		return netip.Addr{}, errors.New("IPv4 address cannot have an interface scope")
	}
	ip = ip.Unmap()
	if ip.IsLinkLocalUnicast() && ip.Is6() && ip.Zone() == "" {
		return netip.Addr{}, errors.New("link-local IPv6 requires an interface scope")
	}
	return ip, nil
}
