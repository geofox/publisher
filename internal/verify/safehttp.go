package verify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// maxRedirects bounds redirect chains so a hostile server can't spin us forever.
const maxRedirects = 5

// cgnatRange is RFC 6598 carrier-grade NAT space (100.64.0.0/10) — internal,
// never a legitimate public verification target.
var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// maxResponseBytes caps any verification response body (CAR files, AP objects).
const maxResponseBytes int64 = 8 << 20 // 8 MB

// isBlockedIP reports whether an IP is in a range we must never dial from a
// user-controlled URL: loopback, RFC1918/ULA private, link-local, or the
// unspecified address. This is the core SSRF guard.
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && cgnatRange.Contains(ip4) {
		return true
	}
	return false
}

// NewSafeClient returns an SSRF-guarded *http.Client that refuses
// loopback/private/link-local/CGNAT targets.
//
// The check runs in the dialer Control hook, which fires AFTER DNS resolution
// on the concrete IP about to be dialed — so it also defeats DNS rebinding (a
// hostname that resolves to a public IP first and a private IP on a later
// lookup is still checked per-connection).
func NewSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("could not parse dial address %q", host)
			}
			if isBlockedIP(ip) {
				return fmt.Errorf("blocked non-public address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
