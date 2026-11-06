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

	// DeepLAPIKey enables /v1/translate and draft translation.
	// Free vs Pro is auto-detected from the key suffix (":fx" → Free). Unset
	// → translation disabled (the UI hides the button).
	DeepLAPIKey string

	// PublicFeedToken gates GET /v1/public/feed (empty → endpoint disabled).
	PublicFeedToken string
	// FeedWebhookURL: signal-only ping POSTed when a feed-eligible post is
	// published (empty → no webhook). FeedWebhookToken is sent as a bearer token.
	FeedWebhookURL   string
	FeedWebhookToken string

	// Auto-retry / Retrier settings.
	AutoRetryEnabled     bool
	PublishWorkers       int // items the engine sends at once
	AutoRetryMaxAttempts int
	AutoRetryBaseDelay   time.Duration
	AutoRetryMaxDelay    time.Duration
	RetrierTick          time.Duration

	// Primary fan-out settings (Fix 2).
	NostrPrimaryFanout bool
	PrimaryRelays      []string
	FanoutTick         time.Duration

	// Video pipeline. Enabled only when FFmpegPath points at an existing binary.
	// VideoWorkdir overrides the default tempdir location for upload staging and
	// transcode outputs (never bare os.TempDir(): the runner's startup sweep
	// deletes everything in the workdir; dev /tmp is RAM-backed tmpfs).
	FFmpegPath   string
	FFprobePath  string
	VideoWorkdir string

	// OIDC Relying-Party auth. When OIDCIssuer is empty, auth is dormant and all
	// gates are off (behaves exactly as before — relies on the reverse proxy).
	OIDCIssuer          string
	OIDCClientID        string
	OIDCClientSecret    string
	OIDCRedirectURL     string
	OIDCAllowedSubjects []string
	OIDCAllowedEmails   []string
	OIDCScopes          []string
	OIDCEndSession      bool
	SessionTTL          time.Duration
	// AllowedHosts are the Host names served (ALLOWED_HOSTS); empty = any.
	AllowedHosts []string
}

// OIDCEnabled reports whether app-level OIDC auth is configured.
func (c Config) OIDCEnabled() bool { return c.OIDCIssuer != "" }

