package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"netpatrol/internal/discovery"
)

type discoveryAccessKey struct{}

func directRequestAccess(r *http.Request) discovery.Access {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		if len(r.Header.Values(name)) != 0 {
			return discovery.Access{}
		}
	}
	remote, _ := netip.ParseAddrPort(r.RemoteAddr)
	peer := remote.Addr().Unmap()
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.Trim(r.Host, "[]")
	}
	ip, _ := netip.ParseAddr(host)
	local := strings.EqualFold(host, "localhost") || ip.Unmap().IsLoopback()
	return discovery.Access{Peer: peer, Local: peer.IsLoopback() && local}
}

func requestAllowsNoAuth(access discovery.Access, sources []netip.Prefix) bool {
	for _, prefix := range sources {
		if prefix.Contains(access.Peer) {
			return true
		}
	}
	return false
}

func withDiscoveryAccess(r *http.Request, access discovery.Access) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), discoveryAccessKey{}, access))
}

func discoveryAccess(r *http.Request) discovery.Access {
	access, _ := r.Context().Value(discoveryAccessKey{}).(discovery.Access)
	return access
}
