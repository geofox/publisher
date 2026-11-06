package bluesky

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.Checker = (*Account)(nil)

// Check implements destination.Checker. Unauthenticated, it asks the PDS's
// health endpoint; authenticated, it signs in and validates the session.
func (a *Account) Check(ctx context.Context, req destination.CheckRequest) destination.Health {
	if !req.Authenticated {
		return destination.CheckReachable(ctx, a.C.HTTP, a.C.PDS+"/xrpc/_health", nil)
	}
	err := a.C.CheckSession(ctx)
	return destination.CheckedAuth(err, credentialsRefused(err))
}

// credentialsRefused reports whether the PDS turned down the identifier and
// app password, or the account can't sign in at all. A rate limit or a
// server error is not a refusal.
func credentialsRefused(err error) bool {
	var he *destination.HTTPError
	if err == nil || !errors.As(err, &he) {
		return false
	}
	if he.StatusCode == http.StatusTooManyRequests || he.StatusCode >= 500 {
		return false
	}
	return sessionRefused(err) ||
		strings.Contains(he.Body, "AuthenticationRequired") ||
		strings.Contains(he.Body, "AccountTakedown") ||
		strings.Contains(he.Body, "AccountDeactivated")
}
