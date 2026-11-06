package threads

import "github.com/geofox/publisher/internal/destination"

var _ destination.EnvSource = Driver{}

// EnvVars implements destination.EnvSource.
func (Driver) EnvVars() []string { return []string{"THREADS_ACCESS_TOKEN", "THREADS_USER_ID"} }

// FromEnv implements destination.EnvSource: THREADS_ACCESS_TOKEN configures
// one account; THREADS_USER_ID defaults to "me".
func (Driver) FromEnv(getenv func(string) string) ([]destination.EnvAccount, error) {
	token := destination.EnvString(getenv, "THREADS_ACCESS_TOKEN", "")
	if token == "" {
		return nil, nil
	}
	return []destination.EnvAccount{{
		Key:     destination.EnvKeyDefault,
		Config:  map[string]any{ConfigUserID: destination.EnvString(getenv, "THREADS_USER_ID", "me")},
		Secrets: map[string]string{SecretAccessToken: token},
	}}, nil
}
