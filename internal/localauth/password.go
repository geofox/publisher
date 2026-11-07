// Package localauth is local sign-in (AUTH_MODE=local): passkeys through
// go-webauthn, an optional argon2id password, the throttle that guards it,
// the first-run setup token and re-authentication. HTTP lives in
// internal/api; rows in internal/store.
package localauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password rules and argon2id parameters (RFC 9106's second recommended
// option: 64 MiB, 3 passes).
const (
	MinPasswordChars = 12
	MaxPasswordBytes = 1024

	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ErrBusy: two password hashes are already running; the caller answers
// 429 at once instead of queueing more 64 MiB jobs.
var ErrBusy = errors.New("too many password checks at once; try again in a moment")

// CheckPassword reports why a new password can't be used, or nil.
func CheckPassword(pw string) error {
	switch {
	case !utf8.ValidString(pw):
		return errors.New("the password must be valid text")
	case utf8.RuneCountInString(pw) < MinPasswordChars:
		return fmt.Errorf("use at least %d characters", MinPasswordChars)
	case len(pw) > MaxPasswordBytes:
		return fmt.Errorf("use at most %d bytes", MaxPasswordBytes)
	}
	return nil
}

// Hasher hashes and checks passwords, at most two at once.
type Hasher struct {
	sem chan struct{}
}

// NewHasher returns a Hasher that runs at most two hashes at once.
func NewHasher() *Hasher { return &Hasher{sem: make(chan struct{}, 2)} }

func (h *Hasher) acquire() bool {
	select {
	case h.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Hasher) release() { <-h.sem }

// Do runs fn, a job that costs as much as a hash (an export file's key),
// in one of the same two places: ErrBusy when both are taken.
func (h *Hasher) Do(fn func()) error {
	if !h.acquire() {
		return ErrBusy
	}
	defer h.release()
	fn()
	return nil
}

// Hash returns the password's PHC string
// ($argon2id$v=19$m=65536,t=3,p=4$salt$key).
func (h *Hasher) Hash(pw string) (string, error) {
	if !h.acquire() {
		return "", ErrBusy
	}
	defer h.release()
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether pw matches the PHC string, comparing in constant
// time. A password over the size limit never matches (and isn't hashed).
func (h *Hasher) Verify(phc, pw string) (bool, error) {
	p, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if len(pw) > MaxPasswordBytes {
		return false, nil
	}
	if !h.acquire() {
		return false, ErrBusy
	}
	defer h.release()
	key := argon2.IDKey([]byte(pw), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

type phcParams struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

// parsePHC reads an argon2id PHC string, with bounds on its cost so a
// changed row can't make a check run for minutes.
func parsePHC(s string) (phcParams, error) {
	var p phcParams
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return p, errors.New("not an argon2id password hash")
	}
	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &threads); err != nil {
		return p, errors.New("bad argon2id parameters")
	}
	if p.memory < 8*1024 || p.memory > 256*1024 || p.time < 1 || p.time > 10 || threads < 1 || threads > 16 {
		return p, errors.New("argon2id parameters out of range")
	}
	p.threads = uint8(threads)
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return p, errors.New("bad argon2id salt")
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < 16 || len(p.key) > 64 {
		return p, errors.New("bad argon2id key")
	}
	return p, nil
}
