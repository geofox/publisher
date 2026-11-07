package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func master(seed byte) []byte {
	b := make([]byte, MasterKeyLen)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func keyring(t *testing.T, seeds ...byte) Keyring {
	t.Helper()
	var kr Keyring
	for i, s := range seeds {
		k, err := NewKey(master(s))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			kr.Current = k
		} else {
			kr.Previous = k
		}
	}
	return kr
}

// The derivation is fixed by the spec: a change would make every stored
// secret unreadable. Expected values computed with an independent RFC 5869
// implementation (Python hmac) for the master key 00 01 … 1f.
func TestKnownAnswerDerivation(t *testing.T) {
	k, err := NewKey(master(0))
	if err != nil {
		t.Fatal(err)
	}
	if k.ID() != "3c573e88060667f3" {
		t.Errorf("key id %s", k.ID())
	}
	if got := hex.EncodeToString(k.enc); got != "351b1e101caf2cfe4dc6c282cd3e3df636dabd984271da7d075f2ae2273a5a76" {
		t.Errorf("enc %s", got)
	}
	if got := hex.EncodeToString(k.idem); got != "4e0f77c742eba80644415a1486701ebdeba809dd3e1e696831b1c5815457a69f" {
		t.Errorf("idem %s", got)
	}
}

var bind = Binding{Owner: "own_default", Scope: ScopeAccount, Ref: "acc_1", Name: "app_password", Audience: "https://pds.example"}

func TestSealOpenRoundTrip(t *testing.T) {
	kr := keyring(t, 0)
	s, err := kr.Seal(bind, "abcd-efgh-ijkl-mnop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "v1."+kr.Current.ID()+".") || strings.Contains(s, "abcd") {
		t.Fatalf("sealed %q", s)
	}
	if kid, _ := KeyID(s); kid != kr.Current.ID() {
		t.Errorf("KeyID %s", kid)
	}
	got, err := kr.Open(bind, s)
	if err != nil || got != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("open %q %v", got, err)
	}
	// A fresh nonce each time.
	s2, _ := kr.Seal(bind, "abcd-efgh-ijkl-mnop")
	if s2 == s {
		t.Error("two seals are equal")
	}
	// The empty value round-trips too.
	s3, _ := kr.Seal(bind, "")
	if got, err := kr.Open(bind, s3); err != nil || got != "" {
		t.Errorf("empty %q %v", got, err)
	}
}

// Every field of the binding is authenticated: changing any one fails.
func TestEachBindingFieldIsAuthenticated(t *testing.T) {
	kr := keyring(t, 0)
	s, _ := kr.Seal(bind, "secret")
	for name, b := range map[string]Binding{
		"owner":       {Owner: "own_other", Scope: bind.Scope, Ref: bind.Ref, Name: bind.Name, Audience: bind.Audience},
		"scope":       {Owner: bind.Owner, Scope: ScopeSetting, Ref: bind.Ref, Name: bind.Name, Audience: bind.Audience},
		"ref":         {Owner: bind.Owner, Scope: bind.Scope, Ref: "acc_2", Name: bind.Name, Audience: bind.Audience},
		"name":        {Owner: bind.Owner, Scope: bind.Scope, Ref: bind.Ref, Name: "access_token", Audience: bind.Audience},
		"audience":    {Owner: bind.Owner, Scope: bind.Scope, Ref: bind.Ref, Name: bind.Name, Audience: "https://evil.example"},
		"no audience": {Owner: bind.Owner, Scope: bind.Scope, Ref: bind.Ref, Name: bind.Name},
		// Fields can't be shifted across the separators.
		"shifted": {Owner: bind.Owner, Scope: bind.Scope, Ref: bind.Ref + "\x00" + bind.Name, Name: "", Audience: bind.Audience},
	} {
		if _, err := kr.Open(b, s); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := kr.Seal(Binding{Ref: "a\x00b"}, "x"); !errors.Is(err, ErrBinding) {
		t.Errorf("NUL in binding: %v", err)
	}
}

func TestTamperingFails(t *testing.T) {
	kr := keyring(t, 0)
	s, _ := kr.Seal(bind, "secret")
	kid, body, _ := split(s)
	for i := range body {
		b := append([]byte(nil), body...)
		b[i] ^= 1
		t2 := "v1." + kid + "." + base64.RawURLEncoding.EncodeToString(b)
		if _, err := kr.Open(bind, t2); !errors.Is(err, ErrOpen) {
			t.Fatalf("byte %d flipped: %v", i, err)
		}
	}
	for _, bad := range []string{"", "secret", "v2." + kid + ".AAAA", "v1.", "v1." + kid, "v1.zz." + "AAAA", "v1." + kid + ".!!!", "v1." + kid + ".AAAA"} {
		if _, err := kr.Open(bind, bad); err == nil {
			t.Errorf("%q opened", bad)
		}
	}
}

func TestUnknownKeyAndPrevious(t *testing.T) {
	old := keyring(t, 7)
	s, _ := old.Seal(bind, "secret")
	cur := keyring(t, 0)
	if _, err := cur.Open(bind, s); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if cur.Knows(old.Current.ID()) {
		t.Error("Knows the old key")
	}
	rot := keyring(t, 0, 7)
	if got, err := rot.Open(bind, s); err != nil || got != "secret" {
		t.Fatalf("previous key: %q %v", got, err)
	}
	out, changed, err := rot.Reseal(bind, s)
	if err != nil || !changed {
		t.Fatalf("reseal %v %v", changed, err)
	}
	if kid, _ := KeyID(out); kid != rot.Current.ID() {
		t.Errorf("resealed under %s", kid)
	}
	if got, err := cur.Open(bind, out); err != nil || got != "secret" {
		t.Errorf("resealed value: %q %v", got, err)
	}
	again, changed, err := rot.Reseal(bind, out)
	if err != nil || changed || again != out {
		t.Errorf("reseal of a current value: %v %v", changed, err)
	}
	// A value that fails to open isn't resealed.
	if _, _, err := rot.Reseal(Binding{Ref: "other"}, s); !errors.Is(err, ErrOpen) {
		t.Errorf("reseal of a moved value: %v", err)
	}
	if _, err := (Keyring{}).Seal(bind, "x"); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
	if _, err := (Keyring{}).Open(bind, out); !errors.Is(err, ErrNoKey) {
		t.Errorf("open with no key: %v", err)
	}
	if _, err := (Keyring{Previous: old.Current}).Open(bind, out); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("open with another previous key: %v", err)
	}
}

