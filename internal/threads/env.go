package threads

import "github.com/geofox/publisher/internal/destination"

var _ destination.EnvSource = Driver{}

// EnvVars implements destination.EnvSource.
func (Driver) EnvVars() []destination.EnvVar {
	return []destination.EnvVar{{Name: "THREADS_ACCESS_TOKEN", Required: true, Secret: true}, {Name: "THREADS_USER_ID"}}
}

// FromEnv implements destination.EnvSource: THREADS_ACCESS_TOKEN configures
// one account; THREADS_USER_ID defaults to "me". The token is kept exactly
// as set: the token manager recognizes it by its hash, and a trimmed
// token would read as a new one and replace the refreshed token.
func (Driver) FromEnv(getenv func(string) string) ([]destination.EnvAccount, error) {
	token := destination.EnvSecret(getenv, "THREADS_ACCESS_TOKEN")
	if token == "" {
		return nil, nil
	}
	return []destination.EnvAccount{{
		Key:     destination.EnvKeyDefault,
		Config:  map[string]any{ConfigUserID: destination.EnvString(getenv, "THREADS_USER_ID", "me")},
		Secrets: map[string]string{SecretAccessToken: token},
	}}, nil
}
