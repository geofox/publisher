package accounts

import (
	"errors"
	"fmt"
	"strings"

	"github.com/geofox/publisher/internal/destination"
)

// Status says whether an account can be used, and if not, why.
type Status string

const (
	// Connected: open and usable.
	Connected Status = "connected"
	// Disabled: the owner turned it off.
	Disabled Status = "disabled"
	// NotConfigured: an account from the environment whose variables are
	// gone, or of a network this version has no driver for.
	NotConfigured Status = "not_configured"
	// Locked: its credentials can't be opened now (its saved state can't
	// be read; an account added in the app before part C). Its posts wait.
	Locked Status = "locked"
	// NeedsReconnect: the network refused its credentials, confirmed by an
	// authenticated check.
	NeedsReconnect Status = "needs_reconnect"
	// Deleted: removed by the owner (part C).
	Deleted Status = "deleted"
)

// Unavailable is the error Acquire returns for an account that can't be
// used now.
type Unavailable struct {
	ID     string
	Status Status
	Reason string
}

func (u *Unavailable) Error() string {
	what := map[Status]string{
		Disabled:       "is turned off",
		NotConfigured:  "is not configured",
		Locked:         "is locked",
		NeedsReconnect: "needs to be connected again",
		Deleted:        "was deleted",
	}[u.Status]
	if what == "" {
		what = "is " + string(u.Status)
	}
	if u.Reason == "" {
		return "the account " + what
	}
	return "the account " + what + ": " + u.Reason
}

// ErrNoAccount: no account has that id.
var ErrNoAccount = errors.New("no such account")

// ErrClosed: the registry was closed (the server is stopping); the
// account may be fine, so a send that meets it is tried again.
var ErrClosed = errors.New("accounts are closed: the server is stopping")

// IsUnavailable reports whether err says an account can't be used, and
// returns the details.
func IsUnavailable(err error) (*Unavailable, bool) {
	var u *Unavailable
	ok := errors.As(err, &u)
	return u, ok
}

// notConfiguredReason tells the operator what to set for an account of
// type d from the environment.
func notConfiguredReason(d destination.Driver) string {
	src, ok := d.(destination.EnvSource)
	if !ok {
		return "its variables are no longer set"
	}
	var names []string
	for _, v := range src.EnvVars() {
		if v.Required {
			names = append(names, v.Name)
		}
	}
	if len(names) == 0 {
		return "its variables are no longer set"
	}
	return "set " + joinAnd(names) + " to use it again"
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	default:
		return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
	}
}

// openProblem describes why Open refused an account. Driver errors name
// fields, never values (contract rule).
func openProblem(err error) string {
	return fmt.Sprintf("its settings can't be used (%v)", err)
}
