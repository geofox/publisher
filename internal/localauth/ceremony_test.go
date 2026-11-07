package localauth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestCeremonySealOpenSpend(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c := NewCeremonies()
	c.now = func() time.Time { return now }
	cer := ceremony{Purpose: PurposeReauth, Binding: "sess", Op: "signin.add", Data: webauthn.SessionData{Challenge: "ch1"}}
	sealed, err := c.start("192.0.2.1", cer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "sess") || strings.Contains(sealed, "ch1") {
		t.Fatal("the sealed ceremony is readable")
	}
	for _, tc := range []struct {
		name             string
		purpose          Purpose
		binding, op, raw string
	}{
		{"another purpose", PurposeLogin, "sess", "signin.add", sealed},
		{"another session", PurposeReauth, "other", "signin.add", sealed},
		{"another operation", PurposeReauth, "sess", "signin.remove", sealed},
		{"tampered", PurposeReauth, "sess", "signin.add", sealed[:len(sealed)-2] + "AA"},
		{"empty", PurposeReauth, "sess", "signin.add", ""},
		{"garbage", PurposeReauth, "sess", "signin.add", "not base64!"},
	} {
		if _, err := c.open(tc.raw, tc.purpose, tc.binding, tc.op); !errors.Is(err, ErrCeremony) {
			t.Errorf("%s: %v, want ErrCeremony", tc.name, err)
		}
	}
	got, err := c.open(sealed, PurposeReauth, "sess", "signin.add")
	if err != nil || got.Data.Challenge != "ch1" {
		t.Fatalf("open: %+v %v", got, err)
	}
	// Opening doesn't use it; spending does, once.
	if _, err := c.open(sealed, PurposeReauth, "sess", "signin.add"); err != nil {
		t.Fatalf("open again before spending: %v", err)
	}
	if err := c.spend(got); err != nil {
		t.Fatal(err)
	}
	if err := c.spend(got); !errors.Is(err, ErrCeremony) {
		t.Fatalf("spent twice: %v", err)
	}
	if _, err := c.open(sealed, PurposeReauth, "sess", "signin.add"); !errors.Is(err, ErrCeremony) {
		t.Fatalf("open after spending: %v", err)
	}
	// Another key (a restart) can't open it.
	if _, err := NewCeremonies().open(sealed, PurposeReauth, "sess", "signin.add"); !errors.Is(err, ErrCeremony) {
		t.Fatalf("opened with another key: %v", err)
	}
	// Expired.
	s2, _ := c.start("192.0.2.1", ceremony{Purpose: PurposeLogin, Data: webauthn.SessionData{Challenge: "ch2"}})
	now = now.Add(CeremonyTTL)
	if _, err := c.open(s2, PurposeLogin, "", ""); !errors.Is(err, ErrCeremony) {
		t.Fatalf("expired: %v", err)
	}
}

// Begins from other addresses never crowd out a ceremony (the old pool
// dropped the oldest past 1000).
func TestCeremoniesCantBeCrowdedOut(t *testing.T) {
	c := NewCeremonies()
	mine, err := c.start("203.0.113.5", ceremony{Purpose: PurposeLogin, Data: webauthn.SessionData{Challenge: "mine"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err := c.start(fmt.Sprintf("2001:db8:%x::/64", i), ceremony{Purpose: PurposeLogin, Data: webauthn.SessionData{Challenge: fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.open(mine, PurposeLogin, "", ""); err != nil {
		t.Fatalf("crowded out: %v", err)
	}
}

func TestCeremonyBeginsAndFailuresAreLimited(t *testing.T) {
	c := NewCeremonies()
	for i := range beginsPerMinute - 2 {
		if _, err := c.start("a", ceremony{Purpose: PurposeLogin}); err != nil {
			t.Fatalf("begin %d: %v", i+1, err)
		}
	}
	c.failed("a")
	c.failed("a")
	var rl *ErrRateLimited
	if err := c.limited("a"); !errors.As(err, &rl) || rl.Wait <= 0 {
		t.Fatalf("finish after %d begins and failures: %v", beginsPerMinute, err)
	}
	if _, err := c.start("a", ceremony{Purpose: PurposeLogin}); !errors.As(err, &rl) {
		t.Fatalf("begin: %v", err)
	}
	if err := c.limited("b"); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

// A ceremony too large for the cookie is refused, whether its plain form
// already is or only the sealed one would be.
func TestCeremonyTooLargeIsRefused(t *testing.T) {
	c := NewCeremonies()
	for name, n := range map[string]int{"plain": maxSealedLen + 1, "sealed": maxSealedLen * 3 / 4} {
		cer := ceremony{Purpose: PurposeLogin, Data: webauthn.SessionData{Challenge: strings.Repeat("c", n)}}
		if _, err := c.start(fmt.Sprintf("192.0.2.%d", len(name)), cer); !errors.Is(err, errCeremonyTooLarge) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.start("192.0.2.9", ceremony{Purpose: PurposeLogin, Data: webauthn.SessionData{Challenge: "ch"}}); err != nil {
		t.Fatal(err)
	}
}
