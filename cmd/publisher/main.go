// publisher: a self-hosted cross-poster (Nostr, Bluesky, Mastodon,
// Threads). See README.md for the HTTP API and the variables it reads.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/auth"
	"github.com/geofox/publisher/internal/config"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/feed"
	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/metrics"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/notify"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/transcode"
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
	if code, ok := runCLI(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	// SIGTERM/SIGINT start a graceful stop: readiness fails, new work stops
	// being claimed, and what is in flight finishes. A second signal kills
	// the process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	os.Exit(run(ctx, runOptions{Getenv: os.Getenv, Unsetenv: os.Unsetenv, Stopping: stop}))
}

// runOptions are run's inputs besides its context.
type runOptions struct {
	// Getenv reads the configuration (os.Getenv; a test's map).
	Getenv func(string) string
	// Unsetenv, if set, removes a credential variable once it is read
	// (os.Unsetenv), so no subprocess inherits it.
	Unsetenv func(string) error
	// Started, if set, is told the address the server listens on.
	Started func(addr string)
	// Stopping, if set, is called when the context ends, before the
	// graceful stop (main restores the default signal handling).
	Stopping func()
}

// run starts the server and serves until ctx ends, then stops gracefully.
// It returns the exit code: 0 after a graceful stop, 2 for a configuration
// error, 1 for any other failure.
func run(ctx context.Context, o runOptions) int {
	getenv := o.Getenv
	// Each driver reads its own variables; every invalid one, from the
	// config and the drivers, is reported at once.
	cfg, cfgErr := config.LoadFrom(getenv)
	env, envErr := drivers.EnvAccounts(getenv)
	keys, keyWarnings, keyErr := secrets.FromEnv(getenv)
	// The settings' variables, read before the credential ones are
	// removed: each one set locks its setting (settings.Defs).
	settingsEnv := map[string]string{}
	for _, n := range settings.EnvNames(settings.Defs()) {
		settingsEnv[n] = getenv(n)
	}
	// What Settings, System shows, read now: the credential variables
	// are removed below.
	system := api.SystemValues(getenv, systemVars)
	startErrs := []error{cfgErr, envErr, keyErr}
	// Every credential is in memory now: take them out of the environment.
	// (The kernel's copy, /proc/<pid>/environ, keeps the values it started
	// with; only the same user or root can read it.)
	if o.Unsetenv != nil {
		for _, n := range slices.Concat(secrets.EnvVars(), config.SecretVars, drivers.SecretEnvVars()) {
			_ = o.Unsetenv(n)
		}
	}
	// Media uploads to a Blossom server are signed with the store's own
	// key once one is made in the app, else with the environment's Nostr
	// key, as before.
	mediaKey, _, haveSigner := drivers.EnvMediaSigner(env)
	if err := errors.Join(startErrs...); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 2
	}
	setupLogger(cfg.LogLevel, os.Stdout)
	metrics.SetBuildInfo(version, commit)
	for _, w := range keyWarnings {
		slog.Warn("master key: " + w)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("store open failed", "err", err)
		return 1
	}
	defer st.Close()

	uf := unfurl.New()
	// Every account the environment configures has a stored row with a
	// stable id; state 2.x kept for "the" account of a type moves to it;
	// then the registry opens what it can (without network I/O).
	var seed []store.EnvAccount
	for _, e := range env {
		drv, _ := destination.Lookup(e.Type)
		seed = append(seed, store.EnvAccount{Type: e.Type, Key: e.Key, Label: drv.Manifest().Name})
	}
	envRows, err := st.SeedEnvAccounts(context.Background(), store.DefaultOwner, seed)
	if err != nil {
		slog.Error("seeding accounts failed", "err", err)
		return 1
	}
	if err := drivers.AdoptLegacy(context.Background(), st, envRows, env); err != nil {
		slog.Error("adopting 2.x account state failed; it is tried again at the next start", "err", err)
	}
	warnAdoptedStillSet(context.Background(), st, env, cfg.BlossomURL != "")
	// The master key: values sealed with the previous key move to the
	// current one, then the key is checked against what is stored. Saved
	// account state goes in the secret store once a usable key is set.
	keyState, state, err := prepareSecrets(context.Background(), st, keys, envRows, env, cfg.DBPath)
	if err != nil {
		slog.Error("reading the stored credentials failed", "err", err)
		return 1
	}
	regEnv := make([]accounts.EnvAccount, 0, len(env))
	for _, e := range env {
		regEnv = append(regEnv, accounts.EnvAccount{Type: e.Type, EnvAccount: e.EnvAccount})
	}
	// Owner-set addresses (accounts added in the app, relay sync targets)
	// reach public addresses and those PRIVATE_DESTINATIONS lists, never
	// the server's own.
	guard := cfg.PrivateDestinations.WithOwn()
	// The owner's settings (env, then saved in the app, then defaults).
	svc, err := settings.New(context.Background(), settings.Options{
		Store: st, Keys: keys, CanSeal: keyState.WriteError, Env: settingsEnv,
		URL: func(raw string, rule destination.URLRule) (string, error) {
			return guard.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path, Query: rule.Query})
		},
	})
	if err != nil {
		slog.Error("loading the settings failed", "err", err)
		return 1
	}
	// The media store follows the settings (spec §7.6). Without one posts
	// carry no media; a retry still fetches the media of posts made while
	// one was set.
	disk := &media.Local{Dir: cfg.MediaDir, BaseURL: cfg.PublicURL,
		Quota:    func() int64 { return int64(svc.Current().Int(settings.MediaLocalQuota)) << 20 },
		IsPublic: func() bool { return svc.Current().Bool(settings.MediaPublic) }}
	if n, err := disk.RemoveLeftovers(); err != nil {
		slog.Warn("media: removing unfinished uploads failed", "err", err)
	} else if n > 0 {
		slog.Info("media: removed unfinished uploads", "count", n)
	}
	guardedStream := guardedStreamClient(guard)
	mp := media.New(liveMediaStore(svc, guardedStream, mediaKey, haveSigner, disk))
	mp.Lookup = reuseStored(st.LookupMediaURL, disk, func() bool { return svc.Current().String(settings.MediaStore) == settings.StoreLocal })
	// Webhooks set in the app reach what owner-set addresses may; those
	// from the environment are the operator's, reached as before.
	trustedHTTP := &http.Client{Timeout: 10 * time.Second}
	guardedHTTP := destination.GuardedNet(guard.Allow).Client(10 * time.Second)
	webhookHTTP := func(s *settings.Snapshot, key string) *http.Client {
		if v, _ := s.Get(key); v.Source == settings.SourceSaved {
			return guardedHTTP
		}
		return trustedHTTP
	}
	alertWebhook := func() *notify.Webhook {
		s := svc.Current()
		u := s.Secret(settings.AlertsWebhookURL)
		if u == "" {
			return nil
		}
		return &notify.Webhook{URL: u, User: s.String(settings.AlertsWebhookUser), Pass: pairedSecret(s, settings.AlertsWebhookPass, settings.AlertsWebhookURL),
			HTTP: webhookHTTP(s, settings.AlertsWebhookURL)}
	}
	feedWebhook := func() *feed.Webhook {
		s := svc.Current()
		return &feed.Webhook{URL: s.Secret(settings.FeedWebhookURL), Token: pairedSecret(s, settings.FeedWebhookToken, settings.FeedWebhookURL),
			HTTP: webhookHTTP(s, settings.FeedWebhookURL)}
	}
	notifier := &notify.Live{Webhook: alertWebhook}
	reg := accounts.New(accounts.Options{
		Store:    st,
		Env:      regEnv,
		State:    state,
		Secrets:  st,
		Keys:     keys,
		KeyState: keyState,
		Tuning: destination.Tuning{
			PublishTimeout:  cfg.PublishTimeout,
			POWTimeout:      cfg.POWTimeout,
			RelayCacheTTL:   cfg.RelayCacheTTL,
			VerifyTimeout:   cfg.VerifyHTTPTimeout,
			PLCDirectoryURL: cfg.PLCDirectoryURL,
		},
		Alerter: notifier,
		Guard:   guard,
		Writes:  st,
		Seed:    drivers.NewAccountSeeder(st, getenv),
		Getenv:  getenv,
	})
	if err := reg.Load(context.Background()); err != nil {
		slog.Error("loading accounts failed", "err", err)
		return 1
	}
	defer reg.Close()
	// Profiles are fetched in the background now, so the composer has
	// them when it first asks.
	reg.RefreshProfiles()
	d := &dispatch.Dispatcher{
		Registry: reg,
		Store:    st,
		// A plain fetcher, never a nil *Pipeline inside the interface.
		// Media on this server's disk are read from it, never over HTTP;
		// those on the environment's Blossom server are reached as
		// before; any other address (a server set in the app, or one it
		// answered) as owner-set addresses are.
		Fetcher: media.LocalFirst{Local: disk, Next: &media.Fetcher{HTTP: guardedStream,
			TrustedOrigin: media.Origin(cfg.BlossomURL), TrustedHTTP: media.NewStreamClient()}},
		Notify:   feed.Live{Webhook: feedWebhook},
		Unfurler: uf,
		Alerter:  notifier,
		Policy: dispatch.RetryPolicy{Enabled: cfg.AutoRetryEnabled, MaxAttempts: cfg.AutoRetryMaxAttempts,
			Base: cfg.AutoRetryBaseDelay, Max: cfg.AutoRetryMaxDelay},
		Workers:       cfg.PublishWorkers,
		ScheduleGrace: cfg.ScheduleGrace,
		Live: func() dispatch.Live {
			s := svc.Current()
			return dispatch.Live{AutoRetry: s.Bool(settings.PublishingAutoRetry), MaxAttempts: s.Int(settings.PublishingMaxAttempts),
				ScheduleGrace: s.Duration(settings.PublishingGrace)}
		},
	}
	if mp != nil {
		// Re-encoded variants for URL-transport destinations (Threads) are
		// hosted in the media store; content addressing makes a retry
		// reuse the URL. Without a store they use the canonical URL.
		d.Host = func(ctx context.Context, body []byte, mime string) (string, error) {
			res, err := mp.Process(ctx, body, mime)
			if err != nil {
				return "", err
			}
			return res.URL, nil
		}
	}
	a := api.New(mp)
	a.Store = st
	a.Dispatch = d
	a.Caps = d.Caps
	a.Accounts = reg
	a.Locator = d
	a.Unfurl = uf
	a.Settings = svc
	// Settings' test buttons send through the same webhooks and clients
	// as the real calls (media's test is the store's own).
	a.TestSettings = func(ctx context.Context, group string) (int, bool, error) {
		switch group {
		case "alerts":
			if w := alertWebhook(); w != nil {
				status, err := w.Test(ctx)
				return status, true, err
			}
		case "feed":
			if w := feedWebhook(); w.URL != "" {
				status, err := w.Test(ctx)
				return status, true, err
			}
		case "media":
			return testMediaStore(ctx, mp)
		}
		return 0, false, nil
	}
	a.DiskMedia = disk
	a.System = func() []api.SystemValue { return system }
	a.Version = version
	// Drafts saved before /v1 get their /v1 shape, once, before requests.
	a.ConvertOldDrafts(context.Background())
	// Video pipeline: enabled with both ffmpeg and ffprobe present (dev
	// hosts without them run image-only; the endpoints 503 with a clear
	// message); it stores into the media store set at the time.
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
	// Relay sync works on the Nostr account a request names (its own
	// home relay, key and list).
	a.Sync = relaysync.New(relaysync.NewLiveIO())
	a.Guard = guard
	a.MasterKey = keyState
	a.IdemMAC = keys.MAC
	a.Verify = &verify.Service{
		Nostr:    verify.NewNostrVerifier(cfg.VerifyHTTPTimeout),
		Bluesky:  verify.NewBlueskyVerifier(cfg.PLCDirectoryURL, cfg.VerifyHTTPTimeout),
		Mastodon: verify.NewMastodonVerifier(cfg.VerifyHTTPTimeout),
		Threads:  verify.NewThreadsVerifier(cfg.VerifyHTTPTimeout),
	}
	var draining atomic.Bool
	a.Ready = readinessChecks(cfg, st, a.VideoWorkdir, reg, mp)
	a.Ready.Local = append(a.Ready.Local, health.Check{Name: "accepting_work", Critical: true,
		Run: func(context.Context) error {
			if draining.Load() {
				return health.Fail("shutting down", nil)
			}
			return nil
		}})
	a.AllowedHosts = cfg.AllowedHosts
	if code := setupSignIn(ctx, cfg, st, a); code != 0 {
		return code
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
	// Credentials are renewed before they expire, and accounts are checked
	// daily (and after a refused send).
	go reg.RunRefresh(ctx)
	go reg.RunChecks(ctx)
	// The engine first recovers what a stopped process left in flight, then
	// claims due items (scheduled posts, retries) in the background.
	// Settle what a stopped process left sending before anything can claim:
	// a request served first could otherwise be recovered mid-send.
	d.Recover(ctx)
	go d.Run(ctx)
	// Relay-level retries and the Nostr fan-out queue re-send already
	// signed events (relays dedupe them), so stopping mid-pass is safe.
	retrier := dispatch.NewRetrier(d, notifier,
		cfg.AutoRetryEnabled, cfg.AutoRetryMaxAttempts,
		cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay, cfg.RetrierTick,
	)
	retrier.Live = func() (bool, int) {
		s := svc.Current()
		return s.Bool(settings.PublishingAutoRetry), s.Int(settings.PublishingMaxAttempts)
	}
	go retrier.Start(ctx)
	fanout := dispatch.NewFanout(d,
		cfg.AutoRetryMaxAttempts, cfg.AutoRetryBaseDelay, cfg.AutoRetryMaxDelay,
		cfg.FanoutTick,
	)
	fanout.MaxAttempts = func() int { return svc.Current().Int(settings.PublishingMaxAttempts) }
	go fanout.Start(ctx)
	srv := &http.Server{
		Handler:      a.Routes(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
	}
	// Shutdown waits for open requests; event streams end on this signal.
	stopping := make(chan struct{})
	a.Stopping = stopping
	srv.RegisterOnShutdown(func() { close(stopping) })
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		slog.Error("server failed", "err", err)
		return 1
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logStart(cfg, reg, mp)
	if o.Started != nil {
		o.Started(ln.Addr().String())
	}
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			return 1
		}
		return 0
	case <-ctx.Done():
	}
	if o.Stopping != nil {
		o.Stopping()
	}
	gracefulStop(&draining, srv, d)
	return 0
}

