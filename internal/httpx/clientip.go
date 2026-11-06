package httpx

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// ClientAddrs finds the address of the client behind a request, believing
// X-Forwarded-For only from the reverse proxies in Trusted
// (TRUSTED_PROXIES).
type ClientAddrs struct {
	Trusted []netip.Prefix
	warned  atomic.Bool
}

// Of is the client's address: the connection's peer, unless the peer is a
// trusted proxy; then the rightmost X-Forwarded-For address that isn't
// one (each proxy appends the address it received from, so everything to
// the left of the first untrusted hop could be forged). The zero Addr when
// the peer can't be read.
//
// When no proxy is trusted and a private or loopback peer forwards
// X-Forwarded-For, every client would share the proxy's address (one
// throttling bucket for all): that is logged once.
func (c *ClientAddrs) Of(r *http.Request) netip.Addr {
	peer := remoteAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return peer
	}
	if len(c.Trusted) == 0 {
		if len(r.Header.Values("X-Forwarded-For")) > 0 && (peer.IsPrivate() || peer.IsLoopback()) && c.warned.CompareAndSwap(false, true) {
			slog.Warn("requests come through a proxy (X-Forwarded-For) but TRUSTED_PROXIES is unset: every client shares the proxy's address for sign-in throttling; set TRUSTED_PROXIES to the proxy's address",
				"proxy", peer.String())
		}
		return peer
	}
	if !c.trusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a := forwardedAddr(strings.TrimSpace(hops[i]))
		if !a.IsValid() {
			// Not an address: nothing further left can be believed; the
			// last proxy that spoke is as far as we know.
			return client
		}
		client = a
		if !c.trusted(a) {
			return a
		}
	}
	return client
}

func (c *ClientAddrs) trusted(a netip.Addr) bool {
	for _, p := range c.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// remoteAddr parses a connection's "host:port".
func remoteAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// forwardedAddr parses one X-Forwarded-For entry: an address, possibly
// with a port ("192.0.2.1:4711", "[2001:db8::1]:4711").
func forwardedAddr(s string) netip.Addr {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone("")
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return netip.Addr{}
}
