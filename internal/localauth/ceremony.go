package localauth

import (
	"crypto/rand"
	"encoding/base64"
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

// Ceremony limits: a ceremony lives 5 minutes; at most 1000 are kept (the
// oldest dropped); an address may begin 20 a minute.
const (
	CeremonyTTL         = 5 * time.Minute
	maxCeremonies       = 1000
	beginsPerMinute     = 20
	beginRateWindow     = time.Minute
	maxTrackedBeginners = 4096
)

// ErrRateLimited: the address began too many ceremonies; Wait says when it
// may begin again.
type ErrRateLimited struct{ Wait time.Duration }

func (e *ErrRateLimited) Error() string { return "too many sign-in attempts; try again shortly" }

// ErrCeremony: the ceremony is unknown, expired, or was begun for another
// purpose, operation or session.
var ErrCeremony = errors.New("this passkey request expired or doesn't match; start again")

// ceremony is one passkey request in flight, held in memory: a restart
// ends it, which only means starting again.
type ceremony struct {
	purpose Purpose
	binding string // the session hash (register, reauth) or setup state id (setup)
	op      string // reauth: the operation
	data    webauthn.SessionData
	expires time.Time
}

// Ceremonies holds the ceremonies in flight, by a random id the browser
// keeps in a cookie.
type Ceremonies struct {
	mu     sync.Mutex
	m      map[string]*ceremony
	order  []string
	begins map[string][]time.Time
	now    func() time.Time
}

// NewCeremonies returns an empty set.
func NewCeremonies() *Ceremonies {
	return &Ceremonies{m: map[string]*ceremony{}, begins: map[string][]time.Time{}, now: time.Now}
}

func randomID() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// start keeps a new ceremony begun from addr (an AddrKey) and returns its
// id.
func (c *Ceremonies) start(addr string, cer ceremony) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	recent := c.begins[addr][:0]
	for _, t := range c.begins[addr] {
		if now.Sub(t) < beginRateWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= beginsPerMinute {
		c.begins[addr] = recent
		return "", &ErrRateLimited{Wait: recent[0].Add(beginRateWindow).Sub(now).Truncate(time.Second) + time.Second}
	}
	if len(c.begins) >= maxTrackedBeginners {
		c.pruneBegins(now)
	}
	c.begins[addr] = append(recent, now)
	// Drop expired ceremonies, then the oldest while over the cap.
	kept := c.order[:0]
	for _, id := range c.order {
		if x, ok := c.m[id]; ok && now.Before(x.expires) {
			kept = append(kept, id)
		} else {
			delete(c.m, id)
		}
	}
	c.order = kept
	for len(c.order) >= maxCeremonies {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	id := randomID()
	cer.expires = now.Add(CeremonyTTL)
	c.m[id] = &cer
	c.order = append(c.order, id)
	return id, nil
}

// pruneBegins forgets addresses with no begin in the window.
func (c *Ceremonies) pruneBegins(now time.Time) {
	for a, ts := range c.begins {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= beginRateWindow {
			delete(c.begins, a)
		}
	}
}

// take removes and returns a ceremony (once), if it is alive and was
// begun for purpose with this binding (and operation, for reauth).
func (c *Ceremonies) take(id string, purpose Purpose, binding, op string) (ceremony, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.m[id]
	if !ok {
		return ceremony{}, ErrCeremony
	}
	delete(c.m, id)
	if !c.now().Before(x.expires) || x.purpose != purpose || x.binding != binding || x.op != op {
		return ceremony{}, ErrCeremony
	}
	return *x, nil
}
