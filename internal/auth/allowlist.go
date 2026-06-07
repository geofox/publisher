package auth

import "errors"

// ErrNotAllowed is returned when a verified identity is not on the allowlist.
var ErrNotAllowed = errors.New("identity not allowed")

// Allowlist gates which verified identities may use the app. A subject OR an
// email match is sufficient. Empty sets never match (fail closed).
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
			a.emails[e] = true
		}
	}
	return a
}

// Check returns nil if the claims are allowed, ErrNotAllowed otherwise.
func (a *Allowlist) Check(c Claims) error {
	if a.subjects[c.Subject] || (c.Email != "" && a.emails[c.Email]) {
		return nil
	}
	return ErrNotAllowed
}
