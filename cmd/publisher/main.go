// publisher: HTTP→Nostr signing bridge for the n8n cross-posting
// workflow on Oppy. See README.md for the HTTP API and env-var contract.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/config"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/feed"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/identity"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/notify"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
	"github.com/geofox/publisher/internal/transcode"
	"github.com/geofox/publisher/internal/translate"
	"github.com/geofox/publisher/internal/unfurl"
	"github.com/geofox/publisher/internal/verify"
	"github.com/geofox/publisher/internal/videojob"
)

// Build metadata, overridable at link time:
//
//	go build -ldflags "-X main.version=v1.3.0 -X main.commit=$(git rev-parse --short HEAD)"
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		runHealthcheck()
		return
	}

	// Each driver reads its own variables; every invalid one, from the
	// config and the drivers, is reported at once.
	cfg, cfgErr := config.Load()
	env, envErr := drivers.EnvAccounts(os.Getenv)
	startErrs := []error{cfgErr, envErr}
	// Nostr signs the Blossom uploads, so it stays required until media
	// stores get their own key.
	if strings.TrimSpace(os.Getenv("NSEC_HEX")) == "" {
		startErrs = append(startErrs, errors.New("NSEC_HEX is required"))
	}
	if err := errors.Join(startErrs...); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	setupLogger(cfg.LogLevel)
	metrics.SetBuildInfo(version, commit)

	mediaKey, mediaPub, ok := drivers.EnvMediaSigner(env)
	if !ok {
		fmt.Fprintln(os.Stderr, "config error: NSEC_HEX is required")
		os.Exit(2)
	}
	mp := media.New(cfg.BlossomURL, mediaKey, mediaPub)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	mp.Lookup = st.LookupMediaURL

	uf := unfurl.New()
	// One account per destination type, from the environment (the account
	// registry adds more); a type whose variables aren't set has no account,
	// so posting to it fails as "not configured" before any network call.
	// Each configured type is one env-sourced stored account with a stable
	// id; unconfigured types' accounts are disabled (their history stays).
	var seed []store.EnvAccount
	for _, e := range env {
		drv, _ := destination.Lookup(e.Type)
		seed = append(seed, store.EnvAccount{Type: e.Type, Label: drv.Manifest().Name})
	}
	seeded, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner, seed)
	if err != nil {
		slog.Error("seeding accounts failed", "err", err)
		os.Exit(1)
	}
	accountIDs := dispatch.ConnectedIDs(seeded)
	accounts, err := drivers.OpenEnv(context.Background(), env, accountIDs, destination.Tuning{
		PublishTimeout:  cfg.PublishTimeout,
		POWTimeout:      cfg.POWTimeout,
		RelayCacheTTL:   cfg.RelayCacheTTL,
		VerifyTimeout:   cfg.VerifyHTTPTimeout,
		PLCDirectoryURL: cfg.PLCDirectoryURL,
	})
	if err != nil {
		slog.Error("opening accounts failed", "err", err)
		os.Exit(1)
	}
	// The identity, source resolution and token services still take the
	// clients (until they work by account in part A2). Nostr is always
	// configured here; the others are nil when unset.
	np := accounts[pubnostr.Type].(*pubnostr.Account).P
	var bc *bluesky.Client
	if a, ok := accounts[bluesky.Type].(*bluesky.Account); ok {
		bc = a.C
	}
	var mc *mastodon.Client
	if a, ok := accounts[mastodon.Type].(*mastodon.Account); ok {
		mc = a.C
	}
	var tc *threads.Client
	if a, ok := accounts[threads.Type].(*threads.Account); ok {
		tc = a.C
	}
	d := &dispatch.Dispatcher{
		Accounts:   accounts,
		AccountIDs: accountIDs,
		// Re-encoded variants for URL-transport destinations (Threads) are
		// hosted on Blossom; content addressing makes a retry reuse the URL.
		Host: func(ctx context.Context, body []byte, mime string) (string, error) {
			res, err := mp.Process(ctx, body, mime)
			if err != nil {
				return "", err
			}
			return res.URL, nil
		},
		Store:    st,
		Fetcher:  mp,
		Notify:   feed.NewWebhook(cfg.FeedWebhookURL, cfg.FeedWebhookToken),
		Unfurler: uf,
		Policy: dispatch.RetryPolicy{Enabled: cfg.AutoRetryEnabled, MaxAttempts: cfg.AutoRetryMaxAttempts,
			Base: cfg.AutoRetryBaseDelay, Max: cfg.AutoRetryMaxDelay},
		Workers:       cfg.PublishWorkers,
		ScheduleGrace: cfg.ScheduleGrace,
	}
	a := api.New(np, mp)
	a.Store = st
	a.Dispatch = d
	a.Caps = d.Caps
	a.Locator = d
	a.Unfurl = uf
	a.UserLanguages = cfg.UserLanguages
	a.PublicFeedToken = cfg.PublicFeedToken
	a.Version = version
	// Drafts saved before /v1 get their /v1 shape, once, before requests.
	a.ConvertOldDrafts(context.Background())
	// Video pipeline: enabled only when both ffmpeg and ffprobe are present (dev
	// hosts without them run image-only; the endpoints 503 with a clear message).
	{
		_, ffmpegErr := os.Stat(cfg.FFmpegPath)
		_, ffprobeErr := os.Stat(cfg.FFprobePath)
		switch {
		case ffmpegErr != nil:
			slog.Warn("video pipeline disabled", "missing", cfg.FFmpegPath)
		case ffprobeErr != nil:
			slog.Warn("video pipeline disabled", "missing", cfg.FFprobePath)
		default:
			work := cfg.VideoWorkdir
			if work == "" {
				work = videojob.DefaultWorkdir()
			}
			if err := os.MkdirAll(work, 0o700); err != nil {
				slog.Error("video workdir unavailable — video pipeline disabled", "path", work, "err", err)
			} else {
				vr := videojob.NewRunner(transcode.NewExecTranscoder(cfg.FFmpegPath, cfg.FFprobePath), mp, work)
				vr.SweepWorkdir()
				a.VideoJobs = vr
				a.VideoWorkdir = work
			}
		}
	}
	// Operator's own cross-platform identity for the composer (real handle/name/
	// avatar). Each platform is wired only when its credentials are configured;
	// Nostr is always available (npub derives from the owner pubkey).
	ids := &identity.Service{Nostr: np, TTL: 10 * time.Minute, Timeout: 6 * time.Second}
	if bc != nil {
		ids.Bluesky = bc
	}
	if mc != nil {
		ids.Mastodon = mc
	}
	if tc != nil {
		ids.Threads = tc
	}
	a.Identity = ids
	if cfg.DeepLAPIKey != "" {
		a.Translator = translate.NewDeepL(cfg.DeepLAPIKey)
	}
	if err := st.SeedSyncRelaysIfEmpty(envSyncRelaysSeed(env)); err != nil {
		slog.Error("seed sync relays failed", "err", err)
	}
	a.Sync = relaysync.New(relaysync.NewLiveIO(), np.HomeRelay(), np.OwnerPubkey())
	a.HomeRelay = np.HomeRelay()
	a.Verify = &verify.Service{
		Nostr:    verify.NewNostrVerifier(cfg.VerifyHTTPTimeout),
		Bluesky:  verify.NewBlueskyVerifier(cfg.PLCDirectoryURL, cfg.VerifyHTTPTimeout),
		Mastodon: verify.NewMastodonVerifier(cfg.VerifyHTTPTimeout),
		Threads:  verify.NewThreadsVerifier(cfg.VerifyHTTPTimeout),
	}
	// A platform without an account resolves to "not configured" (a
	// client with no credentials used to fail later, at sign-in).
	rs := &resolve.Service{}
	for typ, field := range map[string]*resolve.Source{
		bluesky.Type: &rs.Bluesky, mastodon.Type: &rs.Mastodon, pubnostr.Type: &rs.Nostr,
	} {
		if r, ok := accounts[typ].(destination.SourceResolver); ok {
			*field = resolve.AccountAdapter{R: r}
		}
	}
	a.Resolve = rs
	// SIGTERM/SIGINT start a graceful stop: readiness fails, new work stops
	// being claimed, and what is in flight finishes.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var draining atomic.Bool
	a.Ready = readinessChecks(cfg, st, a.VideoWorkdir, np.HomeRelay(), env)
	a.Ready.Local = append(a.Ready.Local, health.Check{Name: "accepting_work", Critical: true,
		Run: func(context.Context) error {
			if draining.Load() {
				return health.Fail("shutting down", nil)
			}
			return nil
		}})
	notifier := notify.NewWebhook(cfg.AlertWebhookURL, cfg.AlertWebhookUser, cfg.AlertWebhookPass)
	d.Alerter = notifier
	a.AllowedHosts = cfg.AllowedHosts
	if !cfg.OIDCEnabled() && len(cfg.AllowedHosts) == 0 {
		slog.Warn("OIDC is off and ALLOWED_HOSTS is unset: set ALLOWED_HOSTS to the host name you reach publisher by, so a web page can't reach it through DNS rebinding")
	}
	if cfg.OIDCEnabled() {
		authn, err := auth.New(context.Background(), auth.Config{
			Issuer:       cfg.OIDCIssuer,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  cfg.OIDCRedirectURL,
			Scopes:       cfg.OIDCScopes,
		})
		if err != nil {
			slog.Error("oidc init failed", "err", err)
			os.Exit(1)
		}
		a.Auth = authn
		a.Allowlist = auth.NewAllowlist(cfg.OIDCAllowedSubjects, cfg.OIDCAllowedEmails)
		a.SessionTTL = cfg.SessionTTL
		a.EndSession = cfg.OIDCEndSession
		a.AppBaseURL = appBaseURL(cfg.OIDCRedirectURL)
		// GC expired sessions hourly (same background-goroutine pattern as the
		// scheduler/retrier).
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				if n, err := st.SweepExpiredSessions(); err != nil {
					slog.Warn("session sweep failed", "err", err)
				} else if n > 0 {
					slog.Debug("swept expired sessions", "count", n)
				}
			}
		}()
		slog.Info("oidc auth enabled", "issuer", cfg.OIDCIssuer,
			"allowed_subjects", len(cfg.OIDCAllowedSubjects),
			"allowed_emails", len(cfg.OIDCAllowedEmails))
	}
	// Expired Idempotency-Key records, old events and settled fan-out jobs
	// are dead weight: purge them hourly.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if n, err := st.PurgeIdempotency(ctx, time.Now()); err != nil {
				slog.Warn("idempotency purge failed", "err", err)
			} else if n > 0 {
				slog.Debug("purged idempotency keys", "count", n)
			}
			if n, err := st.PurgeEvents(ctx, time.Now()); err != nil {
				slog.Warn("event purge failed", "err", err)
			} else if n > 0 {
				slog.Debug("purged events", "count", n)
			}
			if n, err := st.PurgeFanout(ctx, time.Now()); err != nil {
				slog.Warn("fan-out purge failed", "err", err)
			} else if n > 0 {
				slog.Debug("purged settled fan-out jobs", "count", n)
			}
		}
	}()
	if e, ok := envAccount(env, threads.Type); ok && tc != nil {
		mgr := threads.NewTokenManager(st, tc, notifier, e.Secrets[threads.SecretAccessToken])
		go mgr.Start(ctx)
	}
	// The engine first recovers what a stopped process left in flight, then
	// claims due items (scheduled posts, retries) in the background.
	// Settle what a stopped process left sending before anything can claim:
	// a request served first could otherwise be recovered mid-send.
	d.Recover(ctx)
	go d.Run(ctx)
	// Relay-level retries and the Nostr fan-out queue re-send already
	// signed events (relays dedupe them), so stopping mid-pass is safe.
	go dispatch.NewRetrier(d, notifier,
		cfg.AutoRetryEnabled, cfg.AutoRetryMaxAttempts,
		cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay, cfg.RetrierTick,
	).Start(ctx)
	go dispatch.NewFanout(d, np.PrimaryFanout(),
		cfg.AutoRetryMaxAttempts, cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay,
		cfg.FanoutTick,
	).Start(ctx)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      a.Routes(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
	}
	// Shutdown waits for open requests; event streams end on this signal.
	stopping := make(chan struct{})
	a.Stopping = stopping
	srv.RegisterOnShutdown(func() { close(stopping) })
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	accountTypes := make([]string, 0, len(env))
	for _, e := range env {
		accountTypes = append(accountTypes, e.Type)
	}
	slog.Info("publisher starting",
		"port", cfg.Port,
		"accounts", accountTypes,
		"pubkey", np.OwnerPubkey().Hex(),
		"blossom", cfg.BlossomURL,
		"bootstrap_relay", np.HomeRelay(),
	)
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
		return
	case <-ctx.Done():
	}
	stop() // a second signal kills the process
	gracefulStop(&draining, srv, d)
}

