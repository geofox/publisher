package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

const DefaultPort = "8080"

type Config struct {
	NSEC                 nostr.SecretKey
	OwnerPubkey          nostr.PubKey
	BlossomURL           string
	NIP65BootstrapRelay  string
	FallbackRelays       []string
	POWDifficultyDefault int
	POWDifficultyMax     int
	POWTimeout           time.Duration
	RelayCacheTTL        time.Duration
	PublishTimeout       time.Duration
	ScheduleGrace        time.Duration
	Port                 string
	LogLevel             string

	// New in the publisher graduation:
	DBPath          string
	MastodonBaseURL string
	MastodonToken   string

	BlueskyPDSURL      string
	BlueskyIdentifier  string
	BlueskyAppPassword string

	ThreadsToken  string
	ThreadsUserID string

	AlertWebhookURL  string
	AlertWebhookUser string
	AlertWebhookPass string

	SyncRelaysDefault []string

	PLCDirectoryURL   string
	VerifyHTTPTimeout time.Duration

	// UserLanguages is the operator's spoken-languages list (ISO 639-1 codes,
	// e.g. ["en", "fr"]). The frontend uses the first as the default for the
	// Bluesky and Mastodon language fields and offers a dropdown when there's
	// more than one. Unset → empty (frontend falls back to "en").
	UserLanguages []string

	// DeepLAPIKey enables /api/translate (translate-a-history-post → Compose).
	// Free vs Pro is auto-detected from the key suffix (":fx" → Free). Unset
	// → translation disabled (the UI hides the button).
	DeepLAPIKey string

	// PublicFeedToken gates GET /api/public/feed (empty → endpoint disabled).
	PublicFeedToken string
	// FeedWebhookURL: signal-only ping POSTed when a feed-eligible post is
	// published (empty → no webhook). FeedWebhookToken is sent as a bearer token.
	FeedWebhookURL   string
	FeedWebhookToken string
}

func Load() (Config, error) {
	c := Config{
		BlossomURL:          strings.TrimRight(getEnv("BLOSSOM_URL", ""), "/"),
		NIP65BootstrapRelay: getEnv("NIP65_BOOTSTRAP_RELAY", "wss://relay.geoffrey.one"),
		Port:                getEnv("PORT", DefaultPort),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		DBPath:              getEnv("DB_PATH", "/data/publisher.db"),
		MastodonBaseURL:     strings.TrimRight(getEnv("MASTODON_BASE_URL", ""), "/"),
		MastodonToken:       getEnv("MASTODON_TOKEN", ""),
		BlueskyPDSURL:       strings.TrimRight(getEnv("BLUESKY_PDS_URL", ""), "/"),
		BlueskyIdentifier:   getEnv("BLUESKY_IDENTIFIER", ""),
		BlueskyAppPassword:  getEnv("BLUESKY_APP_PASSWORD", ""),
		ThreadsToken:        getEnv("THREADS_ACCESS_TOKEN", ""),
		ThreadsUserID:       getEnv("THREADS_USER_ID", "me"),
		AlertWebhookURL:     getEnv("ALERT_WEBHOOK_URL", ""),
		AlertWebhookUser:    getEnv("ALERT_WEBHOOK_USER", "alertmanager"),
		AlertWebhookPass:    getEnv("ALERT_WEBHOOK_PASS", ""),
		PLCDirectoryURL:     getEnv("PLC_DIRECTORY_URL", "https://plc.directory"),
	}
	c.FallbackRelays = splitCSV(getEnv("FALLBACK_RELAYS",
		"wss://relay.geoffrey.one,wss://nos.lol,wss://relay.damus.io"))
	c.SyncRelaysDefault = splitCSV(getEnv("SYNC_RELAYS",
		"wss://nos.lol,wss://relay.damus.io,wss://nostr.wine,wss://nostr.land,wss://relay.nostr.band,wss://purplepag.es,wss://relay.snort.social,wss://nostr.mom"))
	c.UserLanguages = splitCSV(getEnv("USER_LANGUAGES", ""))
	c.DeepLAPIKey = getEnv("DEEPL_API_KEY", "")
	c.PublicFeedToken = getEnv("PUBLIC_FEED_TOKEN", "")
	c.FeedWebhookURL = getEnv("FEED_WEBHOOK_URL", "")
	c.FeedWebhookToken = getEnv("FEED_WEBHOOK_TOKEN", "")

	var err error
	if c.POWDifficultyDefault, err = strconv.Atoi(getEnv("POW_DIFFICULTY_DEFAULT", "16")); err != nil {
		return c, fmt.Errorf("POW_DIFFICULTY_DEFAULT: %w", err)
	}
	if c.POWDifficultyMax, err = strconv.Atoi(getEnv("POW_DIFFICULTY_MAX", "28")); err != nil {
		return c, fmt.Errorf("POW_DIFFICULTY_MAX: %w", err)
	}
	if c.POWTimeout, err = time.ParseDuration(getEnv("POW_TIMEOUT", "30s")); err != nil {
		return c, fmt.Errorf("POW_TIMEOUT: %w", err)
	}
	if c.RelayCacheTTL, err = time.ParseDuration(getEnv("RELAY_CACHE_TTL", "1h")); err != nil {
		return c, fmt.Errorf("RELAY_CACHE_TTL: %w", err)
	}
	if c.PublishTimeout, err = time.ParseDuration(getEnv("PUBLISH_TIMEOUT", "10s")); err != nil {
		return c, fmt.Errorf("PUBLISH_TIMEOUT: %w", err)
	}
	if c.ScheduleGrace, err = time.ParseDuration(getEnv("SCHEDULE_GRACE", "2h")); err != nil {
		return c, fmt.Errorf("SCHEDULE_GRACE: %w", err)
	}
	if c.VerifyHTTPTimeout, err = time.ParseDuration(getEnv("VERIFY_HTTP_TIMEOUT", "10s")); err != nil {
		return c, fmt.Errorf("VERIFY_HTTP_TIMEOUT: %w", err)
	}

	nsecHex := getEnv("NSEC_HEX", "")
	pubHex := getEnv("OWNER_PUBKEY", "")
	if nsecHex == "" {
		return c, errors.New("NSEC_HEX is required")
	}
	if pubHex == "" {
		return c, errors.New("OWNER_PUBKEY is required")
	}
	if c.BlossomURL == "" {
		return c, errors.New("BLOSSOM_URL is required")
	}
	if c.NSEC, err = nostr.SecretKeyFromHex(nsecHex); err != nil {
		return c, fmt.Errorf("NSEC_HEX: %w", err)
	}
	if c.OwnerPubkey, err = nostr.PubKeyFromHex(pubHex); err != nil {
		return c, fmt.Errorf("OWNER_PUBKEY: %w", err)
	}
	if derived := nostr.GetPublicKey(c.NSEC); derived != c.OwnerPubkey {
		return c, fmt.Errorf("OWNER_PUBKEY (%s) does not match the pubkey derived from NSEC_HEX (%s)",
			c.OwnerPubkey.Hex(), derived.Hex())
	}
	return c, nil
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
