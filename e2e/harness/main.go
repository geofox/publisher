// Command harness serves the real Publisher web app and API for the Playwright
// end-to-end suite (see e2e/README.md). Everything from the browser down to
// SQLite is the production code — api routes, middleware, account registry,
// dispatcher, threading, store, migrations, embedded assets. Only the network edge is
// fake: the four platform posters record what they would have sent, and a
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

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
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
	sk := nostr.Generate()
	mp := media.New(base+"/__blossom", sk, nostr.GetPublicKey(sk))
	mp.Lookup = st.LookupMediaURL

	rec := newRecorder()
	drivers.Register()
	fakes := fakeAccounts(rec)
	// As in production: every account the environment configures has a
	// stored row, and the registry opens it (here, as a recording fake).
	var seed []store.EnvAccount
	var env []accounts.EnvAccount
	for _, typ := range destination.TypesInOrder() {
		acct, ok := fakes[typ]
		if !ok {
			continue
		}
		seed = append(seed, store.EnvAccount{Type: typ, Key: destination.EnvKeyDefault, Label: acct.Manifest().Name})
		env = append(env, accounts.EnvAccount{Type: typ, EnvAccount: destination.EnvAccount{Key: destination.EnvKeyDefault}})
	}
	if _, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner, seed); err != nil {
		return err
	}
	// A second Mastodon account that the environment doesn't configure:
	// the composer must keep posting as the connected one, and /v1 must
	// refuse to send as it.
	if err := st.InsertAccount(context.Background(), store.Account{ID: "acc_e2esecondmastodon", Type: mastodon.Type,
		Label: "Mastodon (second)", EnvKey: "second", Enabled: true, DefaultSelected: true, PublicFeed: true,
		CreatedAt: time.Now().UTC().Add(time.Second)}); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng, err := newEngine(ctx, func(ctx context.Context) (*accounts.Registry, *dispatch.Dispatcher, error) {
		reg := accounts.New(accounts.Options{Store: st, Env: env,
			Open: func(_ context.Context, typ string, _ destination.OpenRequest) (destination.Account, error) {
				if a, ok := fakes[typ]; ok {
					return a, nil
				}
				return nil, destination.Invalidf("no fake for %s", typ)
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
