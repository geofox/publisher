package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The word list is BIP 39's English list, unchanged (its licence names
// this hash).
func TestWordList(t *testing.T) {
	sum := sha256.Sum256([]byte(wordData))
	if got := hex.EncodeToString(sum[:]); got != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("bip39-english.txt sha256 = %s", got)
	}
	if len(wordList) != 2048 || len(wordIndex) != 2048 {
		t.Fatalf("%d words, %d distinct", len(wordList), len(wordIndex))
	}
}

func TestPassphrase(t *testing.T) {
	a, err := NewPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewPassphrase()
	if a == b {
		t.Fatal("two passphrases alike")
	}
	ws := strings.Fields(a)
	if len(ws) != Words || strings.Join(ws, " ") != a {
		t.Fatalf("passphrase %q", a)
	}
	for _, w := range ws {
		if !wordIndex[w] {
			t.Fatalf("%q isn't a list word", w)
		}
	}
	// As typed or pasted: any case, any spacing.
	if got, err := ParsePassphrase("\n " + strings.ToUpper(strings.ReplaceAll(a, " ", "\t\n  ")) + " "); err != nil || got != a {
		t.Fatalf("parse = %q, %v", got, err)
	}
	for _, bad := range []string{"", strings.Join(ws[:23], " "), a + " abandon", strings.Replace(a, ws[3], "zebraa", 1),
		strings.Replace(a, ws[0], ws[0]+"-", 1)} {
		if _, err := ParsePassphrase(bad); !errors.Is(err, ErrPassphraseForm) {
			t.Fatalf("parse %q: %v", bad, err)
		}
	}
}

