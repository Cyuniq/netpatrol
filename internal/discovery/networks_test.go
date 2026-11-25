package discovery

import (
	"net/netip"
	"testing"
)

func TestDefaultDiscoveryScope(t *testing.T) {
	networks := []netip.Prefix{netip.MustParsePrefix("10.25.6.0/24")}
	sameLAN := Access{Peer: netip.MustParseAddr("10.25.6.9")}
	public := Access{Peer: netip.MustParseAddr("203.0.113.2")}
	tests := []struct {
		target string
		access Access
		want   bool
	}{
		{"10.25.6.0/28", sameLAN, true},
		{"10.25.7.0/24", sameLAN, false},
		{"10.25.6.0/24", public, false},
		{"10.25.6.0/24", Access{Local: true}, true},
		{"10.25.6.0/23", Access{Local: true}, false},
		{"198.51.100.0/24", Access{Local: true}, false},
	}
	for _, test := range tests {
		if got := directDiscovery(networks, netip.MustParsePrefix(test.target), test.access); got != test.want {
			t.Errorf("directDiscovery(%s, %+v) = %v, want %v", test.target, test.access, got, test.want)
		}
	}
}