// shutdownGrace bounds a graceful stop. Docker's default stop timeout is
// 10 s, so the compose example sets stop_grace_period: 30s.
const shutdownGrace = 25 * time.Second

// readinessDrain is how long a stopping process keeps serving, with
// readiness failed, before it closes its listener.
const readinessDrain = 3 * time.Second

// gracefulStop fails readiness, stops accepting requests and lets the ones
// in flight finish, and waits for the items being published. Whatever is
// still sending when the grace runs out is recovered on the next start.
func gracefulStop(draining *atomic.Bool, srv *http.Server, d *dispatch.Dispatcher) {
	slog.Info("shutting down", "grace", shutdownGrace.String())
	draining.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Keep answering for a moment so probes see accepting_work=false
		// before the listener closes.
		select {
		case <-time.After(readinessDrain):
		case <-ctx.Done():
		}
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("http shutdown incomplete", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := d.Shutdown(ctx); err != nil {
			slog.Warn("publishing still in flight at exit; it is recovered on the next start", "err", err)
		}
	}()
	wg.Wait()
	slog.Info("stopped")
}

// readinessChecks builds the /readyz checks. Local checks run on every probe;
// upstream checks only on /readyz?external=1, cached, and never critical. No
// probe sends credentials.
func readinessChecks(cfg config.Config, st *store.Store, videoWorkdir, homeRelay string, env []drivers.EnvAccount) *health.Checker {
	dataDir := filepath.Dir(cfg.DBPath)
	c := &health.Checker{Local: []health.Check{
		health.Database(st, store.LatestSchemaVersion()),
		health.DiskSpace("data_disk", dataDir, health.MinFreeBytes),
	}}
	if videoWorkdir != "" {
		c.Local = append(c.Local, health.WritableDir("video_workdir", videoWorkdir))
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	c.External = append(c.External,
		health.HTTPReachable("blossom", hc, http.MethodHead, cfg.BlossomURL, nil),
		health.NostrRelay("nostr_relay", hc, homeRelay),
	)
	// Until the accounts check themselves (part A2), one probe per
	// configured type, as before.
	for _, e := range env {
		switch e.Type {
		case bluesky.Type:
			pds, _ := destination.ConfigString(e.Config, bluesky.ConfigPDSURL)
			c.External = append(c.External, health.HTTPReachable("bluesky", hc, http.MethodGet, pds+"/xrpc/_health", nil))
		case mastodon.Type:
			base, _ := destination.ConfigString(e.Config, mastodon.ConfigInstanceURL)
			c.External = append(c.External, health.HTTPReachable("mastodon", hc, http.MethodGet, base+"/api/v1/instance", nil))
		case threads.Type:
			c.External = append(c.External, health.HTTPReachable("threads", hc, http.MethodGet, "https://graph.threads.net/", nil))
		}
	}
	return c
}

// envSyncRelaysSeed is the relay-sync list the env Nostr account starts
// with (SYNC_RELAYS).
func envSyncRelaysSeed(env []drivers.EnvAccount) []string {
	e, ok := envAccount(env, pubnostr.Type)
	if !ok {
		return nil
	}
	relays, _ := destination.ConfigStrings(e.Config, pubnostr.ConfigSyncRelaysSeed)
	return relays
}

// envAccount is the environment's account of a type.
func envAccount(env []drivers.EnvAccount, typ string) (drivers.EnvAccount, bool) {
	for _, e := range env {
		if e.Type == typ {
			return e, true
		}
	}
	return drivers.EnvAccount{}, false
}

// runHealthcheck is the container healthcheck: it asks /readyz (local checks
// only) and exits 0 when the instance is ready (200, including "degraded").
func runHealthcheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = config.DefaultPort
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		os.Exit(1)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		os.Exit(1)
	}
}

func setupLogger(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	h := logging.ContextHandler{Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})}
	slog.SetDefault(slog.New(h))
}

// appBaseURL extracts scheme://host from the configured redirect URL.
func appBaseURL(redirect string) string {
	if u, err := url.Parse(redirect); err == nil {
		return u.Scheme + "://" + u.Host
	}
	return ""
}
