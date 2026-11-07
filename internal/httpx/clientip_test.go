package httpx

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddrs(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	for _, tc := range []struct {
		name    string
		trusted []netip.Prefix
		peer    string
		xff     []string
		want    string
	}{
		{"no proxy trusted: the peer, whatever it forwards", nil, "203.0.113.9:5000", []string{"198.51.100.1"}, "203.0.113.9"},
		{"spoofed header from an untrusted peer", trusted, "203.0.113.9:5000", []string{"198.51.100.1"}, "203.0.113.9"},
		{"trusted proxy: the client it saw", trusted, "10.0.0.2:5000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"rightmost untrusted wins over a forged left part", trusted, "10.0.0.2:5000", []string{"1.2.3.4, 198.51.100.1"}, "198.51.100.1"},
		{"a chain of trusted proxies", trusted, "10.0.0.2:5000", []string{"198.51.100.1, 10.9.9.9", "10.0.0.3"}, "198.51.100.1"},
		{"only trusted hops: the leftmost", trusted, "10.0.0.2:5000", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
		{"garbage stops the walk at the last proxy", trusted, "10.0.0.2:5000", []string{"198.51.100.1, unknown"}, "10.0.0.2"},
		{"ports and brackets", trusted, "[fd00::1]:5000", []string{"[2001:db8::7]:443"}, "2001:db8::7"},
		{"an IPv4-mapped peer", trusted, "[::ffff:10.0.0.2]:5000", []string{"198.51.100.1:80"}, "198.51.100.1"},
		{"no header", trusted, "10.0.0.2:5000", nil, "10.0.0.2"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		c := &ClientAddrs{Trusted: tc.trusted}
		if got := c.Of(r); got != netip.MustParseAddr(tc.want) {
			t.Errorf("%s: %v, want %s", tc.name, got, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "pipe"
	if (&ClientAddrs{}).Of(r).IsValid() {
		t.Error("an unreadable peer gave an address")
	}
}
