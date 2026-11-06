package destination

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Checker is implemented by accounts that can check their connection.
type Checker interface {
	Check(ctx context.Context, req CheckRequest) Health
}

// CheckRequest says what a check may do.
type CheckRequest struct {
	// Authenticated lets the check use the account's credentials. A false
	// value asks for reachability only (readiness probes), with no
	// credential sent.
	Authenticated bool
}

// Health is the outcome of a check.
type Health struct {
	OK        bool `json:"ok"`
	Reachable bool `json:"reachable"`
	// AuthOK is set by authenticated checks: false means the network
	// refused the credentials.
	AuthOK  *bool  `json:"auth_ok,omitempty"`
	Message string `json:"message,omitempty"`
	// ExpiresAt is when the credentials stop working, when known.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Refresher is implemented by accounts whose credentials must be renewed
// before they expire (a Threads long-lived token).
type Refresher interface {
	// NextRefresh is when Refresh should next run; zero means never.
	NextRefresh(now time.Time) time.Time
	// Refresh renews the credentials and saves them through the host.
	Refresh(ctx context.Context) error
}

// CheckReachable is the Health of an unauthenticated GET: any answer below
// 500 means the server is up. client must not add credentials.
func CheckReachable(ctx context.Context, client *http.Client, url string, header http.Header) Health {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Health{Message: "the address is not valid"}
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", "publisher-check")
	resp, err := client.Do(req)
	if err != nil {
		return Health{Message: NetProblem(err)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 {
		return Health{Message: fmt.Sprintf("the server answered %d", resp.StatusCode)}
	}
	return Health{OK: true, Reachable: true}
}

// CheckedAuth is the Health of an authenticated request that ended with
// err. refused says the network turned the credentials down; only that
// sets AuthOK to false, so a network that is down or busy never reads as
// a bad credential.
func CheckedAuth(err error, refused bool) Health {
	yes, no := true, false
	switch {
	case err == nil:
		return Health{OK: true, Reachable: true, AuthOK: &yes}
	case refused:
		return Health{Reachable: true, AuthOK: &no, Message: "the network refused the credentials; connect the account again"}
	}
	var he *HTTPError
	if errors.As(err, &he) {
		h := Health{Reachable: he.StatusCode < 500, Message: fmt.Sprintf("the server answered %d", he.StatusCode)}
		if he.StatusCode == http.StatusTooManyRequests {
			h.Message = "the server is rate limiting this account; try again later"
		}
		return h
	}
	return Health{Message: NetProblem(err)}
}

// NetProblem describes a failure to reach a server in a few words, without
// the request (which can carry addresses the owner didn't type).
func NetProblem(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "the server did not answer in time"
	case errors.Is(err, context.Canceled):
		return "the check was cancelled"
	case errors.As(err, &dnsErr):
		return "the server's name could not be found"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr):
		return "the server's certificate is not trusted"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the server refused the connection"
	default:
		return "the server could not be reached"
	}
}
