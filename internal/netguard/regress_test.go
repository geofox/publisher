package netguard

import (
	"net"
	"testing"
)

// NAT64 addresses (RFC 6052 well-known prefix, RFC 8215 local-use prefix)
// embed an IPv4 address: on an IPv6-only/NAT64 network, dialing them reaches
// that IPv4 host, including RFC 1918 and link-local ones.
func TestNAT64Bypass(t *testing.T) {
	for _, s := range []string{
		"64:ff9b::7f00:1",    // 127.0.0.1
		"64:ff9b::a00:1",     // 10.0.0.1
		"64:ff9b::a9fe:a9fe", // 169.254.169.254 (cloud metadata)
		"64:ff9b:1::c0a8:101", // 192.168.1.1
	} {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s is not blocked", s)
		}
	}
}
