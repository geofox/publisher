package localauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// Purpose is what a passkey ceremony was begun for; finishing it for
// another purpose is refused.
type Purpose string

// Ceremony purposes.
const (
	PurposeLogin    Purpose = "login"    // sign in (no session)
	PurposeSetup    Purpose = "setup"    // the first passkey (a setup state)
	PurposeRegister Purpose = "register" // another passkey (a session)
	PurposeReauth   Purpose = "reauth"   // re-authenticate for one operation (a session)
)

// Ceremony limits: a ceremony lives 5 minutes; an address may begin 20 a
// minute, and a failed sign-in counts as a begin.
const (
	CeremonyTTL      = 5 * time.Minute
	beginsPerMinute  = 20
	maxSealedLen     = 4096
	spentSweepAtSize = 256
)

// ErrRateLimited: the address began too many ceremonies; Wait says when it
// may begin again.
type ErrRateLimited struct{ Wait time.Duration }

func (e *ErrRateLimited) Error() string { return "too many sign-in attempts; try again shortly" }

// ErrCeremony: the ceremony is unknown, expired, used, or was begun for
// another purpose, operation or session.
var ErrCeremony = errors.New("this passkey request expired or doesn't match; start again")

// ceremony is one passkey request in flight.
type ceremony struct {
	Purpose Purpose              `json:"p"`
	Binding string               `json:"b,omitempty"` // the session hash (register, reauth) or setup state id (setup)
	Op      string               `json:"o,omitempty"` // reauth: the operation
	Data    webauthn.SessionData `json:"d"`
	Expires int64                `json:"e"` // unix seconds
}

// Ceremonies keeps each ceremony in the browser that began it, sealed
// (AES-GCM, with a key made at start: a restart ends them, which only
// means starting again), so no address can crowd out another's. A
// ceremony is used once: the challenges of finished ones are remembered
// until they expire. A failed finish doesn't use it (a signature can't be
// guessed), but counts against the address's begins.
type Ceremonies struct {
	aead   cipher.AEAD
	begins *Limiter
	now    func() time.Time

	mu    sync.Mutex
	spent map[string]time.Time // challenge → when it expires
}

// NewCeremonies returns an empty set with a fresh key.
func NewCeremonies() *Ceremonies {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	c := &Ceremonies{aead: aead, spent: map[string]time.Time{}, now: time.Now}
	c.begins = &Limiter{N: beginsPerMinute, Window: time.Minute, Now: func() time.Time { return c.now() }}
	return c
}

func randomID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// start seals a new ceremony begun from addr (an AddrKey), for the
// browser's cookie.
func (c *Ceremonies) start(addr string, cer ceremony) (string, error) {
	if w := c.begins.Take(addr); w > 0 {
		return "", &ErrRateLimited{Wait: w}
	}
	cer.Expires = c.now().Add(CeremonyTTL).Unix()
	plain, err := json.Marshal(cer)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize(), c.aead.NonceSize()+len(plain)+c.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, nil))
	if len(sealed) > maxSealedLen {
		return "", errors.New("passkey request too large for a cookie")
	}
	return sealed, nil
}

// open returns the sealed ceremony, if it is alive, unused, and was begun
// for purpose with this binding (and operation, for reauth). It doesn't
// use it: spend does, once the response checks out.
func (c *Ceremonies) open(sealed string, purpose Purpose, binding, op string) (ceremony, error) {
	if sealed == "" || len(sealed) > maxSealedLen {
		return ceremony{}, ErrCeremony
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	n := c.aead.NonceSize()
	if err != nil || len(raw) < n {
		return ceremony{}, ErrCeremony
	}
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return ceremony{}, ErrCeremony
	}
	var cer ceremony
	if err := json.Unmarshal(plain, &cer); err != nil {
		return ceremony{}, ErrCeremony
	}
	if !c.now().Before(time.Unix(cer.Expires, 0)) || cer.Purpose != purpose || cer.Binding != binding || cer.Op != op {
		return ceremony{}, ErrCeremony
	}
	c.mu.Lock()
	_, used := c.spent[cer.Data.Challenge]
	c.mu.Unlock()
	if used {
		return ceremony{}, ErrCeremony
	}
	return cer, nil
}

// spend uses a ceremony: ErrCeremony when it already was (two finishes
// racing with one response: only one wins).
func (c *Ceremonies) spend(cer ceremony) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.spent) >= spentSweepAtSize {
		for ch, exp := range c.spent {
			if !now.Before(exp) {
				delete(c.spent, ch)
			}
		}
	}
	if _, used := c.spent[cer.Data.Challenge]; used {
		return ErrCeremony
	}
	c.spent[cer.Data.Challenge] = time.Unix(cer.Expires, 0)
	return nil
}

// failed counts a failed finish from addr (an AddrKey) against its begins.
func (c *Ceremonies) failed(addr string) { c.begins.Note(addr) }

// limited refuses a finish from addr (an AddrKey) that failed or began
// too often.
func (c *Ceremonies) limited(addr string) error {
	if w := c.begins.Wait(addr); w > 0 {
		return &ErrRateLimited{Wait: w}
	}
	return nil
}
