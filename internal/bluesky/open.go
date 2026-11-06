package bluesky

import (
	"context"

	"github.com/geofox/publisher/internal/destination"
)

// Account configuration and secret names.
const (
	ConfigIdentifier  = "identifier" // handle, DID or email
	ConfigPDSURL      = "pds_url"
	SecretAppPassword = "app_password"
)

// Open implements destination.Driver.
func (Driver) Open(_ context.Context, req destination.OpenRequest) (destination.Account, error) {
	pds, err := destination.ConfigString(req.Config, ConfigPDSURL)
	if err != nil {
		return nil, destination.InvalidField(ConfigPDSURL, err.Error())
	}
	id, err := destination.ConfigString(req.Config, ConfigIdentifier)
	if err != nil {
		return nil, destination.InvalidField(ConfigIdentifier, err.Error())
	}
	pw := req.Secrets[SecretAppPassword]
	switch {
	case pds == "":
		return nil, destination.InvalidField(ConfigPDSURL, "required")
	case id == "":
		return nil, destination.InvalidField(ConfigIdentifier, "required")
	case pw == "":
		return nil, destination.InvalidField(SecretAppPassword, "required")
	}
	return &Account{C: NewWithNet(pds, id, pw, req.Host.Net(destination.NetTrusted))}, nil
}
