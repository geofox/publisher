package accounts

import (
	"context"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

const (
	profileTTL     = 10 * time.Minute
	profileFailTTL = time.Minute
	profileTimeout = 6 * time.Second
)

// Profile is an account's profile as its network last reported it.
type Profile struct {
	Handle      string `json:"handle,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	ProfileURL  string `json:"profile_url,omitempty"`
	RemoteID    string `json:"remote_id,omitempty"`
	// FetchedAt is when the network last answered (zero: not yet; the
	// handle is then the stored one).
	FetchedAt time.Time `json:"fetched_at,omitzero"`
}

// profileState is an account's cached profile (guarded by Registry.mu).
type profileState struct {
	last     Profile
	at       time.Time // last fetch attempt
	ok       bool      // the last fetch succeeded
	fetching bool
}

// Profile returns the account's last known profile at once: before the
// first answer from its network, the stored handle. A profile older than
// 10 minutes (1 minute after a failure) is fetched again in the
// background; the caller never waits on the network.
func (r *Registry) Profile(id string) (Profile, bool) {
	now := r.now()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return Profile{}, false
	}
	p := e.profile.last
	if p.Handle == "" {
		p.Handle = e.row.Handle
	}
	if p.RemoteID == "" {
		p.RemoteID = e.row.RemoteID
	}
	ttl := profileTTL
	if !e.profile.ok {
		ttl = profileFailTTL
	}
	fetch := e.live != nil && !e.profile.fetching && (e.profile.at.IsZero() || now.Sub(e.profile.at) >= ttl)
	if fetch {
		e.profile.fetching = true
	}
	r.mu.Unlock()
	if fetch {
		r.background(func(ctx context.Context) { r.fetchProfile(ctx, id) })
	}
	return p, true
}

// RefreshProfiles starts fetching every stale profile (at startup, so the
// composer has them when it first asks).
func (r *Registry) RefreshProfiles() {
	for _, id := range r.ids() {
		r.Profile(id)
	}
}

// fetchProfile asks the account's network for its profile and records the
// handle and remote id it reports. Writing them is not an owner's change:
// no Change is emitted, so a refresh and a change can't trigger each other.
func (r *Registry) fetchProfile(ctx context.Context, id string) {
	var ident destination.Identity
	var err error
	l, release := r.leaseAny(id)
	if l != nil {
		if idr, ok := l.acct.(destination.Identifier); ok {
			cctx, cancel := context.WithTimeout(ctx, profileTimeout)
			ident, err = idr.Identity(cctx)
			cancel()
		}
		release()
	}
	now := r.now()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	e.profile.fetching = false
	e.profile.at = now
	e.profile.ok = l != nil && err == nil
	if ident.Handle == "" && ident.RemoteID == "" {
		r.mu.Unlock()
		return // nothing usable (an outage); try again in a minute
	}
	e.profile.last = Profile{Handle: ident.Handle, DisplayName: ident.DisplayName, AvatarURL: ident.AvatarURL,
		ProfileURL: ident.ProfileURL, RemoteID: ident.RemoteID, FetchedAt: now}
	row := e.row
	handle, remote := row.Handle, row.RemoteID
	if ident.Handle != "" {
		handle = ident.Handle
	}
	if ident.RemoteID != "" {
		remote = ident.RemoteID
	}
	changed := handle != row.Handle || remote != row.RemoteID
	if changed {
		e.row.Handle, e.row.RemoteID = handle, remote
	}
	r.mu.Unlock()
	if !changed {
		return
	}
	if row.RemoteID != "" && remote != row.RemoteID && row.Source == "env" {
		r.log.Warn("the environment now configures a different account on the network",
			"account", id, "type", row.Type, "was", row.RemoteID, "now", remote)
	}
	if err := r.opt.Store.SetAccountIdentity(ctx, id, handle, remote); err != nil {
		r.log.Error("account identity not saved", "account", id, "err", err)
	}
}
