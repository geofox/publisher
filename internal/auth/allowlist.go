package auth

import (
	"errors"
	"strings"
)

// ErrNotAllowed is returned when a verified identity is not on the allowlist.
var ErrNotAllowed = errors.New("identity not allowed")

// Allowlist gates which verified identities may use the app. A subject OR a
// verified email match is sufficient; emails compare case-insensitively.
// Empty sets never match (fail closed).
type Allowlist struct {
	subjects map[string]bool
	emails   map[string]bool
}

func NewAllowlist(subjects, emails []string) *Allowlist {
	a := &Allowlist{subjects: map[string]bool{}, emails: map[string]bool{}}
	for _, s := range subjects {
		if s != "" {
			a.subjects[s] = true
		}
	}
	for _, e := range emails {
		if e != "" {
			a.emails[strings.ToLower(e)] = true
		}
	}
	return a
}

// Check returns nil if the claims are allowed, ErrNotAllowed otherwise.
func (a *Allowlist) Check(c Claims) error {
	if a.subjects[c.Subject] || (c.Email != "" && c.EmailVerified && a.emails[strings.ToLower(c.Email)]) {
		return nil
	}
	return ErrNotAllowed
}

// AllowsUser reports whether a signed-in user is still allowed: the list can
// change (a restart with a new OIDC_ALLOWED_*) while their session lives.
// The stored email is only ever a verified one (see the login callback).
func (a *Allowlist) AllowsUser(subject, email string) bool {
	if a == nil {
		return false
	}
	return a.Check(Claims{Subject: subject, Email: email, EmailVerified: email != ""}) == nil
}