// logStart logs what the server runs with: each account by type and
// status, and the media store.
func logStart(cfg config.Config, reg *accounts.Registry, mp *media.Pipeline) {
	byStatus := map[string][]string{}
	for _, e := range reg.List(store.DefaultOwner) {
		byStatus[string(e.Status)] = append(byStatus[string(e.Status)], e.Row.Type+":"+e.Row.ID)
	}
	args := []any{"port", cfg.Port, "accounts", byStatus}
	switch err := mp.Check(); {
	case errors.Is(err, media.ErrNoStore):
		args = append(args, "media", "none (set a store in Settings, Media, or BLOSSOM_URL: posts carry no media)")
	case err != nil:
		args = append(args, "media", err.Error())
	default:
		args = append(args, "media", mp.Describe())
	}
	slog.Info("publisher starting", args...)
	if len(byStatus) == 0 {
		slog.Warn("no account is configured: set a network's variables to post (see the README)")
	}
	for _, e := range reg.List(store.DefaultOwner) {
		switch e.Status {
		case accounts.Connected:
		case accounts.Disabled: // the owner's choice
			slog.Info("account turned off", "account", e.Row.ID, "type", e.Row.Type)
		default:
			slog.Warn("account not connected", "account", e.Row.ID, "type", e.Row.Type, "status", e.Status, "reason", e.Reason)
		}
	}
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
func readinessChecks(cfg config.Config, st *store.Store, videoWorkdir string, reg *accounts.Registry, mp *media.Pipeline) *health.Checker {
	dataDir := filepath.Dir(cfg.DBPath)
	c := &health.Checker{Local: []health.Check{
		health.Database(st, store.LatestSchemaVersion()),
		health.DiskSpace("data_disk", dataDir, health.MinFreeBytes),
	}}
	if videoWorkdir != "" {
		c.Local = append(c.Local, health.WritableDir("video_workdir", videoWorkdir))
	}
	mediaDisk, mediaChecks := mediaReadiness(cfg.MediaDir, mp)
	c.Local = append(c.Local, mediaDisk)
	// The master key: not critical (accounts from the environment work),
	// but accounts added in the app are locked until it is fixed.
	c.Local = append(c.Local, health.Check{Name: "secrets", Run: func(context.Context) error {
		switch reg.KeyState().Status {
		case accounts.KeyMissing:
			return health.Fail("master key missing", nil)
		case accounts.KeyMismatch:
			return health.Fail("master key mismatch", nil)
		}
		return nil
	}})
	// One check per open account (account:<id>), asked again at each
	// refresh so an account turned off or reopened is followed.
	c.MoreExternal = func() []health.Check { return append(mediaChecks(), reg.Readiness(store.DefaultOwner)...) }
	return c
}

// mediaReadiness follows the media store the settings set: this server's
// disk is checked for room (MEDIA_DIR may be another volume), a Blossom
// server for reachability, through the client its uploads use.
func mediaReadiness(dir string, mp *media.Pipeline) (health.Check, func() []health.Check) {
	room := health.DiskSpace("media_disk", dir, health.MinFreeBytes)
	disk := health.Check{Name: "media_disk", Run: func(ctx context.Context) error {
		if s, err := mp.Store(); err != nil {
			return nil
		} else if _, local := s.(*media.Local); !local {
			return nil
		}
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil // made at the first upload
		}
		return room.Run(ctx)
	}}
	external := func() []health.Check {
		s, err := mp.Store()
		b, ok := s.(*media.Blossom)
		if err != nil || !ok {
			return nil
		}
		hc := &http.Client{Transport: b.HTTP.Transport, CheckRedirect: b.HTTP.CheckRedirect, Timeout: 5 * time.Second}
		return []health.Check{health.HTTPReachable("blossom", hc, http.MethodHead, b.URL, nil)}
	}
	return disk, external
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

func setupLogger(level string, w io.Writer) {
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
	// Every string and error the log prints is scrubbed of credentials
	// (an upstream error that echoes a token, a URL with access_token=).
	h := logging.ContextHandler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l, ReplaceAttr: logging.ScrubAttr})}
	slog.SetDefault(slog.New(h))
}

