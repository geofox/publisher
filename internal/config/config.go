package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

const DefaultPort = "8080"

// Config is the operator's configuration. Destination accounts are not in
// it: each driver reads its own variables (destination.EnvSource).
type Config struct {
	BlossomURL     string
	POWTimeout     time.Duration
	RelayCacheTTL  time.Duration
	PublishTimeout time.Duration
	ScheduleGrace  time.Duration
	Port           string
	LogLevel       string
	DBPath         string

	AlertWebhookURL  string
	AlertWebhookUser string
	AlertWebhookPass string

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

	// FanoutTick is how often the Nostr fan-out queue is worked.
	FanoutTick time.Duration

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
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads the configuration through getenv (os.Getenv, or a test's
// map).
func LoadFrom(getenv func(string) string) (Config, error) {
	l := loader{getenv: getenv}
	getEnv := l.get
	c := Config{
		BlossomURL:       strings.TrimRight(getEnv("BLOSSOM_URL", ""), "/"),
		Port:             getEnv("PORT", DefaultPort),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
		DBPath:           getEnv("DB_PATH", "/data/publisher.db"),
		AlertWebhookURL:  getEnv("ALERT_WEBHOOK_URL", ""),
		AlertWebhookUser: getEnv("ALERT_WEBHOOK_USER", "alertmanager"),
		AlertWebhookPass: getEnv("ALERT_WEBHOOK_PASS", ""),
		PLCDirectoryURL:  getEnv("PLC_DIRECTORY_URL", "https://plc.directory"),
	}
	c.UserLanguages = splitCSV(getEnv("USER_LANGUAGES", ""))
	c.DeepLAPIKey = getEnv("DEEPL_API_KEY", "")
	c.PublicFeedToken = getEnv("PUBLIC_FEED_TOKEN", "")
	c.FeedWebhookURL = getEnv("FEED_WEBHOOK_URL", "")
	c.FeedWebhookToken = getEnv("FEED_WEBHOOK_TOKEN", "")

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
	c.FanoutTick, _ = l.duration("FANOUT_TICK", "5s")
	c.VerifyHTTPTimeout, _ = l.duration("VERIFY_HTTP_TIMEOUT", "10s")

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

// loader reads variables through getenv and collects every error.
type loader struct {
	getenv func(string) string
	errs   []error
}

// get is variable k, or d when it is unset or empty.
func (l *loader) get(k, d string) string {
	if v := l.getenv(k); v != "" {
		return v
	}
	return d
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

func (l *loader) err() error { return errors.Join(l.errs...) }

func (l *loader) int(k, d string) (int, bool) {
	n, err := strconv.Atoi(l.get(k, d))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", k, err))
		return 0, false
	}
	return n, true
}

func (l *loader) duration(k, d string) (time.Duration, bool) {
	v, err := time.ParseDuration(l.get(k, d))
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: %w", k, err))
		return 0, false
	}
	return v, true
}

func (l *loader) bool(k string, d bool) bool {
	v, err := ParseBool(k, l.getenv(k), d)
	if err != nil {
		l.errs = append(l.errs, err)
	}
	return v
}

// ParseBool reads a boolean variable strictly (see destination.ParseBool).
func ParseBool(name, value string, def bool) (bool, error) {
	return destination.ParseBool(name, value, def)
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
