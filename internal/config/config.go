package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
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
	// MediaDir is where media on this server's disk are kept (MEDIA_DIR;
	// default: media beside the database).
	MediaDir string

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

	// PublicURL is the origin the owner's browser uses (PUBLIC_URL), as
	// scheme://host[:port] without a trailing slash; "" when unset. It is
	// an allowed host, the app's base URL, the passkey relying party and
	// the default base of OIDC_REDIRECT_URL.
	PublicURL string
	// AuthMode is AUTH_MODE as given: oidc, local, proxy, or "" (resolved
	// at start from OIDC_ISSUER and the database's birth version; see
	// ResolveAuthMode).
	AuthMode string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is
	// believed (TRUSTED_PROXIES).
	TrustedProxies []netip.Prefix
	// SetupTokenFile, when set, receives the first-run setup URL instead
	// of the log (SETUP_TOKEN_FILE).
	SetupTokenFile string
	// PrivateDestinations are the private hosts and ranges that addresses
	// the owner sets in the app may reach (PRIVATE_DESTINATIONS).
	PrivateDestinations netguard.Policy
}

// SecretVars are the configuration's credential variables (a webhook URL
// can carry its secret in the path). The server removes them, and every
// other credential variable, from its environment once read, so no
// subprocess inherits them.
var SecretVars = []string{"OIDC_CLIENT_SECRET", "DEEPL_API_KEY", "PUBLIC_FEED_TOKEN",
	"FEED_WEBHOOK_URL", "FEED_WEBHOOK_TOKEN", "ALERT_WEBHOOK_URL", "ALERT_WEBHOOK_PASS"}

// Auth modes (AUTH_MODE).
const (
	AuthOIDC  = "oidc"
	AuthLocal = "local"
	AuthProxy = "proxy"
)

// LocalAuthSchema is the first schema version created by a build with
// local sign-in: a database born at it or later defaults to AUTH_MODE=local.
const LocalAuthSchema = 12

// ResolveAuthMode is the mode the server runs in: AUTH_MODE when set;
// otherwise oidc with OIDC_ISSUER, local for a database created by a build
// with local sign-in (createdSchema >= LocalAuthSchema), and proxy for an
// older one, which keeps an existing deployment without OIDC working as
// before.
func (c Config) ResolveAuthMode(createdSchema int) string {
	switch {
	case c.AuthMode != "":
		return c.AuthMode
	case c.OIDCEnabled():
		return AuthOIDC
	case createdSchema >= LocalAuthSchema:
		return AuthLocal
	}
	return AuthProxy
}

// CheckLocal reports why local sign-in can't use PUBLIC_URL, or nil: a
// passkey's relying party must be a domain name, never an IP address.
func (c Config) CheckLocal() error {
	if c.PublicURL == "" {
		return nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return fmt.Errorf("PUBLIC_URL: %w", err)
	}
	if _, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil {
		return errors.New("PUBLIC_URL: local sign-in needs a domain name, not an IP address (passkeys refuse IP addresses); use a name such as post.example.com, or http://localhost")
	}
	return nil
}

// OIDCEnabled reports whether app-level OIDC auth is configured.
func (c Config) OIDCEnabled() bool { return c.OIDCIssuer != "" }

