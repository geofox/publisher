// Package webauthntest is a software passkey authenticator for tests: it
// answers the options the server sends to navigator.credentials.create
// and .get with the JSON a browser would post back, signed with a P-256
// key, so the whole ceremony runs in Go.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// Authenticator flags (WebAuthn §6.1).
const (
	FlagUP = 0x01 // user present
	FlagUV = 0x04 // user verified
	FlagBE = 0x08 // backup eligible
	FlagBS = 0x10 // backed up
	FlagAT = 0x40 // attested credential data
)

// Authenticator holds passkeys, one per relying party id and user handle,
// like a platform authenticator.
type Authenticator struct {
	// Origin is what the "browser" puts in clientDataJSON.
	Origin string
	// Flags are added to UP|UV on every response (FlagBE|FlagBS for a
	// synced passkey).
	Flags byte
	// NoUV leaves user verification out.
	NoUV bool
	// FreezeCounter keeps the signature counter where it is (a clone, or
	// an authenticator that always sends 0 when Counter is 0).
	FreezeCounter bool

	Creds []*Credential
}

// Credential is one passkey.
type Credential struct {
	ID         []byte
	RPID       string
	UserHandle []byte
	Key        *ecdsa.PrivateKey
	Counter    uint32
}

// New returns an authenticator whose "browser" runs at origin.
func New(origin string) *Authenticator { return &Authenticator{Origin: origin} }

var b64 = base64.RawURLEncoding

type creationOptions struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		ExcludeCredentials []struct {
			ID string `json:"id"`
		} `json:"excludeCredentials"`
	} `json:"publicKey"`
}

type requestOptions struct {
	PublicKey struct {
		Challenge        string `json:"challenge"`
		RPID             string `json:"rpId"`
		AllowCredentials []struct {
			ID string `json:"id"`
		} `json:"allowCredentials"`
	} `json:"publicKey"`
}

// options decodes what the server answered (either the options object
// itself or any JSON value holding it, such as {"publicKey": …}).
func options(v any, into any) error {
	b, ok := v.([]byte)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			return err
		}
	}
	return json.Unmarshal(b, into)
}

func (a *Authenticator) flags(extra byte) byte {
	f := byte(FlagUP|FlagUV) | a.Flags | extra
	if a.NoUV {
		f &^= FlagUV
	}
	return f
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

func authData(rpID string, flags byte, counter uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, counter)
	return append(out, attested...)
}

func coseKey(k *ecdsa.PublicKey) ([]byte, error) {
	pub, err := k.ECDH()
	if err != nil {
		return nil, err
	}
	raw := pub.Bytes() // 0x04 | X | Y
	return webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: raw[1:33], -3: raw[33:65]})
}

// Create makes a passkey for the creation options (the server's answer to
// a "begin") and returns the JSON the browser posts back.
func (a *Authenticator) Create(opts any) (json.RawMessage, *Credential, error) {
	var o creationOptions
	if err := options(opts, &o); err != nil {
		return nil, nil, err
	}
	pk := o.PublicKey
	if pk.Challenge == "" || pk.RP.ID == "" {
		return nil, nil, fmt.Errorf("webauthntest: not creation options: %v", opts)
	}
	for _, ex := range pk.ExcludeCredentials {
		for _, c := range a.Creds {
			if b64.EncodeToString(c.ID) == ex.ID {
				return nil, nil, fmt.Errorf("webauthntest: already registered (excludeCredentials)")
			}
		}
	}
	user, err := b64.DecodeString(pk.User.ID)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	c := &Credential{ID: id, RPID: pk.RP.ID, UserHandle: user, Key: key}
	ck, err := coseKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	attested := make([]byte, 16) // AAGUID: zero
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(id)))
	attested = append(attested, id...)
	attested = append(attested, ck...)
	ad := authData(pk.RP.ID, a.flags(FlagAT), c.Counter, attested)
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(map[string]any{
		"id": b64.EncodeToString(id), "rawId": b64.EncodeToString(id), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData("webauthn.create", pk.Challenge, a.Origin)),
			"attestationObject": b64.EncodeToString(att),
			"transports":        []string{"internal", "hybrid"},
		},
		"clientExtensionResults": map[string]any{},
	})
	if err != nil {
		return nil, nil, err
	}
	a.Creds = append(a.Creds, c)
	return body, c, nil
}

// Get signs the request options (the server's answer to a sign-in or
// re-authentication "begin") with a matching passkey: one listed in
// allowCredentials, else any for the relying party (discoverable).
func (a *Authenticator) Get(opts any) (json.RawMessage, error) {
	var o requestOptions
	if err := options(opts, &o); err != nil {
		return nil, err
	}
	pk := o.PublicKey
	var c *Credential
	for _, cand := range a.Creds {
		if cand.RPID != pk.RPID {
			continue
		}
		if len(pk.AllowCredentials) == 0 {
			c = cand
			break
		}
		for _, al := range pk.AllowCredentials {
			if al.ID == b64.EncodeToString(cand.ID) {
				c = cand
			}
		}
	}
	if c == nil {
		return nil, fmt.Errorf("webauthntest: no passkey for %q", pk.RPID)
	}
	return a.Sign(c, pk.Challenge, pk.RPID)
}

// Sign makes an assertion with c for challenge and rpID (a test can use
// another rpID or a stale challenge on purpose).
func (a *Authenticator) Sign(c *Credential, challenge, rpID string) (json.RawMessage, error) {
	if !a.FreezeCounter {
		c.Counter++
	}
	ad := authData(rpID, a.flags(0), c.Counter, nil)
	cd := clientData("webauthn.get", challenge, a.Origin)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, c.Key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(c.ID), "rawId": b64.EncodeToString(c.ID), "type": "public-key",
		"authenticatorAttachment": "platform",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(cd),
			"authenticatorData": b64.EncodeToString(ad),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(c.UserHandle),
		},
		"clientExtensionResults": map[string]any{},
	})
}

// Challenge reads the challenge out of options (to replay a stale one).
func Challenge(opts any) string {
	var o requestOptions
	_ = options(opts, &o)
	return o.PublicKey.Challenge
}
