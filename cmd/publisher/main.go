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
	"github.com/geofox/publisher/internal/progress"
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

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	setupLogger(cfg.LogLevel)
	metrics.SetBuildInfo(version, commit)

	np := pubnostr.New(pubnostr.Config{
		NSEC:                 cfg.NSEC,
		OwnerPubkey:          cfg.OwnerPubkey,
		NIP65BootstrapRelay:  cfg.NIP65BootstrapRelay,
		FallbackRelays:       cfg.FallbackRelays,
		POWDifficultyDefault: cfg.POWDifficultyDefault,
		POWDifficultyMax:     cfg.POWDifficultyMax,
		POWTimeout:           cfg.POWTimeout,
		RelayCacheTTL:        cfg.RelayCacheTTL,
		PublishTimeout:       cfg.PublishTimeout,
		PrimaryFanout:        cfg.NostrPrimaryFanout,
		PrimaryRelays:        cfg.PrimaryRelays,
	})
	mp := media.New(cfg.BlossomURL, cfg.NSEC, cfg.OwnerPubkey)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	mp.Lookup = st.LookupMediaURL

	tc := threads.New(cfg.ThreadsToken, cfg.ThreadsUserID)
	mc := mastodon.New(cfg.MastodonBaseURL, cfg.MastodonToken)
	bc := bluesky.New(cfg.BlueskyPDSURL, cfg.BlueskyIdentifier, cfg.BlueskyAppPassword)
	uf := unfurl.New()
	drivers.Register()
	// One account per destination type, from env (step 4 adds more); a type
	// whose credentials aren't configured has no account, so posting to it
	// fails as "not configured" before any network call.
	accounts := map[string]destination.Account{pubnostr.Type: &pubnostr.Account{P: np}}
	if cfg.BlueskyAppPassword != "" {
		accounts[bluesky.Type] = &bluesky.Account{C: bc}
	}
	if cfg.MastodonToken != "" {
		accounts[mastodon.Type] = mastodon.NewAccount(mc)
	}
	if cfg.ThreadsToken != "" {
		accounts[threads.Type] = &threads.Account{C: tc}
	}
	// Each configured type is one env-sourced stored account with a stable
	// id; unconfigured types' accounts are disabled (their history stays).
	var seed []store.EnvAccount
	for typ, acct := range accounts {
		seed = append(seed, store.EnvAccount{Type: typ, Label: acct.Manifest().Name})
	}
	if _, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner, seed); err != nil {
		slog.Error("seeding accounts failed", "err", err)
		os.Exit(1)
	}
	d := &dispatch.Dispatcher{
		Accounts: accounts,
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
	a.Unfurl = uf
	a.UserLanguages = cfg.UserLanguages
	a.PublicFeedToken = cfg.PublicFeedToken
	a.Progress = progress.NewRegistry()
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
	if cfg.BlueskyAppPassword != "" {
		ids.Bluesky = bc
	}
	if cfg.MastodonToken != "" {
		ids.Mastodon = mc
	}
	if cfg.ThreadsToken != "" {
		ids.Threads = tc
	}
	a.Identity = ids
	if cfg.DeepLAPIKey != "" {
		a.Translator = translate.NewDeepL(cfg.DeepLAPIKey)
	}
	if err := st.SeedSyncRelaysIfEmpty(cfg.SyncRelaysDefault); err != nil {
		slog.Error("seed sync relays failed", "err", err)
	}
	a.Sync = relaysync.New(relaysync.NewLiveIO(), cfg.NIP65BootstrapRelay, cfg.OwnerPubkey)
	a.HomeRelay = cfg.NIP65BootstrapRelay
	a.Verify = &verify.Service{
		Nostr:    verify.NewNostrVerifier(cfg.VerifyHTTPTimeout),
		Bluesky:  verify.NewBlueskyVerifier(cfg.PLCDirectoryURL, cfg.VerifyHTTPTimeout),
		Mastodon: verify.NewMastodonVerifier(cfg.VerifyHTTPTimeout),
		Threads:  verify.NewThreadsVerifier(cfg.VerifyHTTPTimeout),
	}
	a.Resolve = &resolve.Service{
		Bluesky:  resolve.BlueskyAdapter{C: bc},
		Mastodon: resolve.MastodonAdapter{C: mc},
		Nostr:    resolve.NostrAdapter{P: np},
	}
	// SIGTERM/SIGINT start a graceful stop: readiness fails, new work stops
	// being claimed, and what is in flight finishes.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var draining atomic.Bool
	a.Ready = readinessChecks(cfg, st, a.VideoWorkdir)
	a.Ready.Local = append(a.Ready.Local, health.Check{Name: "accepting_work", Critical: true,
		Run: func(context.Context) error {
			if draining.Load() {
				return health.Fail("shutting down", nil)
			}
			return nil
		}})
	notifier := notify.NewWebhook(cfg.AlertWebhookURL, cfg.AlertWebhookUser, cfg.AlertWebhookPass)
	d.Alerter = notifier
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
	if cfg.ThreadsToken != "" {
		mgr := threads.NewTokenManager(st, tc, notifier, cfg.ThreadsToken)
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
	go dispatch.NewFanout(d, cfg.NostrPrimaryFanout,
		cfg.AutoRetryMaxAttempts, cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay,
		cfg.FanoutTick,
	).Start(ctx)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      a.Routes(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	slog.Info("publisher starting",
		"port", cfg.Port,
		"pubkey", cfg.OwnerPubkey.Hex(),
		"blossom", cfg.BlossomURL,
		"bootstrap_relay", cfg.NIP65BootstrapRelay,
		"pow_default", cfg.POWDifficultyDefault,
		"pow_max", cfg.POWDifficultyMax,
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
func readinessChecks(cfg config.Config, st *store.Store, videoWorkdir string) *health.Checker {
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
		health.NostrRelay("nostr_relay", hc, cfg.NIP65BootstrapRelay),
	)
	if cfg.BlueskyAppPassword != "" && cfg.BlueskyPDSURL != "" {
		c.External = append(c.External, health.HTTPReachable("bluesky", hc, http.MethodGet, cfg.BlueskyPDSURL+"/xrpc/_health", nil))
	}
	if cfg.MastodonToken != "" && cfg.MastodonBaseURL != "" {
		c.External = append(c.External, health.HTTPReachable("mastodon", hc, http.MethodGet, cfg.MastodonBaseURL+"/api/v1/instance", nil))
	}
	if cfg.ThreadsToken != "" {
		c.External = append(c.External, health.HTTPReachable("threads", hc, http.MethodGet, "https://graph.threads.net/", nil))
	}
	return c
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
