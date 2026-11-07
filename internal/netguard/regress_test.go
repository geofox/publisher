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
		"64:ff9b::7f00:1",     // 127.0.0.1
		"64:ff9b::a00:1",      // 10.0.0.1
		"64:ff9b::a9fe:a9fe",  // 169.254.169.254 (cloud metadata)
		"64:ff9b:1::c0a8:101", // 192.168.1.1
	} {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s is not blocked", s)
		}
	}
}

func TestIsBlockedIPReservedAndTranslated(t *testing.T) {
	for _, s := range []string{"0.1.2.3", "198.18.0.1", "240.0.0.1", "255.255.255.255",
		"64:ff9b:1:7f00:0:100:808:808", "::ffff:0:7f00:1", "2002:7f00:1::1", "2002:a00:1::1",
		"::7f00:1", "::a9fe:a9fe", "::808:808", // IPv4-compatible (::/96)
		"192.0.0.192", "168.63.129.16"} {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2002:808:808::1", "64:ff9b::808:808", "2001:4860:4860::8888"} {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s blocked", s)
		}
	}
}
