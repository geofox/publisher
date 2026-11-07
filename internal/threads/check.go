package threads

import (
	"context"
	"net/url"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.Checker = (*Account)(nil)

// Check implements destination.Checker. Unauthenticated, it reaches the
// Graph API host; authenticated, it asks for the account's id. Both report
// the token's expiry when the token manager knows it.
func (a *Account) Check(ctx context.Context, req destination.CheckRequest) destination.Health {
	var h destination.Health
	if req.Authenticated {
		var err error
		if _, err = a.C.Me(ctx); err != nil {
			err = classify(err) // Graph codes: 190 refuses the token, 4 is a rate limit
		}
		h = destination.CheckedAuth(err, destination.ClassOf(err) == destination.ClassAuth)
	} else {
		h = destination.CheckReachable(ctx, a.C.HTTP, apiRoot(a.C.BaseURL), nil)
	}
	if exp := a.C.TokenExpiry(); !exp.IsZero() {
		h.ExpiresAt = &exp
	}
	if req.Authenticated && h.AuthOK != nil && *h.AuthOK {
		if end := a.grant(ctx); !end.IsZero() {
			h.ReconnectBy = &end
		}
	}
	return h
}

// grant is when Meta's grant ends: asked again at each authenticated
// check (a renewal moves it on for a public profile), the last answer
// kept when Meta doesn't say (an app that has left development mode
// answers debug_token only with its own token).
func (a *Account) grant(ctx context.Context) time.Time {
	end, err := a.C.GrantEnd(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.grantEnd = end
	}
	return a.grantEnd
}

// apiRoot is the Graph API host's root ("https://graph.threads.net/").
func apiRoot(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	return u.Scheme + "://" + u.Host + "/"
}