func TestParseMasterKeyForms(t *testing.T) {
	// fb ef be encodes as "++++" (or "----"), ff ff as "//8=" (or "__8=").
	m := append(bytes.Repeat([]byte{0xfb, 0xef, 0xbe}, 10), 0xff, 0xff)
	std, url := base64.StdEncoding.EncodeToString(m), base64.URLEncoding.EncodeToString(m)
	if !strings.ContainsAny(std, "+/") {
		t.Fatalf("test key has no +/: %s", std)
	}
	for _, s := range []string{std, strings.TrimRight(std, "="), url, strings.TrimRight(url, "="), "  " + std + "\n"} {
		b, err := ParseMasterKey(s)
		if err != nil || string(b) != string(m) {
			t.Errorf("%q: %v", s, err)
		}
	}
	short := base64.StdEncoding.EncodeToString(m[:16])
	for _, s := range []string{"", "   ", short, "not base64 at all!", std + "AA", hex.EncodeToString(m)} {
		_, err := ParseMasterKey(s)
		if err == nil {
			t.Errorf("%q accepted", s)
			continue
		}
		if s != "" && strings.TrimSpace(s) != "" && strings.Contains(err.Error(), strings.TrimSpace(s)) {
			t.Errorf("error repeats the value: %v", err)
		}
	}
	if g := Generate(); len(g) != 44 {
		t.Errorf("Generate %q", g)
	} else if b, err := ParseMasterKey(g); err != nil || len(b) != MasterKeyLen {
		t.Errorf("Generate doesn't parse: %v", err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	cur, prev := Generate(), Generate()
	kr, warn, err := FromEnv(env(nil))
	if err != nil || kr.HasKey() || len(warn) != 0 {
		t.Fatalf("no key: %v %v", err, warn)
	}
	kr, _, err = FromEnv(env(map[string]string{EnvKey: cur, EnvPreviousKey: prev}))
	if err != nil || kr.Current == nil || kr.Previous == nil {
		t.Fatalf("both keys: %v", err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "key")
	if err := os.WriteFile(f, []byte(cur+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kr2, warn, err := FromEnv(env(map[string]string{EnvKeyFile: f}))
	if err != nil || len(warn) != 0 || kr2.Current.ID() != kr.Current.ID() {
		t.Fatalf("file: %v %v", err, warn)
	}
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, warn, err = FromEnv(env(map[string]string{EnvKeyFile: f})); err != nil || len(warn) != 1 || !strings.Contains(warn[0], EnvKeyFile) {
		t.Errorf("world-readable file: %v %v", err, warn)
	}

	for name, m := range map[string]map[string]string{
		"both forms":       {EnvKey: cur, EnvKeyFile: f},
		"malformed":        {EnvKey: "nope"},
		"missing file":     {EnvKeyFile: filepath.Join(dir, "absent")},
		"previous alone":   {EnvPreviousKey: prev},
		"previous is same": {EnvKey: cur, EnvPreviousKey: cur},
		"bad previous":     {EnvKey: cur, EnvPreviousKey: "short"},
	} {
		_, _, err := FromEnv(env(m))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), cur) || strings.Contains(err.Error(), prev) {
			t.Errorf("%s: error has the key: %v", name, err)
		}
	}
}

func TestMAC(t *testing.T) {
	if (Keyring{}).MAC([]byte("x")) != nil {
		t.Error("MAC without a key")
	}
	a, b := keyring(t, 0), keyring(t, 1)
	if string(a.MAC([]byte("x"))) == string(b.MAC([]byte("x"))) || string(a.MAC([]byte("x"))) == string(a.MAC([]byte("y"))) {
		t.Error("MAC doesn't depend on key and data")
	}
}
