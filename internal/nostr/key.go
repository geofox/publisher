package nostr

import (
	"encoding/hex"
	"errors"
	"strings"

	gonostr "fiatjaf.com/nostr"
)

var (
	// errKeyIsNsec: a bech32 nsec1… was given where the hex form belongs.
	errKeyIsNsec = errors.New("this is an nsec1… key: give its 64-character hex form (convert it with `nak key decode`)")
	// errKeyNotHex: anything else that isn't exactly 64 hex characters.
	errKeyNotHex = errors.New("not a 64-character hex secret key (convert an nsec1… key with `nak key decode`)")
)

// ParseSecretKey reads a secret key given as exactly 64 hex characters.
// The library pads a shorter value with zeros, which would sign with a key
// anyone can compute (NSEC_HEX=1 is secret key 1), so the length is
// checked first. Errors never contain the value.
func ParseSecretKey(s string) (gonostr.SecretKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "nsec1") {
		return gonostr.SecretKey{}, errKeyIsNsec
	}
	if len(s) != 64 {
		return gonostr.SecretKey{}, errKeyNotHex
	}
	if _, err := hex.DecodeString(s); err != nil {
		return gonostr.SecretKey{}, errKeyNotHex
	}
	sk, err := gonostr.SecretKeyFromHex(s)
	if err != nil {
		return gonostr.SecretKey{}, errKeyNotHex
	}
	return sk, nil
}
