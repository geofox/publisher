package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

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
	if err == nil {
		return destination.CheckedAuth(nil, false)
	}
	return destination.CheckedAuth(classify(err), credentialsRefused(err))
}

// credentialsRefused reports whether Mastodon itself turned the token
// down on verify_credentials, with its JSON error body
// (https://docs.joinmastodon.org/methods/accounts/#verify_credentials):
// 401 for an invalid token, 403 for a login that is disabled, unconfirmed
// or pending approval, 422 for a token with no user behind it. The same
// status with a page from a proxy or firewall in front of the instance
// says nothing about the token.
func credentialsRefused(err error) bool {
	var he *destination.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	switch he.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
	default:
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(he.Body), &body) == nil && body.Error != ""
}
