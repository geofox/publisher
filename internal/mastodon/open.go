package mastodon

import (
	"context"

	"github.com/geofox/publisher/internal/destination"
)

// Account configuration and secret names.
const (
	ConfigInstanceURL = "instance_url"
	SecretAccessToken = "access_token"
)

// Open implements destination.Driver.
func (Driver) Open(_ context.Context, req destination.OpenRequest) (destination.Account, error) {
	base, err := destination.ConfigString(req.Config, ConfigInstanceURL)
	if err != nil {
		return nil, destination.InvalidField(ConfigInstanceURL, err.Error())
	}
	token := req.Secrets[SecretAccessToken]
	switch {
	case base == "":
		return nil, destination.InvalidField(ConfigInstanceURL, "required")
	case token == "":
		return nil, destination.InvalidField(SecretAccessToken, "required")
	}
	return NewAccount(NewWithNet(base, token, req.Host.Net(destination.NetAccount))), nil
}
