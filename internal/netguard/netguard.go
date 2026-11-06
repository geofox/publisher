// Package netguard holds the SSRF checks shared by every place that dials a
// host named by someone else (a post's media, a Nostr relay hint, a
// verification target).
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// cgnatRange is RFC 6598 carrier-grade NAT space (100.64.0.0/10): internal,
// never a public host.
var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// IsBlockedIP reports whether ip is in a range we must never dial for a
// user-supplied address: loopback, private (RFC 1918, ULA), link-local,
// CGNAT, multicast or unspecified.
func IsBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && cgnatRange.Contains(ip4) {
		return true
	}
	// NAT64 (RFC 6052 well-known and RFC 8215 local-use prefixes): on an
	// IPv6-only host the gateway turns these into the IPv4 address in their
	// last 4 bytes, so judge that address.
	if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil && (nat64WKP.Contains(ip16) || nat64Local.Contains(ip16)) {
		return IsBlockedIP(net.IP(ip16[12:16]))
	}
	return false
}

var (
	nat64WKP   = &net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}
	nat64Local = &net.IPNet{IP: net.ParseIP("64:ff9b:1::"), Mask: net.CIDRMask(48, 128)}
)

// Resolver is the lookup CheckPublicURL uses (net.DefaultResolver in
// production).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// CheckPublicURL checks that u uses one of schemes and that its host is a
// public address: an IP literal outside the blocked ranges, or a name whose
// every address is. A name can still resolve differently when it is dialed
// later, so prefer a dial-time check where the client allows one.
func CheckPublicURL(ctx context.Context, r Resolver, raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad url")
	}
	ok := false
	for _, s := range schemes {
		ok = ok || strings.EqualFold(u.Scheme, s)
	}
	if !ok {
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("no host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return fmt.Errorf("non-public address")
		}
		return nil
	}
	lh := strings.ToLower(strings.TrimSuffix(host, "."))
	if lh == "localhost" || strings.HasSuffix(lh, ".localhost") || strings.HasSuffix(lh, ".local") ||
		strings.HasSuffix(lh, ".internal") || !strings.Contains(lh, ".") {
		return fmt.Errorf("non-public host")
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("lookup failed")
	}
	if len(addrs) == 0 {
		return fmt.Errorf("no address")
	}
	for _, a := range addrs {
		if IsBlockedIP(a.IP) {
			return fmt.Errorf("non-public address")
		}
	}
	return nil
}
