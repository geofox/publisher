package destination

import (
	"context"
	"time"
)

// Checker is implemented by accounts that can check their connection.
type Checker interface {
	Check(ctx context.Context, req CheckRequest) Health
}

// CheckRequest says what a check may do.
type CheckRequest struct {
	// Authenticated lets the check use the account's credentials. A false
	// value asks for reachability only (readiness probes), with no
	// credential sent.
	Authenticated bool
}

// Health is the outcome of a check.
type Health struct {
	OK        bool `json:"ok"`
	Reachable bool `json:"reachable"`
	// AuthOK is set by authenticated checks: false means the network
	// refused the credentials.
	AuthOK  *bool  `json:"auth_ok,omitempty"`
	Message string `json:"message,omitempty"`
	// ExpiresAt is when the credentials stop working, when known.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Refresher is implemented by accounts whose credentials must be renewed
// before they expire (a Threads long-lived token).
type Refresher interface {
	// NextRefresh is when Refresh should next run; zero means never.
	NextRefresh(now time.Time) time.Time
	// Refresh renews the credentials and saves them through the host.
	Refresh(ctx context.Context) error
}