// LoadFrom reads the configuration through getenv (os.Getenv, or a test's
// map). Every invalid variable is reported, not only the first, so one
// restart fixes them all.
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
	c.MediaDir = getEnv("MEDIA_DIR", filepath.Join(filepath.Dir(c.DBPath), "media"))

	// Kept as given: the issuer must match the provider's exactly, and
	// some end in "/" (auth.New also tries the other form).
	c.OIDCIssuer = strings.TrimSpace(getEnv("OIDC_ISSUER", ""))
	c.OIDCClientID = getEnv("OIDC_CLIENT_ID", "")
	c.OIDCClientSecret = getEnv("OIDC_CLIENT_SECRET", "")
	c.PublicURL = l.publicURL("PUBLIC_URL")
	c.OIDCRedirectURL = getEnv("OIDC_REDIRECT_URL", "")
	if c.OIDCRedirectURL == "" && c.PublicURL != "" && c.OIDCIssuer != "" {
		c.OIDCRedirectURL = c.PublicURL + "/auth/callback"
	}
	c.OIDCAllowedSubjects = splitCSV(getEnv("OIDC_ALLOWED_SUBJECTS", ""))
	c.OIDCAllowedEmails = splitCSV(getEnv("OIDC_ALLOWED_EMAILS", ""))
	c.OIDCScopes = splitCSV(getEnv("OIDC_SCOPES", "openid,profile,email"))
	c.OIDCEndSession = l.bool("OIDC_END_SESSION", true)
	c.AllowedHosts = splitCSV(getEnv("ALLOWED_HOSTS", ""))
	c.SessionTTL, _ = l.duration("SESSION_TTL", "168h")
	c.AuthMode = strings.ToLower(strings.TrimSpace(getEnv("AUTH_MODE", "")))
	switch c.AuthMode {
	case "", AuthProxy:
	case AuthOIDC:
		if !c.OIDCEnabled() {
			l.fail("AUTH_MODE=oidc needs OIDC_ISSUER (and the other OIDC_* variables)")
		}
	case AuthLocal:
		if err := c.CheckLocal(); err != nil {
			l.errs = append(l.errs, err)
		}
	default:
		l.fail("AUTH_MODE must be oidc, local or proxy (or unset), not %q", c.AuthMode)
	}
	if c.OIDCEnabled() && (c.AuthMode == AuthLocal || c.AuthMode == AuthProxy) {
		l.fail("OIDC_ISSUER is set but AUTH_MODE=%s: unset OIDC_ISSUER, or set AUTH_MODE=oidc", c.AuthMode)
	}
	c.TrustedProxies = l.prefixes("TRUSTED_PROXIES")
	if pd, err := netguard.ParsePrivateDestinations(getEnv("PRIVATE_DESTINATIONS", "")); err != nil {
		l.errs = append(l.errs, err)
	} else {
		c.PrivateDestinations = pd
	}
	c.SetupTokenFile = strings.TrimSpace(getEnv("SETUP_TOKEN_FILE", ""))
	if c.SetupTokenFile != "" {
		if fi, err := os.Stat(filepath.Dir(c.SetupTokenFile)); err != nil || !fi.IsDir() {
			l.fail("SETUP_TOKEN_FILE: the directory %s must exist", filepath.Dir(c.SetupTokenFile))
		}
	}
	if c.OIDCEnabled() {
		if c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCRedirectURL == "" {
			l.fail("OIDC_ISSUER is set; OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_REDIRECT_URL (or PUBLIC_URL) are required")
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

// publicURL reads an origin: https://host[:port], or http://localhost[:port]
// (browsers treat localhost as secure). No path, query, fragment or user.
func (l *loader) publicURL(k string) string {
	v := strings.TrimSpace(l.getenv(k))
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Opaque != "" {
		l.fail("%s must be an origin such as https://post.example.com, not %q", k, v)
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/"):
		l.fail("%s must be an origin only (scheme, host and port), such as https://post.example.com", k)
		return ""
	case u.Scheme == "https":
	case u.Scheme == "http" && host == "localhost":
	default:
		l.fail("%s must use https (http only for localhost): sessions and passkeys need a secure context", k)
		return ""
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			l.fail("%s has an invalid port %q", k, p)
			return ""
		}
	}
	h := host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	// The form a browser sends in Origin: no default port.
	if p := u.Port(); p != "" && !(u.Scheme == "https" && p == "443") && !(u.Scheme == "http" && p == "80") {
		h = net.JoinHostPort(strings.Trim(h, "[]"), p)
	}
	return u.Scheme + "://" + h
}

// prefixes reads a comma-separated list of CIDRs or addresses.
func (l *loader) prefixes(k string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range splitCSV(l.getenv(k)) {
		if p, err := netip.ParsePrefix(s); err == nil {
			// An IPv4-mapped IPv6 prefix (::ffff:10.0.0.0/104) means the
			// IPv4 one: peers are compared unmapped.
			if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
				p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
			}
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		l.fail("%s: %q is neither an address nor a CIDR such as 172.16.0.0/12", k, s)
	}
	return out
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
