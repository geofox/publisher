package nostr

import (
	"encoding/hex"
	"strings"
	"testing"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// A key typed in the app is nsec1… or hex (NIP-19's example pair); errors
// never repeat it.
func TestParseKey(t *testing.T) {
	const nsec = "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"
	const hexKey = "67dea2ed018072d675f5415ecfaed7d2597555e202d85b3d65ea4e58d2d92ffa"
	for _, in := range []string{nsec, " " + strings.ToUpper(nsec) + " ", hexKey, strings.ToUpper(hexKey)} {
		sk, err := ParseKey(in)
		if err != nil || hex.EncodeToString(sk[:]) != hexKey {
			t.Errorf("ParseKey(%q) = %x, %v", in, sk, err)
		}
	}
	npub := nip19.EncodeNpub(gonostr.GetPublicKey(gonostr.Generate()))
	for _, bad := range []string{
		"", "nsec1", nsec[:len(nsec)-1] + "x", npub, hexKey[:63] + "g", strings.Repeat("0", 64),
		nip19.EncodeNsec([32]byte{}), "1" + hexKey, hexKey[2:],
	} {
		_, err := ParseKey(bad)
		if err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		} else if len(bad) > 8 && strings.Contains(err.Error(), bad) {
			t.Errorf("the error repeats the key: %v", err)
		}
	}
	if _, err := ParseKey(hexKey[2:]); err != errKeyShort {
		t.Errorf("short hex: %v, want the leading-zeros hint", err)
	}
}

// A generated key is a usable key, different each time, and only for the
// generate method.
func TestGenerateCredentials(t *testing.T) {
	a, err := Driver{}.GenerateCredentials(MethodGenerate)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Driver{}.GenerateCredentials(MethodGenerate)
	if _, err := ParseKey(a[SecretKey]); err != nil || len(a) != 1 || a[SecretKey] == b[SecretKey] {
		t.Errorf("generated %d secrets; parse %v; same twice %v", len(a), err, a[SecretKey] == b[SecretKey])
	}
	if _, err := (Driver{}).GenerateCredentials(MethodSecretKey); err == nil {
		t.Error("generated for the secret_key method")
	}
}