// Load reads the configuration from the environment. Every invalid
// variable is reported, not only the first, so one restart fixes them all.
func Load() (Config, error) {
	var l loader
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

	c.POWDifficultyDefault, _ = l.int("POW_DIFFICULTY_DEFAULT", "16")
	c.POWDifficultyMax, _ = l.int("POW_DIFFICULTY_MAX", "28")
	c.POWTimeout, _ = l.duration("POW_TIMEOUT", "30s")
	c.RelayCacheTTL, _ = l.duration("RELAY_CACHE_TTL", "1h")
	c.PublishTimeout, _ = l.duration("PUBLISH_TIMEOUT", "10s")
	grace, graceOK := l.duration("SCHEDULE_GRACE", "2h")
	c.ScheduleGrace = grace
	c.AutoRetryEnabled = l.bool("AUTO_RETRY_ENABLED", true)
	if n, ok := l.int("PUBLISH_WORKERS", "4"); ok && n < 1 {
		l.fail("PUBLISH_WORKERS must be a positive integer")
	} else {
		c.PublishWorkers = n
	}
	attempts, attemptsOK := l.int("AUTO_RETRY_MAX_ATTEMPTS", "6")
	c.AutoRetryMaxAttempts = attempts
	base, baseOK := l.duration("AUTO_RETRY_BASE_DELAY", "2m")
	c.AutoRetryBaseDelay = base
	maxDelay, maxOK := l.duration("AUTO_RETRY_MAX_DELAY", "1h")
	c.AutoRetryMaxDelay = maxDelay
	if graceOK && c.ScheduleGrace <= 0 {
		l.fail("SCHEDULE_GRACE must be positive (a scheduled post later than this is marked missed)")
	}
	if c.AutoRetryEnabled {
		if attemptsOK && c.AutoRetryMaxAttempts < 1 {
			l.fail("AUTO_RETRY_MAX_ATTEMPTS must be at least 1; set AUTO_RETRY_ENABLED=false to turn retries off")
		}
		if baseOK && maxOK && (c.AutoRetryBaseDelay <= 0 || c.AutoRetryMaxDelay < c.AutoRetryBaseDelay) {
			l.fail("AUTO_RETRY_BASE_DELAY must be positive and AUTO_RETRY_MAX_DELAY at least as long")
		}
	}
	c.RetrierTick, _ = l.duration("RETRIER_TICK", "60s")
	c.NostrPrimaryFanout = l.bool("NOSTR_PRIMARY_FANOUT", false)
	c.PrimaryRelays = splitCSV(getEnv("PRIMARY_RELAYS", c.NIP65BootstrapRelay))
	if len(c.PrimaryRelays) == 0 {
		c.PrimaryRelays = []string{c.NIP65BootstrapRelay}
	}
	c.FanoutTick, _ = l.duration("FANOUT_TICK", "5s")
	c.VerifyHTTPTimeout, _ = l.duration("VERIFY_HTTP_TIMEOUT", "10s")

	nsecHex := getEnv("NSEC_HEX", "")
	pubHex := getEnv("OWNER_PUBKEY", "")
	if nsecHex == "" {
		l.fail("NSEC_HEX is required")
	}
	if pubHex == "" {
		l.fail("OWNER_PUBKEY is required")
	}
	if c.BlossomURL == "" {
		l.fail("BLOSSOM_URL is required")
	}
	var nsecOK, pubOK bool
	if nsecHex != "" {
		var err error
		if c.NSEC, err = nostr.SecretKeyFromHex(nsecHex); err != nil {
			l.errs = append(l.errs, fmt.Errorf("NSEC_HEX: %w", err))
		} else {
			nsecOK = true
		}
	}
	if pubHex != "" {
		var err error
		if c.OwnerPubkey, err = nostr.PubKeyFromHex(pubHex); err != nil {
			l.errs = append(l.errs, fmt.Errorf("OWNER_PUBKEY: %w", err))
		} else {
			pubOK = true
		}
	}
	if nsecOK && pubOK {
		if derived := nostr.GetPublicKey(c.NSEC); derived != c.OwnerPubkey {
			l.fail("OWNER_PUBKEY (%s) does not match the pubkey derived from NSEC_HEX (%s)",
				c.OwnerPubkey.Hex(), derived.Hex())
		}
	}

	c.FFmpegPath = getEnv("FFMPEG_PATH", "/usr/local/bin/ffmpeg")
	c.FFprobePath = getEnv("FFPROBE_PATH", "/usr/local/bin/ffprobe")
	c.VideoWorkdir = getEnv("VIDEO_WORKDIR", "")

	// Kept as given: the issuer must match the provider's exactly, and
	// some end in "/" (auth.New also tries the other form).
	c.OIDCIssuer = strings.TrimSpace(getEnv("OIDC_ISSUER", ""))
	c.OIDCClientID = getEnv("OIDC_CLIENT_ID", "")
	c.OIDCClientSecret = getEnv("OIDC_CLIENT_SECRET", "")
	c.OIDCRedirectURL = getEnv("OIDC_REDIRECT_URL", "")
	c.OIDCAllowedSubjects = splitCSV(getEnv("OIDC_ALLOWED_SUBJECTS", ""))
	c.OIDCAllowedEmails = splitCSV(getEnv("OIDC_ALLOWED_EMAILS", ""))
	c.OIDCScopes = splitCSV(getEnv("OIDC_SCOPES", "openid,profile,email"))
	c.OIDCEndSession = l.bool("OIDC_END_SESSION", true)
	c.AllowedHosts = splitCSV(getEnv("ALLOWED_HOSTS", ""))
	c.SessionTTL, _ = l.duration("SESSION_TTL", "168h")
	if c.OIDCEnabled() {
		if c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCRedirectURL == "" {
			l.fail("OIDC_ISSUER is set; OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_REDIRECT_URL are required")
		}
		if len(c.OIDCAllowedSubjects) == 0 && len(c.OIDCAllowedEmails) == 0 {
			l.fail("OIDC enabled but no allowlist: set OIDC_ALLOWED_SUBJECTS and/or OIDC_ALLOWED_EMAILS")
		}
	}
	return c, l.err()
}

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// loader parses variables and collects every error.
type loader struct{ errs []error }

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) err() error { return errors.Join(l.errs...) }

func (l *loader) int(k, d string) (int, bool) {
	n, err := strconv.Atoi(getEnv(k, d))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", k, err))
		return 0, false
	}
	return n, true
}

func (l *loader) duration(k, d string) (time.Duration, bool) {
	v, err := time.ParseDuration(getEnv(k, d))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", k, err))
		return 0, false
	}
	return v, true
}

func (l *loader) bool(k string, d bool) bool {
	v, err := ParseBool(k, os.Getenv(k), d)
	if err != nil {
		l.errs = append(l.errs, err)
	}
	return v
}

// ParseBool reads a boolean variable: 1/true/yes/on or 0/false/no/off in any
// case. An empty value is the default; anything else is an error naming the
// variable, so a typo can't silently keep the default.
func ParseBool(name, value string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return def, fmt.Errorf("%s: %q is not a boolean (use true/false, 1/0, yes/no or on/off)", name, value)
	}
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
