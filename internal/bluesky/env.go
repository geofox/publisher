package bluesky

import (
	"errors"
	"strings"

	"github.com/geofox/publisher/internal/destination"
)

var _ destination.EnvSource = Driver{}

// EnvVars implements destination.EnvSource.
func (Driver) EnvVars() []string {
	return []string{"BLUESKY_APP_PASSWORD", "BLUESKY_IDENTIFIER", "BLUESKY_PDS_URL"}
}

// FromEnv implements destination.EnvSource: BLUESKY_APP_PASSWORD configures
// one account and needs BLUESKY_IDENTIFIER and BLUESKY_PDS_URL with it.
// Without the password the other two are ignored.
func (Driver) FromEnv(getenv func(string) string) ([]destination.EnvAccount, error) {
	pw := destination.EnvString(getenv, "BLUESKY_APP_PASSWORD", "")
	if pw == "" {
		return nil, nil
	}
	id := destination.EnvString(getenv, "BLUESKY_IDENTIFIER", "")
	pds := strings.TrimRight(destination.EnvString(getenv, "BLUESKY_PDS_URL", ""), "/")
	var errs []error
	if id == "" {
		errs = append(errs, errors.New("BLUESKY_IDENTIFIER is required with BLUESKY_APP_PASSWORD (your handle, DID or email)"))
	}
	if pds == "" {
		errs = append(errs, errors.New("BLUESKY_PDS_URL is required with BLUESKY_APP_PASSWORD (for example https://bsky.social)"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return []destination.EnvAccount{{
		Key:     destination.EnvKeyDefault,
		Config:  map[string]any{ConfigIdentifier: id, ConfigPDSURL: pds},
		Secrets: map[string]string{SecretAppPassword: pw},
	}}, nil
}
