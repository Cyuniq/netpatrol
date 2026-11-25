package discovery

import (
	"net"
	"net/netip"
)

type Access struct {
	Peer    netip.Addr
	Local   bool
	Trusted bool
}

var privateIPv4 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func connectedIPv4() []netip.Prefix {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var networks []netip.Prefix
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Is4() && contained(privateIPv4, prefix.Masked()) {
			networks = append(networks, prefix.Masked())
		}
	}
	return networks
}

func directDiscovery(networks []netip.Prefix, target netip.Prefix, access Access) bool {
	for _, network := range networks {
		if network.Bits() <= target.Bits() && network.Contains(target.Addr()) &&
			(access.Local || network.Contains(access.Peer)) {
			return true
		}
	}
	return false
}
