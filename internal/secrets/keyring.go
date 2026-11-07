// Package secrets encrypts credentials at rest under the operator's master
// key (PUBLISHER_MASTER_KEY): AES-256-GCM with a key derived by HKDF, each
// value bound to where it belongs (owner, scope, ref, name and the origin
// it is sent to), so a value copied to another row or pointed at another
// server fails to open.
package secrets

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// MasterKeyLen is the master key's length in bytes.
const MasterKeyLen = 32

// The variables the keyring reads.
const (
	EnvKey             = "PUBLISHER_MASTER_KEY"
	EnvKeyFile         = "PUBLISHER_MASTER_KEY_FILE"
	EnvPreviousKey     = "PUBLISHER_MASTER_KEY_PREVIOUS"
	EnvPreviousKeyFile = "PUBLISHER_MASTER_KEY_PREVIOUS_FILE"
)

// HKDF parameters (spec §3.1). Changing any of them makes every stored
// secret unreadable.
const (
	hkdfSalt     = "publisher"
	infoEnc      = "secrets v1 enc"
	infoKeyID    = "secrets v1 key id"
	infoIdem     = "idempotency v1"
	keyIDLen     = 8 // bytes, shown as 16 hex digits
	encKeyLen    = 32
	idemKeyLen   = 32
	sealedPrefix = "v1."
)

// Key is one master key's derived keys. Its zero value is unusable; build
// it with NewKey.
type Key struct {
	id   string
	enc  []byte
	idem []byte
}

// NewKey derives the keys of a 32-byte master key.
func NewKey(master []byte) (*Key, error) {
	if len(master) != MasterKeyLen {
		return nil, fmt.Errorf("a master key is %d bytes, not %d", MasterKeyLen, len(master))
	}
	enc, err := hkdf.Key(sha256.New, master, []byte(hkdfSalt), infoEnc, encKeyLen)
	if err != nil {
		return nil, err
	}
	kid, err := hkdf.Key(sha256.New, master, []byte(hkdfSalt), infoKeyID, keyIDLen)
	if err != nil {
		return nil, err
	}
	idem, err := hkdf.Key(sha256.New, master, []byte(hkdfSalt), infoIdem, idemKeyLen)
	if err != nil {
		return nil, err
	}
	return &Key{id: hex.EncodeToString(kid), enc: enc, idem: idem}, nil
}

// ID is the key's public id (16 hex digits), stored with every value it
// seals.
func (k *Key) ID() string { return k.id }

// Keyring is the current master key and, during a rotation, the previous
// one. Either may be nil.
type Keyring struct {
	Current  *Key
	Previous *Key
}

// HasKey reports whether a current key is set.
func (kr Keyring) HasKey() bool { return kr.Current != nil }

// key returns the keyring's key with this id.
func (kr Keyring) key(id string) *Key {
	for _, k := range []*Key{kr.Current, kr.Previous} {
		if k != nil && k.id == id {
			return k
		}
	}
	return nil
}

// Knows reports whether the keyring holds the key with this id.
func (kr Keyring) Knows(id string) bool { return kr.key(id) != nil }

// MAC is an HMAC-SHA256 of data under the current key's idempotency key:
// a fingerprint of a secret that reveals nothing about it (the request
// fingerprints of idempotent writes). nil without a current key.
func (kr Keyring) MAC(data []byte) []byte {
	if kr.Current == nil {
		return nil
	}
	m := hmac.New(sha256.New, kr.Current.idem)
	m.Write(data)
	return m.Sum(nil)
}

// Generate returns a new master key, base64 (standard, padded), as
// `publisher keygen` prints it.
func Generate() string {
	var b [MasterKeyLen]byte
	_, _ = rand.Read(b[:]) // crypto/rand doesn't fail on supported platforms
	return base64.StdEncoding.EncodeToString(b[:])
}

// ParseMasterKey reads a master key: 32 bytes in base64, standard or
// URL-safe alphabet, padded or not, surrounding spaces ignored. Errors
// never repeat the value.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("it is empty")
	}
	raw := strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.Strict().DecodeString(raw); err == nil {
			if len(b) != MasterKeyLen {
				return nil, fmt.Errorf("it decodes to %d bytes; a master key is %d (make one with `publisher keygen`)", len(b), MasterKeyLen)
			}
			return b, nil
		}
	}
	return nil, errors.New("it isn't base64 (make one with `publisher keygen`)")
}

// FromEnv reads the master key and, during a rotation, the previous one,
// each from its variable or from the file its _FILE variable names (the
// file's content, trimmed). Both forms of one key, a malformed key, or a
// previous key without a current one is an error naming the variable.
// warnings are worth logging (a key file every user can read).
func FromEnv(getenv func(string) string) (kr Keyring, warnings []string, err error) {
	var errs []error
	read := func(name, fileName string) *Key {
		v, file := getenv(name), strings.TrimSpace(getenv(fileName))
		switch {
		case strings.TrimSpace(v) != "" && file != "":
			errs = append(errs, fmt.Errorf("set %s or %s, not both", name, fileName))
			return nil
		case file != "":
			b, err := os.ReadFile(file)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: can't read %s: %w", fileName, file, unwrapPath(err)))
				return nil
			}
			if fi, err := os.Stat(file); err == nil && fi.Mode().Perm()&0o004 != 0 {
				warnings = append(warnings, fmt.Sprintf("%s: %s can be read by every user on this system; make it readable by Publisher's user only (chmod 600 on the host)", fileName, file))
			}
			v, name = string(b), fileName
		case strings.TrimSpace(v) == "":
			return nil
		}
		master, err := ParseMasterKey(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return nil
		}
		k, err := NewKey(master)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return nil
		}
		return k
	}
	kr.Current = read(EnvKey, EnvKeyFile)
	kr.Previous = read(EnvPreviousKey, EnvPreviousKeyFile)
	if len(errs) == 0 {
		switch {
		case kr.Previous != nil && kr.Current == nil:
			errs = append(errs, fmt.Errorf("%s is set without %s: set the new key as %s and the old one as %s", EnvPreviousKey, EnvKey, EnvKey, EnvPreviousKey))
		case kr.Previous != nil && kr.Previous.id == kr.Current.id:
			errs = append(errs, fmt.Errorf("%s is the same key as %s: remove it", EnvPreviousKey, EnvKey))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Keyring{}, nil, err
	}
	return kr, warnings, nil
}

// unwrapPath drops the path os.ReadFile repeats in its error.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// EnvVars are the variables FromEnv reads, for unsetting once read.
func EnvVars() []string {
	return []string{EnvKey, EnvKeyFile, EnvPreviousKey, EnvPreviousKeyFile}
}
