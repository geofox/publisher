package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// ErrBlocked is the error for an address a policy refuses. It names no
// address: it can reach the owner, and what a name resolves to inside the
// server's network is not theirs to learn.
var ErrBlocked = errors.New("refused to connect to a private or local address")

// privateRanges are the ranges an operator may open to owner-set
// addresses (PRIVATE_DESTINATIONS): RFC 1918, ULA and carrier-grade NAT
// (Tailscale's addresses). Nothing else that IsBlockedIP refuses can be
// opened: loopback, link-local (cloud metadata), unspecified, multicast,
// reserved and translated addresses stay closed.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// Policy is who owner-set addresses (an account added in the app, a saved
// setting) may reach: public addresses, plus the private hosts and ranges
// the operator listed in PRIVATE_DESTINATIONS. The server's own addresses
// are never reached, so an owner can't point an account at Publisher
// itself. The zero value allows public addresses only.
type Policy struct {
	// Hosts are host names allowed to resolve to a private address
	// (lowercase, no trailing dot).
	Hosts []string
	// Prefixes are private ranges allowed (each inside privateRanges).
	Prefixes []netip.Prefix
	// Own are the server's own addresses, never dialed.
	Own []netip.Addr
}

// ParsePrivateDestinations reads PRIVATE_DESTINATIONS: a comma-separated
// list of host names and private CIDRs (or single addresses). An entry
// that would open loopback, link-local, unspecified or public addresses,
// or a /0, is an error.
func ParsePrivateDestinations(v string) (Policy, error) {
	var p Policy
	var errs []error
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if pre, err := netip.ParsePrefix(e); err == nil {
			// An IPv4-mapped prefix (::ffff:10.0.0.0/104) means the IPv4
			// one: addresses are compared unmapped.
			if a := pre.Addr(); a.Is4In6() && pre.Bits() >= 96 {
				pre = netip.PrefixFrom(a.Unmap(), pre.Bits()-96)
			}
			if err := checkPrivatePrefix(pre); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e, err))
				continue
			}
			p.Prefixes = append(p.Prefixes, pre.Masked())
			continue
		}
		if a, err := netip.ParseAddr(e); err == nil {
			pre := netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
			if err := checkPrivatePrefix(pre); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e, err))
				continue
			}
			p.Prefixes = append(p.Prefixes, pre)
			continue
		}
		h := strings.TrimSuffix(strings.ToLower(e), ".")
		switch {
		case !validHostName(h):
			errs = append(errs, fmt.Errorf("%s: not a host name, an address or a CIDR", e))
		case h == "localhost" || strings.HasSuffix(h, ".localhost"):
			errs = append(errs, fmt.Errorf("%s: loopback can't be opened", e))
		default:
			p.Hosts = append(p.Hosts, h)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Policy{}, fmt.Errorf("PRIVATE_DESTINATIONS: %w", err)
	}
	return p, nil
}

func checkPrivatePrefix(pre netip.Prefix) error {
	if pre.Bits() == 0 {
		return errors.New("a /0 would open everything")
	}
	for _, r := range privateRanges {
		if r.Addr().BitLen() == pre.Addr().BitLen() && r.Bits() <= pre.Bits() && r.Contains(pre.Addr()) {
			return nil
		}
	}
	return errors.New("only private ranges can be listed (10/8, 172.16/12, 192.168/16, 100.64/10, fc00::/7); loopback, link-local and public addresses can't")
}

func validHostName(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.Contains(h, "..") {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

// WithOwn returns the policy with the server's own addresses (those of
// every interface) added to Own.
func (p Policy) WithOwn() Policy {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return p
	}
	q := p
	q.Own = append([]netip.Addr(nil), p.Own...)
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				q.Own = append(q.Own, ip.Unmap())
			}
		}
	}
	return q
}

