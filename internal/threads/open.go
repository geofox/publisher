package threads

import (
	"context"

	"github.com/geofox/publisher/internal/destination"
)

// Account configuration and secret names.
const (
	ConfigUserID      = "user_id" // default "me"
	SecretAccessToken = "access_token"
)

// Open implements destination.Driver.
func (Driver) Open(_ context.Context, req destination.OpenRequest) (destination.Account, error) {
	userID, err := destination.ConfigString(req.Config, ConfigUserID)
	if err != nil {
		return nil, destination.InvalidField(ConfigUserID, err.Error())
	}
	token := req.Secrets[SecretAccessToken]
	if token == "" {
		return nil, destination.InvalidField(SecretAccessToken, "required")
	}
	return &Account{C: NewWithNet(token, userID, req.Host.Net(destination.NetTrusted))}, nil
}
