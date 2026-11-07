package mastodon

import (
	"errors"
	"strings"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.EnvSource = Driver{}

// EnvVars implements destination.EnvSource.
func (Driver) EnvVars() []destination.EnvVar {
	return []destination.EnvVar{{Name: "MASTODON_TOKEN", Required: true, Secret: true}, {Name: "MASTODON_BASE_URL", Required: true}}
}

// FromEnv implements destination.EnvSource: MASTODON_TOKEN configures one
// account and needs MASTODON_BASE_URL with it. Without the token the URL is
// ignored.
func (Driver) FromEnv(getenv func(string) string) ([]destination.EnvAccount, error) {
	token := destination.EnvSecret(getenv, "MASTODON_TOKEN")
	if token == "" {
		return nil, nil
	}
	base := strings.TrimRight(destination.EnvString(getenv, "MASTODON_BASE_URL", ""), "/")
	if base == "" {
		return nil, errors.New("MASTODON_BASE_URL is required with MASTODON_TOKEN (your instance, for example https://mastodon.social)")
	}
	return []destination.EnvAccount{{
		Key:     destination.EnvKeyDefault,
		Config:  map[string]any{ConfigInstanceURL: base},
		Secrets: map[string]string{SecretAccessToken: token},
	}}, nil
}
