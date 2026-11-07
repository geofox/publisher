package netguard

import (
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestParsePrivateDestinations(t *testing.T) {
	p, err := ParsePrivateDestinations(" mastodon.lan , 10.0.0.0/8, 192.168.1.20, fd12::/16, ::ffff:172.16.0.0/108, Pds.Home.Arpa. ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Hosts, ",") != "mastodon.lan,pds.home.arpa" {
		t.Errorf("hosts %v", p.Hosts)
	}
	want := []string{"10.0.0.0/8", "192.168.1.20/32", "fd12::/16", "172.16.0.0/12"}
	if len(p.Prefixes) != len(want) {
		t.Fatalf("prefixes %v", p.Prefixes)
	}
	for i, w := range want {
		if p.Prefixes[i].String() != w {
			t.Errorf("prefix %d = %s, want %s", i, p.Prefixes[i], w)
		}
	}
	if p, err := ParsePrivateDestinations(""); err != nil || len(p.Hosts)+len(p.Prefixes) != 0 {
		t.Errorf("empty: %+v %v", p, err)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "127.0.0.0/8", "127.0.0.1", "::1", "169.254.0.0/16", "169.254.169.254",
		"fe80::/10", "0.0.0.0", "8.8.8.0/24", "10.0.0.0/7", "localhost", "a.localhost", "bad host!", "224.0.0.1"} {
		if _, err := ParsePrivateDestinations(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if !strings.HasPrefix(err.Error(), "PRIVATE_DESTINATIONS: ") {
			t.Errorf("%q: error doesn't name the variable: %v", bad, err)
		}
	}
}

func ip(s string) net.IP { return net.ParseIP(s) }

// Loopback, link-local (metadata), unspecified, multicast, reserved and
// translated addresses are refused whatever is listed; a private address
// only when its host or range is listed; the server's own never.
func TestPolicyAllow(t *testing.T) {
	p, err := ParsePrivateDestinations("mastodon.lan,10.1.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	p.Own = []netip.Addr{netip.MustParseAddr("10.1.0.5"), netip.MustParseAddr("203.0.113.9")}
	cases := []struct {
		host, ip string
		ok       bool
	}{
		{"pub.example", "93.184.216.34", true},
		{"pub.example", "2001:4860:4860::8888", true},
		{"pub.example", "10.9.9.9", false},          // private, not listed
		{"pub.example", "10.1.2.3", true},           // listed range
		{"mastodon.lan", "192.168.7.7", true},       // listed host, any private address
		{"MASTODON.LAN.", "192.168.7.7", true},      // as dialed, case and dot
		{"mastodon.lan", "127.0.0.1", false},        // never loopback
		{"mastodon.lan", "169.254.169.254", false},  // never metadata
		{"mastodon.lan", "::ffff:127.0.0.1", false}, // mapped loopback
		{"mastodon.lan", "::ffff:169.254.169.254", false},
		{"pub.example", "::ffff:10.1.2.3", true}, // mapped, listed
		{"pub.example", "::ffff:10.9.9.9", false},
		{"mastodon.lan", "0.0.0.0", false},
		{"mastodon.lan", "::", false},
		{"mastodon.lan", "224.0.0.1", false},
		{"mastodon.lan", "64:ff9b::a00:1", false},  // NAT64 of 10.0.0.1
		{"mastodon.lan", "2002:a01:203::1", false}, // 6to4 of 10.1.2.3
		{"pub.example", "10.1.0.5", false},         // the server itself
		{"pub.example", "203.0.113.9", false},      // its public address too
		{"pub.example", "fe80::1", false},
	}
	for _, c := range cases {
		if err := p.Allow(c.host, ip(c.ip)); (err == nil) != c.ok {
			t.Errorf("Allow(%s, %s) = %v, want ok=%v", c.host, c.ip, err, c.ok)
		}
	}
	// Content gets no carve-out.
	for _, c := range []struct {
		ip string
		ok bool
	}{{"93.184.216.34", true}, {"10.1.2.3", false}, {"192.168.7.7", false}, {"203.0.113.9", false}, {"::ffff:127.0.0.1", false}} {
		if err := p.AllowContent("mastodon.lan", ip(c.ip)); (err == nil) != c.ok {
			t.Errorf("AllowContent(%s) = %v, want ok=%v", c.ip, err, c.ok)
		}
	}
	if (Policy{}).Allow("x", nil) == nil {
		t.Error("nil IP allowed")
	}
}

// A listed range never opens a cloud's metadata address inside it.
func TestPolicyAllowNeverMetadata(t *testing.T) {
	p, err := ParsePrivateDestinations("meta.lan,100.64.0.0/10,fc00::/7")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, ip string
		ok       bool
	}{
		{"pub.example", "100.100.100.200", false}, // Alibaba Cloud
		{"meta.lan", "100.100.100.200", false},
		{"pub.example", "fd00:ec2::254", false}, // AWS, IPv6
		{"meta.lan", "fd00:ec2::254", false},
		{"pub.example", "100.100.100.201", true}, // the range still works
		{"pub.example", "fd00:ec2::253", true},
	} {
		if err := p.Allow(c.host, ip(c.ip)); (err == nil) != c.ok {
			t.Errorf("Allow(%s, %s) = %v, want ok=%v", c.host, c.ip, err, c.ok)
		}
	}
}

func TestOwnerURL(t *testing.T) {
	p, _ := ParsePrivateDestinations("mastodon.lan,10.1.0.0/16")
	ok := map[string]string{
		"https://Mastodon.Social":            "https://mastodon.social",
		"https://mastodon.social/":           "https://mastodon.social",
		"  https://mastodon.social:443  ":    "https://mastodon.social",
		"https://m.example:8443":             "https://m.example:8443",
		"http://mastodon.lan:8080":           "http://mastodon.lan:8080",
		"http://10.1.2.3":                    "http://10.1.2.3",
		"https://10.1.2.3":                   "https://10.1.2.3",
		"https://93.184.216.34":              "https://93.184.216.34",
		"https://[2001:4860:4860::8888]:443": "https://[2001:4860:4860::8888]",
		"https://mastodon.social.":           "https://mastodon.social",
	}
	for in, want := range ok {
		got, err := p.OwnerURL(in, HTTPS)
		if err != nil || got != want {
			t.Errorf("%q = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://mastodon.social", "ftp://x.example", "https://user:pw@x.example", "https://x.example?a=1",
		"https://x.example#f", "https://x.example/path", "https://127.0.0.1", "https://[::1]", "https://169.254.169.254",
		"https://localhost", "https://a.localhost", "https://10.9.9.9", "http://10.9.9.9", "https://[::ffff:127.0.0.1]",
		"mastodon.social", "https://", "https://x.example:0", "https://x.example:99999", "https://x_y!.example", "javascript:alert(1)"} {
		if got, err := p.OwnerURL(bad, HTTPS); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	ws := URLOptions{Secure: "wss", Plain: "ws", Path: true}
	for in, want := range map[string]string{"wss://relay.example": "wss://relay.example", "wss://relay.example/nostr/": "wss://relay.example/nostr",
		"ws://mastodon.lan:7777": "ws://mastodon.lan:7777", "WSS://Relay.Example:443/a": "wss://relay.example/a"} {
		if got, err := p.OwnerURL(in, ws); err != nil || got != want {
			t.Errorf("relay %q = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ws://relay.example", "https://relay.example", "wss://relay.example/?x"} {
		if _, err := p.OwnerURL(bad, ws); err == nil {
			t.Errorf("relay %q accepted", bad)
		}
	}
}
