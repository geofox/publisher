package verify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/geofox/publisher/internal/netguard"
)

// maxRedirects bounds redirect chains so a hostile server can't spin us forever.
const maxRedirects = 5

// maxResponseBytes caps any verification response body (CAR files, AP objects).
const maxResponseBytes int64 = 8 << 20 // 8 MB

// isBlockedIP is the core SSRF guard (see netguard.IsBlockedIP).
func isBlockedIP(ip net.IP) bool { return netguard.IsBlockedIP(ip) }

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

// fetchProblem is how a failed fetch is reported in a verdict: the reason
// (an HTTP status, a parse error) when the host answered, but only a fixed
// description when it couldn't be reached, so a verdict doesn't echo
// resolved addresses or tell an internal host from a public one.
func fetchProblem(err error) string {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err.Error()
	}
	var ne net.Error
	switch {
	case strings.Contains(ue.Err.Error(), "blocked non-public address"):
		return "the host is not on the public internet; only public hosts are checked"
	case errors.As(ue.Err, &ne) && ne.Timeout():
		return "the host did not answer in time"
	default:
		return "the host could not be reached"
	}
}
