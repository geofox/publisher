package store

import (
	"strings"
	"testing"
)

func TestScrubRemovesSecrets(t *testing.T) {
	in := `{"Authorization":"Bearer abc123","access_token":"xyz","app_password":"hunter2","text":"hello"}`
	out := Scrub(in)
	for _, secret := range []string{"abc123", "xyz", "hunter2"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("scrubbed too much: %s", out)
	}
}

func TestScrubKeepsNonSecretsAndRedactsMore(t *testing.T) {
	out := Scrub(`{"token":"abc","user":"alice","accessJwt":"jjj","nsec":"nnn"}`)
	for _, gone := range []string{`"abc"`, `"jjj"`, `"nnn"`} {
		if strings.Contains(out, strings.Trim(gone, `"`)) {
			t.Errorf("leaked %s: %s", gone, out)
		}
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("over-redacted user: %s", out)
	}
}
