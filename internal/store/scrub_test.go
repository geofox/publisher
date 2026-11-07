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

// Spec §3.2: the names part C adds, credentials in query strings and form
// bodies (a url.Error prints its URL), and bare nsec1 keys; upstream error
// codes and ordinary words stay.
func TestScrubPartCNames(t *testing.T) {
	cases := map[string][]string{
		`{"secret_key":"s1","client_secret":"s2","app_secret":"s3","code_verifier":"s4","refresh_token":"s5","code":"invalid_grant"}`:       {"s1", "s2", "s3", "s4", "s5"},
		`Post "https://graph.threads.net/refresh_access_token?grant_type=th_refresh_token&access_token=THQWERTY123": dial tcp: i/o timeout`: {"THQWERTY123"},
		`grant_type=password&client_secret=CS9&password=hunter22&username=me`:                                                               {"CS9", "hunter22"},
		`key nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5 leaked`:                                                        {"vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"},
		`Authorization: Bearer abc.def/ghi+jkl=`:                                                                                            {"abc.def/ghi+jkl="},
	}
	for in, gone := range cases {
		out := Scrub(in)
		for _, g := range gone {
			if strings.Contains(out, g) {
				t.Errorf("%q leaked in %s", g, out)
			}
		}
	}
	keep := `{"error":"InvalidToken","code":"invalid_grant","message":"the passkey request expired"} grant_type=th_refresh_token npub1abc`
	if out := Scrub(keep); !strings.Contains(out, "invalid_grant") || !strings.Contains(out, "th_refresh_token") || !strings.Contains(out, "npub1abc") || !strings.Contains(out, "InvalidToken") {
		t.Errorf("over-redacted: %s", out)
	}
}
