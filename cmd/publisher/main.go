// publisher: HTTP→Nostr signing bridge for the n8n cross-posting
// workflow on Oppy. See README.md for the HTTP API and env-var contract.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/api"
	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/config"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/media"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/notify"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/resolve"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/threads"
	"github.com/geofox/publisher/internal/translate"
	"github.com/geofox/publisher/internal/verify"
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
	})
	mp := media.New(cfg.BlossomURL, cfg.NSEC, cfg.OwnerPubkey)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	tc := threads.New(cfg.ThreadsToken, cfg.ThreadsUserID)
	mc := mastodon.New(cfg.MastodonBaseURL, cfg.MastodonToken)
	bc := bluesky.New(cfg.BlueskyPDSURL, cfg.BlueskyIdentifier, cfg.BlueskyAppPassword)
	d := &dispatch.Dispatcher{
		Nostr:    dispatch.NostrAdapter{P: np},
		Mastodon: dispatch.MastodonAdapter{C: mc},
		Bluesky:  dispatch.BlueskyAdapter{C: bc},
		Threads:  dispatch.ThreadsAdapter{C: tc},
		Store:    st,
		Fetcher:  mp,
	}
	a := api.New(np, mp)
	a.Store = st
	a.Dispatch = d
	a.UserLanguages = cfg.UserLanguages
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
	notifier := notify.NewWebhook(cfg.AlertWebhookURL, cfg.AlertWebhookUser, cfg.AlertWebhookPass)
	if cfg.ThreadsToken != "" {
		mgr := threads.NewTokenManager(st, tc, notifier, cfg.ThreadsToken)
		go mgr.Start(context.Background())
	}
	go dispatch.NewScheduler(d, notifier, cfg.ScheduleGrace).Start(context.Background())
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      a.Routes(),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
	}
	slog.Info("publisher starting",
		"port", cfg.Port,
		"pubkey", cfg.OwnerPubkey.Hex(),
		"blossom", cfg.BlossomURL,
		"bootstrap_relay", cfg.NIP65BootstrapRelay,
		"pow_default", cfg.POWDifficultyDefault,
		"pow_max", cfg.POWDifficultyMax,
	)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func runHealthcheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = config.DefaultPort
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
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
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(h))
}