// setupSignIn picks how the owner signs in (AUTH_MODE, else OIDC when
// configured, else local for a database this version created and proxy
// for an older one) and wires it into the API. It returns an exit code,
// 0 to go on.
func setupSignIn(ctx context.Context, cfg config.Config, st *store.Store, a *api.API) int {
	born, err := st.CreatedSchema(ctx)
	if err != nil {
		slog.Error("read the database's creation version failed", "err", err)
		return 1
	}
	mode := cfg.ResolveAuthMode(born)
	if mode == config.AuthLocal {
		if err := cfg.CheckLocal(); err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			return 2
		}
	}
	a.Mode = mode
	a.Clients = &httpx.ClientAddrs{Trusted: cfg.TrustedProxies}
	a.PublicURL = cfg.PublicURL
	a.SessionTTL = cfg.SessionTTL
	a.AppBaseURL = cfg.PublicURL
	switch mode {
	case config.AuthOIDC:
		authn, err := auth.New(ctx, auth.Config{
			Issuer:       cfg.OIDCIssuer,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  cfg.OIDCRedirectURL,
			Scopes:       cfg.OIDCScopes,
		})
		if err != nil {
			slog.Error("oidc init failed", "err", err)
			return 1
		}
		a.Auth = authn
		a.Allowlist = auth.NewAllowlist(cfg.OIDCAllowedSubjects, cfg.OIDCAllowedEmails)
		a.EndSession = cfg.OIDCEndSession
		if a.AppBaseURL == "" {
			a.AppBaseURL = appBaseURL(cfg.OIDCRedirectURL)
		}
		slog.Info("sign-in: OIDC", "issuer", cfg.OIDCIssuer,
			"allowed_subjects", len(cfg.OIDCAllowedSubjects),
			"allowed_emails", len(cfg.OIDCAllowedEmails))
	case config.AuthLocal:
		svc, err := localauth.New(st, cfg.PublicURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			return 2
		}
		a.Local = svc
		a.SetupTokenFile = cfg.SetupTokenFile
		if cfg.PublicURL == "" {
			slog.Warn("sign-in: PUBLIC_URL is unset, so nobody can sign in: set it to the https address you use for Publisher (passkeys and secure cookies need it)")
		}
		if err := localauth.AnnounceSetup(ctx, st, cfg.PublicURL, cfg.SetupTokenFile); err != nil {
			slog.Error("setup: creating the setup link failed", "err", err)
			return 1
		}
		slog.Info("sign-in: local (passkeys and password)", "public_url", cfg.PublicURL,
			"trusted_proxies", len(cfg.TrustedProxies))
	case config.AuthProxy:
		if len(cfg.AllowedHosts) == 0 {
			slog.Warn("ALLOWED_HOSTS is unset: set ALLOWED_HOSTS to the host name you reach publisher by, so a web page can't reach it through DNS rebinding")
		}
		slog.Warn("sign-in: none of its own (AUTH_MODE=proxy): the reverse proxy in front is the only gate, and credentials can't be managed in the app; set AUTH_MODE=local and PUBLIC_URL to sign in with a passkey")
	}
	if mode == config.AuthProxy {
		return 0
	}
	// Hourly: expired sessions, and failed sign-ins older than a day.
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
			if mode == config.AuthLocal {
				if err := st.PruneLoginFailures(ctx, time.Now()); err != nil {
					slog.Warn("pruning failed sign-ins failed", "err", err)
				}
			}
		}
	}()
	return 0
}

