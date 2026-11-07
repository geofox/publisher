package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// What part C adds to the scrubber: secret_key= in a query, a master key
// in JSON, and a Bluesky app password's shape anywhere. Dates, UUIDs and
// ordinary words stay.
func TestScrubPartC(t *testing.T) {
	for in, gone := range map[string]string{
		`GET /x?secret_key=S3CR3T&y=1`:                          "S3CR3T",
		`{"master_key":"bWFzdGVy"}`:                             "bWFzdGVy",
		`createSession: wrong password abcd-ef12-ghij-kl34 for`: "abcd-ef12-ghij-kl34",
	} {
		if out := Scrub(in); strings.Contains(out, gone) {
			t.Errorf("%q leaked: %s", gone, out)
		}
	}
	keep := `2026-11-07 550e8400-e29b-41d4-a716-446655440000 ABCD-EFGH-IJKL-MNOP self-hosted-in-a-box`
	if out := Scrub(keep); out != keep {
		t.Errorf("over-redacted: %s", out)
	}
}

func TestUpstream(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"  \n ":                              "",
		"<html><body>502 Bad Gateway</body>": "(an HTML page)",
		" <!DOCTYPE html>\n<p>x":             "(an HTML page)",
		"{\"error\":\"InvalidToken\",\n\t\"message\":\"Bad\"}\r\n": `{"error":"InvalidToken", "message":"Bad"}`,
		"line one\x00\x1b[31mred sep":                              "line one [31mred sep",
		"\x00\x01starts with controls":                             "starts with controls",
		`{"access_token":"tok123","error":"x"}`:                    `{"access_token":"REDACTED","error":"x"}`,
	}
	for in, want := range cases {
		if got := Upstream(in); got != want {
			t.Errorf("Upstream(%q) = %q, want %q", in, got, want)
		}
	}
	long := "a" + strings.Repeat("é", MaxUpstream) // the limit falls inside a rune
	got := Upstream(long)
	if !utf8.ValidString(got) || len(got) > MaxUpstream+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("long: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if exact := strings.Repeat("a", MaxUpstream); Upstream(exact) != exact {
		t.Error("a body of exactly the limit was cut")
	}
}
