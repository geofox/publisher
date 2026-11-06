package threads

import (
	"context"
	"net/url"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.Checker = (*Account)(nil)

// Check implements destination.Checker. Unauthenticated, it reaches the
// Graph API host; authenticated, it asks for the account's id. Both report
// the token's expiry when the token manager knows it.
func (a *Account) Check(ctx context.Context, req destination.CheckRequest) destination.Health {
	var h destination.Health
	if req.Authenticated {
		_, err := a.C.Me(ctx)
		h = destination.CheckedAuth(err, err != nil && destination.ClassOf(classify(err)) == destination.ClassAuth)
	} else {
		h = destination.CheckReachable(ctx, a.C.HTTP, apiRoot(a.C.BaseURL), nil)
	}
	if exp := a.C.TokenExpiry(); !exp.IsZero() {
		h.ExpiresAt = &exp
	}
	return h
}

// apiRoot is the Graph API host's root ("https://graph.threads.net/").
func apiRoot(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	return u.Scheme + "://" + u.Host + "/"
}