func seal(t *testing.T, plain string) ([]byte, string) {
	t.Helper()
	pass, err := NewPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Seal([]byte(plain), pass, time.Date(2026, 11, 7, 9, 30, 15, 500, time.FixedZone("x", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	return f, pass
}

func TestRoundTrip(t *testing.T) {
	f, pass := seal(t, `{"secret":"canary-91ab"}`)
	if bytes.Contains(f, []byte("canary")) || bytes.Contains(f, []byte(pass)) {
		t.Fatal("the file holds its contents or passphrase in the clear")
	}
	lines := strings.Split(strings.TrimSuffix(string(f), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	h, plain, err := Open(f, strings.ToUpper(pass))
	if err != nil || string(plain) != `{"secret":"canary-91ab"}` {
		t.Fatalf("open = %q, %v", plain, err)
	}
	if h.Format != Format || h.Version != Version || h.KDF != KDF || h.T != KDFTime || h.M != KDFMemory || h.P != KDFLanes ||
		!h.CreatedAt.Equal(time.Date(2026, 11, 7, 8, 30, 15, 0, time.UTC)) {
		t.Fatalf("header %+v", h)
	}
	// Line ends a Windows editor wrote, and no final one, still open.
	crlf := bytes.ReplaceAll(bytes.TrimSuffix(f, []byte("\n")), []byte("\n"), []byte("\r\n"))
	if _, _, err := Open(crlf, pass); err != nil {
		t.Fatalf("CRLF: %v", err)
	}
	// Each file has its own salt and nonce.
	g, _ := seal(t, "x")
	hf, _, _, _ := ReadHeader(f)
	hg, _, _, _ := ReadHeader(g)
	if hf.Salt == hg.Salt || hf.Nonce == hg.Nonce {
		t.Fatal("salt or nonce reused")
	}
}

func TestOpenRefusals(t *testing.T) {
	f, pass := seal(t, "{}")
	other, _ := NewPassphrase()
	head, body, _ := strings.Cut(string(f), "\n")
	edit := func(old, new string) []byte { return []byte(strings.Replace(head, old, new, 1) + "\n" + body) }
	for _, c := range []struct {
		name string
		file []byte
		pass string
		want error
	}{
		{"wrong passphrase", f, other, ErrDoesntOpen},
		{"passphrase form", f, "one two", ErrPassphraseForm},
		{"header changed", edit(`"created_at":"2026`, `"created_at":"2027`), pass, ErrDoesntOpen},
		{"header spacing", edit(`"version":1`, `"version": 1`), pass, ErrDoesntOpen},
		{"time", edit(`"t":3`, `"t":1`), pass, ErrParams},
		{"memory", edit(`"m":65536`, `"m":4194304`), pass, ErrParams},
		{"lanes", edit(`"p":4`, `"p":255`), pass, ErrParams},
		{"kdf", edit(`"kdf":"argon2id"`, `"kdf":"pbkdf2"`), pass, ErrParams},
		{"newer", edit(`"version":1`, `"version":2`), pass, ErrVersion},
		{"older", edit(`"version":1`, `"version":0`), pass, ErrNotExport},
		{"format", edit(`"publisher-export"`, `"other"`), pass, ErrNotExport},
		{"unknown field", edit(`{"format"`, `{"x":1,"format"`), pass, ErrNotExport},
		{"two values", []byte(head + "{}\n" + body), pass, ErrNotExport},
		{"short salt", edit(`"salt":"`, `"salt":"AA`), pass, ErrNotExport},
		{"no body", []byte(head + "\n"), pass, ErrNotExport},
		{"one line", []byte(head), pass, ErrNotExport},
		{"spaces in body", []byte(head + "\n" + body[:10] + " " + body[10:]), pass, ErrNotExport},
		{"padding", []byte(head + "\n" + strings.TrimSpace(body) + "=="), pass, ErrNotExport},
		{"not base64url", []byte(head + "\n+" + body[1:]), pass, ErrNotExport},
		{"empty", nil, pass, ErrNotExport},
		{"too large", bytes.Repeat([]byte("a"), MaxFileBytes+1), pass, ErrTooLarge},
		{"long header", []byte(`{"format":"` + strings.Repeat("a", maxHeaderBytes) + "\"}\n" + body), pass, ErrNotExport},
	} {
		if _, _, err := Open(c.file, c.pass); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A changed byte of the contents.
	b := []byte(body)
	if b[20] == 'A' {
		b[20] = 'B'
	} else {
		b[20] = 'A'
	}
	if _, _, err := Open([]byte(head+"\n"+string(b)), pass); !errors.Is(err, ErrDoesntOpen) {
		t.Fatalf("contents changed: %v", err)
	}
	// No error says what the file holds.
	for _, err := range []error{ErrNotExport, ErrVersion, ErrParams, ErrDoesntOpen, ErrPassphraseForm, ErrTooLarge} {
		if strings.Contains(err.Error(), pass) {
			t.Fatal("an error holds the passphrase")
		}
	}
}

func TestSealTooLarge(t *testing.T) {
	pass, _ := NewPassphrase()
	if _, err := Seal(bytes.Repeat([]byte("a"), MaxFileBytes), pass, time.Now()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("seal: %v", err)
	}
	if _, err := Seal([]byte("{}"), "not a passphrase", time.Now()); !errors.Is(err, ErrPassphraseForm) {
		t.Fatalf("seal: %v", err)
	}
}

func TestDecode(t *testing.T) {
	good := Contents{Version: ContentsVersion, Accounts: []Account{{ID: "a", Source: SourceApp}, {ID: "b", Source: SourceEnv}}}
	b, _ := json.Marshal(good)
	if c, err := Decode(b); err != nil || len(c.Accounts) != 2 {
		t.Fatalf("decode = %+v, %v", c, err)
	}
	many := Contents{Version: ContentsVersion}
	for i := range maxAccounts + 1 {
		many.Accounts = append(many.Accounts, Account{ID: string(rune('a' + i%26)), Source: SourceApp})
	}
	for name, c := range map[string]Contents{
		"version":   {Version: 2},
		"no id":     {Version: 1, Accounts: []Account{{Source: SourceApp}}},
		"same id":   {Version: 1, Accounts: []Account{{ID: "a", Source: SourceApp}, {ID: "a", Source: SourceEnv}}},
		"source":    {Version: 1, Accounts: []Account{{ID: "a", Source: "db"}}},
		"too many":  many,
		"many sets": {Version: 1, Sets: make([]Set, maxSets+1)},
	} {
		b, _ := json.Marshal(c)
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := Decode([]byte(`{"version":1,"accounts":[{"id":"canary-x","enabled":"yes"}]}`)); err == nil ||
		strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "yes") {
		t.Fatalf("a bad shape: %v", err)
	}
}
