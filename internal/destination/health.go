package destination

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	Authenticated bool `json:"authenticated"`
}

// Health is the outcome of a check.
//
// OK means the check passed; it says nothing about the credentials unless
// AuthOK is set. Only AuthOK == true proves they work, and only AuthOK ==
// false proves the network refused them: an unauthenticated check, a
// timeout, a rate limit or an outage leaves AuthOK nil. So a host clears
// "needs reconnecting" only on AuthOK == true, and sets it only on false.
type Health struct {
	// OK: the server answered and, for an authenticated check, accepted
	// the credentials.
	OK bool `json:"ok"`
	// Reachable: the server answered at all (any status below 500).
	Reachable bool `json:"reachable"`
	// AuthOK is set by authenticated checks that got an answer about the
	// credentials: true means they work, false that the network refused
	// them. Nil means unknown (a network without credentials to check,
	// such as Nostr's relays, leaves it nil).
	AuthOK  *bool  `json:"auth_ok,omitempty"`
	Message string `json:"message,omitempty"`
	// ExpiresAt is when the credentials stop working, when known.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// ReconnectBy is when the network's grant to Publisher ends, when
	// known (a Meta grant lasts 90 days): renewing the credentials doesn't
	// move it, so the owner connects the account again before then.
	ReconnectBy *time.Time `json:"reconnect_by,omitempty"`
}

// Refresher is implemented by accounts whose credentials must be renewed
// before they expire (a Threads long-lived token).
type Refresher interface {
	// NextRefresh is when Refresh should next run; zero means never.
	NextRefresh(now time.Time) time.Time
	// Refresh renews the credentials and saves them through the host.
	Refresh(ctx context.Context) error
}

// Expirer is implemented by accounts whose credentials expire at a time
// they know without asking the network (a Threads token's expiry, kept with
// it). The host alerts when it comes close.
type Expirer interface {
	// CredentialsExpireAt is when the credentials stop working, or zero
	// when unknown or never.
	CredentialsExpireAt() time.Time
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
	hasStatus := errors.As(err, &he)
	switch {
	case ClassOf(err) == ClassRateLimited:
		// A 429, or a network's own rate-limit code on another status.
		return Health{Reachable: true, Message: "the server is rate limiting this account; try again later"}
	case hasStatus:
		return Health{Reachable: he.StatusCode < 500, Message: fmt.Sprintf("the server answered %d", he.StatusCode)}
	case isDecodeError(err):
		return Health{Reachable: true, Message: "the server's answer could not be read"}
	}
	return Health{Message: NetProblem(err)}
}

// isDecodeError reports whether err is a response that arrived but wasn't
// the JSON expected (an HTML error page with status 200).
func isDecodeError(err error) bool {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syn) || errors.As(err, &typ) || errors.Is(err, io.ErrUnexpectedEOF)
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