func isPrivate(a netip.Addr) bool {
	for _, r := range privateRanges {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

// hostAllowed reports whether host (as dialed: a name, or an address)
// is listed.
func (p Policy) hostAllowed(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	for _, x := range p.Hosts {
		if x == h {
			return true
		}
	}
	return false
}

func (p Policy) own(a netip.Addr) bool {
	for _, o := range p.Own {
		if o == a {
			return true
		}
	}
	return false
}

// Allow decides a connection to ip, made for host (the name or address the
// caller dialed). It is checked on the address actually connected to, so
// a name that resolves differently later is still judged right.
func (p Policy) Allow(host string, ip net.IP) error {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return ErrBlocked
	}
	a = a.Unmap()
	if p.own(a) {
		return ErrBlocked
	}
	if !IsBlockedIP(ip) {
		return nil
	}
	if !isPrivate(a) || IsMetadataAddr(a) {
		return ErrBlocked
	}
	if p.hostAllowed(host) {
		return nil
	}
	for _, pre := range p.Prefixes {
		if pre.Contains(a) {
			return nil
		}
	}
	return ErrBlocked
}

// AllowContent decides a connection to an address that came from content
// or from another server (a relay hint, a link): public addresses only,
// never the server's own.
func (p Policy) AllowContent(_ string, ip net.IP) error {
	a, ok := netip.AddrFromSlice(ip)
	if !ok || IsBlockedIP(ip) || p.own(a.Unmap()) {
		return ErrBlocked
	}
	return nil
}

// privateHost reports whether an owner-set URL's host may use a plain
// scheme (http, ws): a listed host name, or an address in a listed range.
func (p Policy) privateHost(host string) bool {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		a = a.Unmap()
		for _, pre := range p.Prefixes {
			if pre.Contains(a) {
				return true
			}
		}
		return false
	}
	return p.hostAllowed(host)
}

// URLOptions say what OwnerURL accepts.
type URLOptions struct {
	// Secure is the scheme required for a public host ("https", "wss");
	// Plain the one allowed for a listed private host ("http", "ws").
	Secure, Plain string
	// Path keeps a path (a relay at wss://host/path); without it the URL
	// must be an origin.
	Path bool
	// Query keeps the path exactly as given, trailing slash included, and
	// a query: a webhook's address, which may carry its secret in either.
	// It implies Path.
	Query bool
}

// HTTPS is an origin reached over https (http for a listed private host).
var HTTPS = URLOptions{Secure: "https", Plain: "http"}

// OwnerURL checks and normalizes an address the owner typed: the secure
// scheme, or the plain one for a host PRIVATE_DESTINATIONS lists; no user
// info, query or fragment; a host that is a name or an address the policy
// could reach (never loopback, link-local or unspecified). The result is
// the origin (scheme, lowercase host, port when not the default), plus the
// path when o.Path allows one. Names aren't resolved here: the dial is
// checked again.
func (p Policy) OwnerURL(raw string, o URLOptions) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" || u.Host == "" {
		return "", fmt.Errorf("enter a full address, such as %s://example.com", o.Secure)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case u.User != nil:
		return "", errors.New("an address can't carry a user name or password")
	case u.Fragment != "" || !o.Query && (u.RawQuery != "" || u.ForceQuery):
		return "", errors.New("an address can't have a query (?…) or fragment (#…)")
	case !o.Path && !o.Query && u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("enter the server's address only, such as %s://%s", o.Secure, u.Host)
	case scheme == o.Secure:
	case scheme == o.Plain && o.Plain != "" && p.privateHost(host):
	case scheme == o.Plain && o.Plain != "":
		return "", fmt.Errorf("use %s:// (%s:// only for a host PRIVATE_DESTINATIONS lists)", o.Secure, o.Plain)
	default:
		return "", fmt.Errorf("use %s://", o.Secure)
	}
	if host == "" {
		return "", errors.New("the address has no host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if err := p.Allow(host, net.IP(a.AsSlice())); err != nil {
			return "", errors.New("that address is private or local; Publisher can't reach it (an operator can allow a private one with PRIVATE_DESTINATIONS)")
		}
	} else if !validHostName(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("that host name isn't valid here")
	}
	port := u.Port()
	if port != "" {
		if n := portNumber(port); n < 1 || n > 65535 {
			return "", errors.New("the port isn't valid")
		}
		if (scheme == "https" || scheme == "wss") && port == "443" || (scheme == "http" || scheme == "ws") && port == "80" {
			port = ""
		}
	}
	out := scheme + "://" + hostPort(host, port)
	switch {
	case o.Query:
		out += u.EscapedPath()
		if u.RawQuery != "" {
			out += "?" + u.RawQuery
		}
	case o.Path && u.Path != "" && u.Path != "/":
		out += strings.TrimRight(u.EscapedPath(), "/")
	}
	return out, nil
}

func portNumber(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' || n > 65535 {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func hostPort(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}
