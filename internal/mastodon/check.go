package mastodon

import (
	"context"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.Checker = (*Account)(nil)

// Check implements destination.Checker. Unauthenticated, it reads the
// instance's public description; authenticated, it verifies the token.
func (a *Account) Check(ctx context.Context, req destination.CheckRequest) destination.Health {
	if !req.Authenticated {
		return destination.CheckReachable(ctx, a.C.http, a.C.baseURL+"/api/v1/instance", nil)
	}
	_, err := a.C.Me(ctx)
	return destination.CheckedAuth(err, err != nil && destination.ClassOf(classify(err)) == destination.ClassAuth)
}
