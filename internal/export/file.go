// Package export is Publisher's export file (spec §9): an owner's accounts
// and settings, credentials included, sealed with a passphrase the server
// makes and shows once.
//
// The file is two lines of text: a JSON header, then the sealed contents
// in base64url (no padding). The contents are AES-256-GCM under a key
// argon2id derives from the passphrase, with the header's exact bytes as
// additional data, so the header can't be changed either. The KDF's
// parameters are pinned: a file that names others is refused before any
// work is done.
package export

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// The file's format and the KDF's pinned parameters (RFC 9106's second
// recommended option, as passwords use).
const (
	Format  = "publisher-export"
	Version = 1

	KDF       = "argon2id"
	KDFTime   = 3
	KDFMemory = 64 * 1024 // KiB
	KDFLanes  = 4

	saltLen = 16
	keyLen  = 32

	// Words is how many words a passphrase has: 24 of 2048, 264 bits.
	Words = 24
	// MaxFileBytes bounds a file read for import.
	MaxFileBytes = 768 << 10 // within the API's 1 MiB body, as JSON
	// maxHeaderBytes bounds the header line.
	maxHeaderBytes = 1024
)

// Errors of Open and ParsePassphrase. None carries a secret.
var (
	// ErrNotExport: not an export file of this format.
	ErrNotExport = errors.New("this isn't a Publisher export file")
	// ErrVersion: a format version this server doesn't read.
	ErrVersion = errors.New("this export file is from a newer Publisher: upgrade this server first")
	// ErrParams: KDF parameters other than the pinned ones.
	ErrParams = errors.New("this export file names key parameters Publisher doesn't use")
	// ErrDoesntOpen: the passphrase is wrong, or the file was changed.
	ErrDoesntOpen = errors.New("the passphrase doesn't open this file (or the file was changed)")
	// ErrPassphraseForm: not 24 words of the list.
	ErrPassphraseForm = errors.New("the passphrase is 24 words, as they were shown")
	// ErrTooLarge: larger than an export file can be.
	ErrTooLarge = errors.New("this file is larger than a Publisher export file can be")
)

//go:embed bip39-english.txt
var wordData string

var (
	wordList  = strings.Fields(wordData)
	wordIndex = func() map[string]bool {
		m := make(map[string]bool, len(wordList))
		for _, w := range wordList {
			m[w] = true
		}
		return m
	}()
)

// Header is the file's first line.
type Header struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	T       uint32 `json:"t"`
	M       uint32 `json:"m"`
	P       uint8  `json:"p"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	// CreatedAt is when it was made (UTC), shown before importing.
	CreatedAt time.Time `json:"created_at"`
}

// NewPassphrase makes a passphrase: Words words of the list, each chosen
// uniformly (the list has 2048 words: 11 bits from 2 random bytes).
func NewPassphrase() (string, error) {
	if len(wordList) != 2048 {
		return "", fmt.Errorf("export: the word list has %d words", len(wordList))
	}
	b := make([]byte, 2*Words)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]string, Words)
	for i := range out {
		out[i] = wordList[binary.BigEndian.Uint16(b[2*i:])&0x7ff]
	}
	return strings.Join(out, " "), nil
}

// ParsePassphrase reads a passphrase as the owner typed or pasted it:
// any case, any spaces or line breaks between the words.
func ParsePassphrase(s string) (string, error) {
	ws := strings.Fields(strings.ToLower(s))
	if len(ws) != Words {
		return "", ErrPassphraseForm
	}
	for _, w := range ws {
		if !wordIndex[w] {
			return "", ErrPassphraseForm
		}
	}
	return strings.Join(ws, " "), nil
}

func deriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, KDFTime, KDFMemory, KDFLanes, keyLen)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

var b64 = base64.RawURLEncoding

// Seal makes a file of plain under passphrase (as NewPassphrase made it).
func Seal(plain []byte, passphrase string, now time.Time) ([]byte, error) {
	pass, err := ParsePassphrase(passphrase)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := gcm(deriveKey(pass, salt))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	head, err := json.Marshal(Header{Format: Format, Version: Version, KDF: KDF, T: KDFTime, M: KDFMemory, P: KDFLanes,
		Salt: b64.EncodeToString(salt), Nonce: b64.EncodeToString(nonce), CreatedAt: now.UTC().Truncate(time.Second)})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(head)
	out.WriteByte('\n')
	out.WriteString(b64.EncodeToString(aead.Seal(nil, nonce, plain, head)))
	out.WriteByte('\n')
	if out.Len() > MaxFileBytes {
		return nil, ErrTooLarge
	}
	return out.Bytes(), nil
}

// ReadHeader reads and checks a file's header, without the passphrase:
// its format, version and pinned parameters. It answers the header's
// exact bytes too (the contents' additional data) and the sealed part.
func ReadHeader(file []byte) (Header, []byte, []byte, error) {
	if len(file) > MaxFileBytes {
		return Header{}, nil, nil, ErrTooLarge
	}
	line, rest, ok := bytes.Cut(file, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if !ok || len(line) == 0 || len(line) > maxHeaderBytes {
		return Header{}, nil, nil, ErrNotExport
	}
	var h Header
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil || dec.More() || h.Format != Format {
		return Header{}, nil, nil, ErrNotExport
	}
	switch {
	case h.Version > Version:
		return Header{}, nil, nil, ErrVersion
	case h.Version != Version:
		return Header{}, nil, nil, ErrNotExport
	case h.KDF != KDF || h.T != KDFTime || h.M != KDFMemory || h.P != KDFLanes:
		return Header{}, nil, nil, ErrParams
	}
	body := bytes.TrimSpace(rest)
	if len(body) == 0 || bytes.ContainsAny(body, " \r\n\t") {
		return Header{}, nil, nil, ErrNotExport
	}
	return h, line, body, nil
}

// Open reads a file with its passphrase: the header, and the contents.
// The KDF runs only for a file whose header is in order.
func Open(file []byte, passphrase string) (Header, []byte, error) {
	h, head, body, err := ReadHeader(file)
	if err != nil {
		return Header{}, nil, err
	}
	pass, err := ParsePassphrase(passphrase)
	if err != nil {
		return Header{}, nil, err
	}
	salt, err := b64.Strict().DecodeString(h.Salt)
	if err != nil || len(salt) != saltLen {
		return Header{}, nil, ErrNotExport
	}
	nonce, err := b64.Strict().DecodeString(h.Nonce)
	if err != nil || len(nonce) != 12 {
		return Header{}, nil, ErrNotExport
	}
	sealed, err := b64.Strict().DecodeString(string(body))
	if err != nil {
		return Header{}, nil, ErrNotExport
	}
	aead, err := gcm(deriveKey(pass, salt))
	if err != nil {
		return Header{}, nil, err
	}
	plain, err := aead.Open(nil, nonce, sealed, head)
	if err != nil {
		return Header{}, nil, ErrDoesntOpen
	}
	return h, plain, nil
}
