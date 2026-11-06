// Command harness serves the real Publisher web app and API for the Playwright
// end-to-end suite (see e2e/README.md). Everything from the browser down to
// SQLite is the production code — api routes, middleware, account registry,
// dispatcher, threading, store, migrations, embedded assets. Only the network edge is
// fake: each account (harnessAccounts: several of a network, one the
// environment doesn't configure) records what it would have sent, and a
// same-origin fake Blossom server stores uploads in memory.
//
// It is a test tool: it never ships in the image, binds to 127.0.0.1 only,
// and deletes its temporary database on exit.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
)

func main() {
	if err := run(); err != nil {
		slog.Error("e2e harness failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "4173"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return err
	}
	base := "http://" + ln.Addr().String()

	dir, err := os.MkdirTemp("", "publisher-e2e-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "e2e.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	// Blossom lives on the same origin so the app's CSP (img-src 'self' https:)
	// lets the browser render uploaded thumbnails, as it would for a real
	// https Blossom server.
	blossom := newFakeBlossom(base + "/__blossom")
	sk := gonostr.Generate()
	mp := media.New(base+"/__blossom", sk, gonostr.GetPublicKey(sk))
	mp.Lookup = st.LookupMediaURL

	rec := newRecorder()
	drivers.Register()
	env, err := seedAccounts(context.Background(), st, harnessAccounts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng, err := newEngine(ctx, func(ctx context.Context) (*accounts.Registry, *dispatch.Dispatcher, error) {
		reg := accounts.New(accounts.Options{Store: st, Env: env,
			Open: func(_ context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
				return newFake(rec, typ, req.AccountID, req.Config)
			}})
		if err := reg.Load(ctx); err != nil {
			return nil, nil, err
		}
		return reg, &dispatch.Dispatcher{
			Registry: reg,
			Store:    st,
			Fetcher:  mp,
			Policy:   dispatch.RetryPolicy{Enabled: true, MaxAttempts: 6, Base: 2 * time.Minute, Max: time.Hour},
		}, nil
	})
	if err != nil {
		return err
	}
	rec.restart = eng.restart
	a := api.New(mp)
	a.Store = st
	a.Dispatch = eng
	a.Caps = eng.Caps
	a.Accounts = eng
	a.Sync = fakeSyncer{}
	a.UserLanguages = []string{"en", "fr"}
	a.Ready = &health.Checker{Local: []health.Check{health.Database(st, store.LatestSchemaVersion())}}

	mux := http.NewServeMux()
	mux.Handle("/__e2e/", rec.handler())
	mux.Handle("/__blossom/", http.StripPrefix("/__blossom", blossom))
	mux.Handle("/", a.Routes())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = eng.shutdown(sctx)
	}()
	slog.Info("e2e harness listening", "url", base, "db", dir)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// harnessAccount is one account the harness stores at startup.
type harnessAccount struct {
	Type, Key, Label string
	// NotConfigured leaves the account out of the environment, as when its
	// variables were removed after an earlier start.
	NotConfigured bool
	// NotDefault: the composer doesn't select it by default.
	NotDefault bool
	// MaxLength replaces the driver's text limit (0 keeps it).
	MaxLength int
}

// harnessAccounts are the accounts every spec runs with, in registry order
// within a type: the first connected account of each type is the one the
// composer (one row per type until part A3) posts as, so specs written
// for one account per type see the four they expect.
var harnessAccounts = []harnessAccount{
	{Type: nostr.Type, Key: destination.EnvKeyDefault, Label: "Nostr"},
	{Type: mastodon.Type, Key: destination.EnvKeyDefault, Label: "Mastodon"},
	{Type: mastodon.Type, Key: "2", Label: "Mastodon (long posts)", MaxLength: 1000},
	{Type: mastodon.Type, Key: "old", Label: "Mastodon (old)", NotConfigured: true},
	{Type: bluesky.Type, Key: destination.EnvKeyDefault, Label: "Bluesky"},
	{Type: bluesky.Type, Key: "2", Label: "Bluesky (alt)", NotDefault: true},
	{Type: threads.Type, Key: destination.EnvKeyDefault, Label: "Threads"},
}

// seedAccounts stores the accounts as a start does (store.SeedEnvAccounts,
// in the order given) and returns the environment's list for the registry:
// the configured ones, each with its harness config.
func seedAccounts(ctx context.Context, st *store.Store, accts []harnessAccount) ([]accounts.EnvAccount, error) {
	var seed []store.EnvAccount
	var env []accounts.EnvAccount
	for _, a := range accts {
		seed = append(seed, store.EnvAccount{Type: a.Type, Key: a.Key, Label: a.Label})
		if a.NotConfigured {
			continue
		}
		var config map[string]any
		if a.MaxLength > 0 {
			config = map[string]any{configMaxLength: a.MaxLength}
		}
		env = append(env, accounts.EnvAccount{Type: a.Type, EnvAccount: destination.EnvAccount{Key: a.Key, Config: config}})
	}
	rows, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, seed)
	if err != nil {
		return nil, err
	}
	off := false
	for _, a := range accts {
		if !a.NotDefault {
			continue
		}
		for _, r := range rows {
			if r.Type == a.Type && r.EnvKey == a.Key {
				if _, err := st.UpdateAccountMeta(ctx, store.DefaultOwner, r.ID, store.AccountMeta{DefaultSelected: &off}); err != nil {
					return nil, err
				}
			}
		}
	}
	return env, nil
}
