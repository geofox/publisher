package api

import (
	"context"
	"errors"
	"net/http"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/resolve"
)

// apiAccount is an open account in API tests. It never sends; it reports
// a profile and, depending on its type, looks posts up (sourcesResolver)
// and lists relays.
type apiAccount struct {
	id, typ string
	env     *v1Env
}

func (a *apiAccount) Manifest() destination.Manifest {
	d, _ := destination.Lookup(a.typ)
	return d.Manifest()
}
func (a *apiAccount) Capabilities(context.Context) destination.Capabilities {
	return a.Manifest().Defaults
}
func (a *apiAccount) Publish(context.Context, destination.PublishRequest) (destination.PublishResult, error) {
	return destination.PublishResult{}, errors.New("API tests don't send")
}

// resolvingAccount looks posts up like the Bluesky, Mastodon and Nostr
// drivers do.
type resolvingAccount struct{ apiAccount }

func (a *resolvingAccount) ResolveSource(ctx context.Context, in string) (destination.ResolvedSource, error) {
	a.env.lookups = append(a.env.lookups, a.id)
	ref, err := sourcesResolver{}.Resolve(ctx, in)
	if err != nil {
		return destination.ResolvedSource{}, err
	}
	return toResolved(ref), nil
}

// relayAccount is a Nostr account: it looks posts up and has relays.
type nostrAccount struct {
	resolvingAccount
	home   string
	client *http.Client // the account's relay client
}

func (a *nostrAccount) HomeRelay() string { return a.home }
func (a *nostrAccount) OwnerPubkey() gonostr.PubKey {
	var pk gonostr.PubKey
	copy(pk[:], a.id)
	return pk
}
func (a *nostrAccount) ResolveWriteRelays(context.Context) ([]string, error) { return nil, nil }
func (a *nostrAccount) RelayClient() *http.Client                            { return a.client }

var _ pubnostr.RelayLister = (*nostrAccount)(nil)

// withAccounts loads the store's env accounts into a registry (what
// cmd/publisher does), opening each as an API test account: Threads can't
// look posts up, Nostr has relays (its home relay is wss://home.example
// for the first, wss://home-<n>.example for later ones).
func (e *v1Env) withAccounts() *accounts.Registry {
	e.t.Helper()
	rows, err := e.st.AllAccounts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	var env []accounts.EnvAccount
	for _, r := range rows {
		if r.Source == "env" {
			env = append(env, accounts.EnvAccount{Type: r.Type, EnvAccount: destination.EnvAccount{Key: r.EnvKey}})
		}
	}
	nostrs := 0
	reg := accounts.New(accounts.Options{Store: e.st, Env: env,
		Open: func(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
			base := apiAccount{id: req.AccountID, typ: typ, env: e}
			switch typ {
			case pubnostr.Type:
				home := "wss://home.example"
				if nostrs > 0 {
					home = "wss://home-" + string(rune('0'+nostrs)) + ".example"
				}
				nostrs++
				return &nostrAccount{resolvingAccount{base}, home, &http.Client{}}, nil
			case "threads":
				return &base, nil
			}
			return &resolvingAccount{base}, nil
		}})
	if err := reg.Load(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(reg.Close)
	if e.reg != nil {
		e.reg.Close()
	}
	e.reg, e.api.Accounts = reg, reg
	return reg
}

// toResolved is what a driver returns for a SourceRef (FromResolved's
// inverse).
func toResolved(r *resolve.SourceRef) destination.ResolvedSource {
	var media []destination.SourceMedia
	for _, m := range r.Preview.Media {
		media = append(media, destination.SourceMedia{URL: m.URL, Alt: m.Alt, Video: m.Video})
	}
	act := func(c resolve.Cap) destination.ActionAllowed {
		return destination.ActionAllowed{Allowed: c.Allowed, Reason: c.Reason}
	}
	return destination.ResolvedSource{
		Ref: destination.SourceRef{Type: r.Platform, URI: r.Ref.URI, CID: r.Ref.CID, ReplyRootURI: r.Ref.ReplyRootURI,
			ReplyRootCID: r.Ref.ReplyRootCID, LocalID: r.Ref.LocalID, EventID: r.Ref.EventID, Author: r.Ref.Author,
			RelayHints: r.Ref.RelayHints, EventKind: r.Ref.Kind, ThreadRootID: r.Ref.ThreadRootID, Mentions: r.Ref.Mentions},
		Preview: destination.SourcePreview{AuthorName: r.Preview.AuthorName, AuthorHandle: r.Preview.AuthorHandle,
			Text: r.Preview.Text, Media: media, CreatedAt: r.Preview.CreatedAt, WebURL: r.Preview.WebURL},
		Actions: destination.SourceActions{Reply: act(r.Caps.Reply), Quote: act(r.Caps.Quote), Repost: act(r.Caps.Repost)},
	}
}

// syncRegistry tells the registry about an account row the test changed
// in the store (the owner's edit, which the app sends through it).
func (e *v1Env) syncRegistry(id string) {
	e.t.Helper()
	if e.reg == nil {
		return
	}
	rows, err := e.st.AllAccounts(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			if err := e.reg.Update(id, r); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// profiled is the registry with fixed profiles.
type profiled struct {
	*accounts.Registry
	p map[string]accounts.Profile
}

func (r profiled) Profile(id string) (accounts.Profile, bool) {
	p, ok := r.p[id]
	return p, ok
}
