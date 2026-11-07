package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// Errors of Seal and Open. None carries a value.
var (
	// ErrNoKey: no master key is set, so nothing can be sealed.
	ErrNoKey = errors.New("no master key is set (PUBLISHER_MASTER_KEY)")
	// ErrUnknownKey: the value was sealed under a key this server doesn't
	// have (neither the current nor the previous one).
	ErrUnknownKey = errors.New("sealed under a master key this server doesn't have")
	// ErrOpen: the value didn't open: it was changed, moved to another
	// row, or bound to another server.
	ErrOpen = errors.New("a stored secret failed to open (changed, moved, or bound to another server)")
	// ErrFormat: not a sealed value.
	ErrFormat = errors.New("not a sealed secret")
	// ErrBinding: a binding field holds a NUL byte.
	ErrBinding = errors.New("a secret's binding can't contain a NUL byte")
)

// Binding is where a secret belongs. All of it is authenticated with the
// value (spec §3.2), so a sealed value opens only for the same binding.
type Binding struct {
	Owner string // an owner id, or "instance"
	Scope string // account | setting | oauth_client | export
	Ref   string // the account id, setting key or client id
	Name  string // app_password, access_token, secret_key, …
	// Audience is the normalized origin the secret is sent to (spec §3.3),
	// or "".
	Audience string
}

// Scopes.
const (
	ScopeAccount     = "account"
	ScopeSetting     = "setting"
	ScopeOAuthClient = "oauth_client"
	ScopeExport      = "export"
)

func (b Binding) aad(kid string) ([]byte, error) {
	parts := []string{"publisher/secrets/v1", kid, b.Owner, b.Scope, b.Ref, b.Name, b.Audience}
	for _, p := range parts {
		if strings.IndexByte(p, 0) >= 0 {
			return nil, ErrBinding
		}
	}
	return []byte(strings.Join(parts, "\x00")), nil
}

// Seal encrypts plain for b under the current key, as
// "v1.<key id>.<base64url(nonce || ciphertext || tag)>".
func (kr Keyring) Seal(b Binding, plain string) (string, error) {
	if kr.Current == nil {
		return "", ErrNoKey
	}
	return kr.Current.seal(b, plain)
}

func (k *Key) seal(b Binding, plain string) (string, error) {
	aad, err := b.aad(k.id)
	if err != nil {
		return "", err
	}
	aead, err := k.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize()) // Seal grows it to fit
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := aead.Seal(nonce, nonce, []byte(plain), aad)
	return sealedPrefix + k.id + "." + base64.RawURLEncoding.EncodeToString(out), nil
}

func (k *Key) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.enc)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KeyID is the id of the key a sealed value was sealed under.
func KeyID(sealed string) (string, error) {
	kid, _, err := split(sealed)
	return kid, err
}

func split(sealed string) (kid string, body []byte, err error) {
	rest, ok := strings.CutPrefix(sealed, sealedPrefix)
	if !ok {
		return "", nil, ErrFormat
	}
	kid, enc, ok := strings.Cut(rest, ".")
	if !ok || len(kid) != 2*keyIDLen {
		return "", nil, ErrFormat
	}
	body, err = base64.RawURLEncoding.Strict().DecodeString(enc)
	if err != nil {
		return "", nil, ErrFormat
	}
	return kid, body, nil
}

// Open decrypts a sealed value for b with the key it names (current or
// previous). With no key at all it says so (ErrNoKey) rather than that the
// key is another one.
func (kr Keyring) Open(b Binding, sealed string) (string, error) {
	kid, body, err := split(sealed)
	if err != nil {
		return "", err
	}
	k := kr.key(kid)
	switch {
	case k == nil && kr.Current == nil && kr.Previous == nil:
		return "", ErrNoKey
	case k == nil:
		return "", ErrUnknownKey
	}
	aad, err := b.aad(kid)
	if err != nil {
		return "", err
	}
	aead, err := k.aead()
	if err != nil {
		return "", err
	}
	n := aead.NonceSize()
	if len(body) < n+aead.Overhead() {
		return "", ErrOpen
	}
	plain, err := aead.Open(nil, body[:n], body[n:], aad)
	if err != nil {
		return "", ErrOpen
	}
	return string(plain), nil
}

// Reseal moves a value to the current key: it is opened (with the key it
// names) and sealed again under the current key. A value already under
// the current key is returned as it is, with changed false.
func (kr Keyring) Reseal(b Binding, sealed string) (out string, changed bool, err error) {
	if kr.Current == nil {
		return "", false, ErrNoKey
	}
	kid, err := KeyID(sealed)
	if err != nil {
		return "", false, err
	}
	if kid == kr.Current.id {
		return sealed, false, nil
	}
	plain, err := kr.Open(b, sealed)
	if err != nil {
		return "", false, err
	}
	out, err = kr.Current.seal(b, plain)
	return out, err == nil, err
}