// appBaseURL extracts scheme://host from the configured redirect URL.
func appBaseURL(redirect string) string {
	if u, err := url.Parse(redirect); err == nil {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

// pairedSecret is key's secret for the address at urlKey: one from the
// environment goes only to an address from the environment, so an address
// set in the app never receives the operator's password or token (the
// settings refuse that pairing; this covers an address saved before the
// variable was set).
func pairedSecret(s *settings.Snapshot, key, urlKey string) string {
	v, _ := s.Get(key)
	u, _ := s.Get(urlKey)
	if v.Source == settings.SourceEnv && u.Source != settings.SourceEnv {
		return ""
	}
	return s.Secret(key)
}

// guardedStreamClient carries large bodies to and from owner-set
// addresses: the owner-set guard on every connection and redirect, no
// overall timeout, a bounded wait for the answer.
func guardedStreamClient(guard netguard.Policy) *http.Client {
	return &http.Client{Transport: destination.GuardedNet(guard.Allow).Transport(destination.TransportOptions{
		ResponseHeaderTimeout: 60 * time.Second}), CheckRedirect: destination.GuardedRedirect}
}

// liveMediaStore is the media store the settings set now: none, or a
// Blossom server, signed with the store's own key once one is set (and
// then named in each authorization), else with the environment's Nostr
// key. A server set in the app is reached with guarded, as owner-set
// addresses are, and the addresses it answers stay on its own origin.
func liveMediaStore(svc *settings.Service, guarded *http.Client, envKey gonostr.SecretKey, haveEnvKey bool, disk *media.Local) func() (media.Store, error) {
	trusted := media.NewStreamClient()
	return func() (media.Store, error) {
		s := svc.Current()
		switch s.String(settings.MediaStore) {
		case settings.StoreBlossom:
			u := strings.TrimRight(s.String(settings.MediaBlossomURL), "/")
			if u == "" {
				return nil, &media.UnusableError{Reason: "no Blossom server is set (Settings, Media)"}
			}
			cl, saved := trusted, false
			if v, _ := s.Get(settings.MediaBlossomURL); v.Source != settings.SourceEnv {
				cl, saved = guarded, true
			}
			if k := s.Secret(settings.MediaBlossomKey); k != "" {
				sk, err := drivers.MediaSigningKey(k)
				if err != nil {
					return nil, &media.UnusableError{Reason: "the Blossom signing key isn't a Nostr key: make a new one in Settings, Media"}
				}
				return &media.Blossom{URL: u, Key: sk, Own: true, Confined: true, HTTP: cl}, nil
			}
			if haveEnvKey {
				return &media.Blossom{URL: u, Key: envKey, Confined: saved, HTTP: cl}, nil
			}
			return nil, &media.UnusableError{Reason: "uploads to a Blossom server are signed: make a signing key in Settings, Media (or set NSEC_HEX)"}
		case settings.StoreLocal:
			return disk, nil
		}
		return nil, media.ErrNoStore
	}
}

// systemVars are the bootstrap and tuning variables Settings, System
// shows (the settings' own, and the accounts', show in their sections).
var systemVars = func() []api.SystemVar {
	var out []api.SystemVar
	for _, e := range []string{"PUBLIC_URL", "AUTH_MODE", "ALLOWED_HOSTS", "TRUSTED_PROXIES", "PORT", "LOG_LEVEL",
		"DB_PATH", "MEDIA_DIR", "VIDEO_WORKDIR", "FFMPEG_PATH", "FFPROBE_PATH", "PLC_DIRECTORY_URL", "PRIVATE_DESTINATIONS",
		"SESSION_TTL", "SETUP_TOKEN_FILE", "PUBLISHER_MASTER_KEY_FILE", "PUBLISHER_MASTER_KEY_PREVIOUS_FILE",
		"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_REDIRECT_URL", "OIDC_SCOPES", "OIDC_END_SESSION",
		"OIDC_ALLOWED_SUBJECTS", "OIDC_ALLOWED_EMAILS",
		"PUBLISH_WORKERS", "PUBLISH_TIMEOUT", "FANOUT_TICK", "RETRIER_TICK", "AUTO_RETRY_BASE_DELAY",
		"AUTO_RETRY_MAX_DELAY", "POW_TIMEOUT", "RELAY_CACHE_TTL", "VERIFY_HTTP_TIMEOUT"} {
		out = append(out, api.SystemVar{Env: e})
	}
	for _, e := range []string{"OIDC_CLIENT_SECRET", "PUBLISHER_MASTER_KEY", "PUBLISHER_MASTER_KEY_PREVIOUS"} {
		out = append(out, api.SystemVar{Env: e, Secret: true})
	}
	return out
}()

// reuseStored is the media pipeline's Lookup: an upload of bytes stored
// before keeps their address, unless that is on this server's disk and
// the store is another now (the owner moved to one others can reach), its
// file is gone, or it was given before PUBLIC_URL was set.
func reuseStored(lookup func(string) (string, bool, error), disk *media.Local, local func() bool) func(string) (string, bool, error) {
	return func(sha string) (string, bool, error) {
		u, ok, err := lookup(sha)
		if err != nil || !ok {
			return u, ok, err
		}
		if name, mine := disk.Owns(u); mine && (!local() || !disk.Has(name) || (disk.BaseURL != "" && strings.HasPrefix(u, "/"))) {
			return "", false, nil
		}
		return u, true, nil
	}
}

// testMediaStore is Settings' media test: a Blossom server asked whether
// it would take an upload signed with the key (BUD-06; one without it
// answering 404 or 405 is reachable, which is all it says); this
// server's disk written and read back, and fetched at PUBLIC_URL/media/
// when served publicly.
func testMediaStore(ctx context.Context, mp *media.Pipeline) (int, bool, error) {
	s, err := mp.Store()
	switch {
	case errors.Is(err, media.ErrNoStore):
		return 0, false, nil
	case err != nil:
		return 0, true, err
	}
	switch s := s.(type) {
	case *media.Blossom:
		status, err := s.Check(ctx)
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			status = http.StatusNoContent
		}
		return status, true, err
	case *media.Local:
		if !s.Public() {
			if err := s.CheckWritable(ctx); err != nil {
				return 0, true, &media.ProbeError{Store: err}
			}
			return http.StatusNoContent, true, nil
		}
		// What PUBLIC_URL/media/ answered: a status, a redirect, or other
		// bytes (a sign-in page or the web app answer 200 too); or the
		// disk's refusal.
		var pe *media.ProbeError
		if err := s.CheckPublic(ctx, media.NoRedirects()); errors.As(err, &pe) && pe.Store == nil && !pe.Mismatch {
			return pe.Status, true, nil
		} else if err != nil {
			return 0, true, err
		}
		return http.StatusOK, true, nil
	}
	return 0, true, errors.New("this store has no test")
}
