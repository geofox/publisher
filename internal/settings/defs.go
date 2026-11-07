// Package settings holds what the owner (and, for the server, an instance
// admin) changes in the app instead of the environment: languages,
// translation, retries, the public feed and webhooks, the media store.
//
// A setting's value comes from, in order: its environment variable when
// that is set (the setting is then locked: the operator's word wins), the
// value saved in the app, its default. Secret settings are sealed with the
// master key in the secret store, bound to where they are sent.
// Consumers read Current(), an immutable snapshot, at the point of use, so
// a change applies to the next request or claim.
package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Scope says whose a setting is.
type Scope string

const (
	// Owner settings belong to the account owner (settings:write).
	Owner Scope = "owner"
	// Instance settings belong to the server (instance:admin).
	Instance Scope = "instance"
)

// Source says where a setting's current value comes from.
type Source string

const (
	SourceEnv     Source = "env"
	SourceSaved   Source = "saved"
	SourceDefault Source = "default"
)

// Def declares one setting.
type Def struct {
	// Key is "<group>.<name>" ("publishing.auto_retry").
	Key   string
	Scope Scope
	// Env is the variable that sets it ("" when none does).
	Env string
	// Field is its form and validation (Field.Key is Key). A Secret field
	// is sealed in the secret store; a secret url field is its own
	// audience.
	Field destination.FieldSpec
	// AudienceOf is the url setting a secret is sent to (a webhook's
	// token goes to the webhook's address): the secret is bound to that
	// address's origin, and a new origin needs it again.
	AudienceOf string
	// Generate: Publisher can make the value (a feed token).
	Generate bool
	// FromEnv reads the variable's value as the configuration does; nil
	// reads it by the field's type.
	FromEnv func(raw string) (any, error)
	// EnvFunc, when set, decides the env value from every variable read
	// (media.store follows BLOSSOM_URL); ok false: not set by env.
	EnvFunc func(env map[string]string) (v any, ok bool)
}

// Group is the key's first part ("publishing").
func (d Def) Group() string { g, _, _ := strings.Cut(d.Key, "."); return g }

// Name is the key's last part ("auto_retry").
func (d Def) Name() string { _, n, _ := strings.Cut(d.Key, "."); return n }

// Setting keys.
const (
	ComposingLanguages    = "composing.languages"
	TranslateDeepLKey     = "translate.deepl_key"
	PublishingAutoRetry   = "publishing.auto_retry"
	PublishingMaxAttempts = "publishing.max_attempts"
	PublishingGrace       = "publishing.schedule_grace"
	FeedToken             = "feed.token"
	FeedWebhookURL        = "feed.webhook_url"
	FeedWebhookToken      = "feed.webhook_token"
	AlertsWebhookURL      = "alerts.webhook_url"
	AlertsWebhookUser     = "alerts.webhook_user"
	AlertsWebhookPass     = "alerts.webhook_pass"
	MediaStore            = "media.store"
	MediaBlossomURL       = "media.blossom_url"
	MediaBlossomKey       = "media.blossom_key"
	MediaPublic           = "media.public"
	MediaLocalQuota       = "media.local_quota"
)

// Media stores (media.store).
const (
	StoreNone    = "none"
	StoreBlossom = "blossom"
	StoreLocal   = "local"
)

func intp(n int) *int { return &n }

// BlossomURLRule is the media server's address: https (http for a listed
// private host), with a path when it has one.
var BlossomURLRule = destination.URLRule{Secure: "https", Plain: "http", Path: true}

// Defs are the built-in settings, in the order the web app shows them.
func Defs() []Def {
	return []Def{
		{Key: ComposingLanguages, Scope: Owner, Env: "USER_LANGUAGES",
			FromEnv: func(raw string) (any, error) { return splitCSV(raw), nil },
			Field: destination.FieldSpec{Label: "Languages you write in", Type: destination.FieldStringList,
				Pattern: "[a-z]{2}", MaxItems: 10, Placeholder: "en",
				Help: "Two-letter codes (en, fr, de), one per line. The first is a new post's language; with several, the composer lets you pick."}},
		{Key: TranslateDeepLKey, Scope: Owner, Env: "DEEPL_API_KEY",
			Field: destination.FieldSpec{Label: "DeepL API key", Type: destination.FieldString, Secret: true, MaxLength: 200,
				Help: "Turns on translation in the composer. A free key ends in :fx. From deepl.com, Account, API keys."}},
		{Key: PublishingAutoRetry, Scope: Owner, Env: "AUTO_RETRY_ENABLED",
			FromEnv: func(raw string) (any, error) { return destination.ParseBool("AUTO_RETRY_ENABLED", raw, true) },
			Field: destination.FieldSpec{Label: "Retry failed posts by themselves", Type: destination.FieldBool, Default: true,
				Help: "A post that fails for a reason that may pass (a server down, a rate limit) is tried again later, waiting longer each time. Off: what waits for a retry stays until you retry it or turn this on again."}},
		{Key: PublishingMaxAttempts, Scope: Owner, Env: "AUTO_RETRY_MAX_ATTEMPTS",
			FromEnv: func(raw string) (any, error) { return strconv.Atoi(strings.TrimSpace(raw)) },
			Field: destination.FieldSpec{Label: "Tries before giving up", Type: destination.FieldInt, Default: 6, Min: intp(1), Max: intp(20),
				Help: "For each post on each account, the first try included."}},
		{Key: PublishingGrace, Scope: Owner, Env: "SCHEDULE_GRACE",
			FromEnv: func(raw string) (any, error) {
				d, err := time.ParseDuration(strings.TrimSpace(raw))
				return destination.ShortDuration(d), err
			},
			Field: destination.FieldSpec{Label: "Late scheduled posts", Type: destination.FieldDuration, Default: "2h",
				Min: intp(60), Max: intp(7 * 24 * 3600), Placeholder: "2h",
				Help: "A scheduled post the server missed (it was stopped) still goes out when it is at most this late; later, it is marked missed. Such as 30m, 2h, 24h."}},
		{Key: FeedToken, Scope: Owner, Env: "PUBLIC_FEED_TOKEN", Generate: true,
			Field: destination.FieldSpec{Label: "Public feed token", Type: destination.FieldString, Secret: true, MaxLength: 200,
				Help: "The bearer token that opens GET /v1/public/feed. Without one, the feed is off."}},
		{Key: FeedWebhookURL, Scope: Owner, Env: "FEED_WEBHOOK_URL",
			Field: destination.FieldSpec{Label: "Feed webhook", Type: destination.FieldURL, URL: &destination.WebhookURL, Secret: true,
				Placeholder: "https://example.com/hooks/feed",
				Help: "Called (an empty POST) when a post that goes to the public feed is published. Kept like a password: it may carry a secret."}},
		{Key: FeedWebhookToken, Scope: Owner, Env: "FEED_WEBHOOK_TOKEN", AudienceOf: FeedWebhookURL,
			Field: destination.FieldSpec{Label: "Feed webhook token", Type: destination.FieldString, Secret: true, MaxLength: 500,
				Help: "Sent as a bearer token, so the receiver can tell the call is yours."}},
		{Key: AlertsWebhookURL, Scope: Owner, Env: "ALERT_WEBHOOK_URL",
			Field: destination.FieldSpec{Label: "Alert webhook", Type: destination.FieldURL, URL: &destination.WebhookURL, Secret: true,
				Placeholder: "https://alertmanager.example.com/api/v2/alerts",
				Help: "Told when a scheduled post fails or an account needs you (Alertmanager's API, or anything that takes its JSON)."}},
		{Key: AlertsWebhookUser, Scope: Owner, Env: "ALERT_WEBHOOK_USER",
			Field: destination.FieldSpec{Label: "Alert webhook user", Type: destination.FieldString, Default: "alertmanager", MaxLength: 200,
				Help: "Basic-auth user name, with the password below."}},
		{Key: AlertsWebhookPass, Scope: Owner, Env: "ALERT_WEBHOOK_PASS", AudienceOf: AlertsWebhookURL,
			Field: destination.FieldSpec{Label: "Alert webhook password", Type: destination.FieldString, Secret: true, MaxLength: 500,
				Help: "Basic-auth password. Without one, no Authorization is sent."}},
		{Key: MediaStore, Scope: Instance,
			EnvFunc: func(env map[string]string) (any, bool) {
				if strings.TrimSpace(env["BLOSSOM_URL"]) != "" {
					return StoreBlossom, true
				}
				return nil, false
			},
			Field: destination.FieldSpec{Label: "Where media go", Type: destination.FieldEnum, Default: StoreNone, Required: true,
				Enum: []destination.EnumValue{
					{Value: StoreNone, Label: "Nowhere: posts are text only"},
					{Value: StoreBlossom, Label: "A Blossom server"},
					{Value: StoreLocal, Label: "This server's disk"},
				},
				Help: "Where photos and videos are uploaded. Changing it applies to new uploads; media already posted stay where they are."}},
		{Key: MediaBlossomURL, Scope: Instance, Env: "BLOSSOM_URL",
			FromEnv: func(raw string) (any, error) { return strings.TrimRight(strings.TrimSpace(raw), "/"), nil },
			Field: destination.FieldSpec{Label: "Blossom server", Type: destination.FieldURL, URL: &BlossomURLRule,
				Placeholder: "https://blossom.example.com", Help: "The Blossom server's address."}},
		{Key: MediaBlossomKey, Scope: Instance, Generate: true,
			Field: destination.FieldSpec{Label: "Blossom signing key", Type: destination.FieldString, Secret: true, MaxLength: 100,
				Pattern: `nsec1[02-9ac-hj-np-zAC-HJ-NP-Z]{58}|[0-9a-fA-F]{64}`, Placeholder: "nsec1…",
				Help: "The Nostr key uploads are signed with: allow its public key on your Blossom server. Without one, uploads are signed with NSEC_HEX."}},
		{Key: MediaPublic, Scope: Instance,
			Field: destination.FieldSpec{Label: "Serve media publicly", Type: destination.FieldBool, Default: false,
				Help: "This server's disk only. Threads and Nostr fetch media by address, so they need it reachable at PUBLIC_URL/media/. Turning it on checks that it is."}},
		{Key: MediaLocalQuota, Scope: Instance,
			Field: destination.FieldSpec{Label: "Most disk space for media (MiB)", Type: destination.FieldInt, Min: intp(0), Max: intp(1 << 30),
				Help: "This server's disk only. Uploads past it are refused; empty or 0: no limit but the free-space floor."}},
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// envValue reads a def's value from the environment (ok false: not set).
func (d Def) envValue(env map[string]string) (any, bool, error) {
	if d.EnvFunc != nil {
		v, ok := d.EnvFunc(env)
		return v, ok, nil
	}
	if d.Env == "" {
		return nil, false, nil
	}
	raw := env[d.Env]
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	if d.FromEnv != nil {
		v, err := d.FromEnv(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", d.Env, err)
		}
		return v, true, nil
	}
	switch d.Field.Type {
	case destination.FieldBool:
		v, err := destination.ParseBool(d.Env, raw, false)
		return v, err == nil, err
	case destination.FieldInt:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, false, fmt.Errorf("%s: not a whole number", d.Env)
		}
		return n, true, nil
	case destination.FieldStringList, destination.FieldURLList:
		return splitCSV(raw), true, nil
	}
	if d.Field.Secret {
		return raw, true, nil // as given: a secret isn't trimmed
	}
	return strings.TrimSpace(raw), true, nil
}

// EnvNames are the variables the definitions read.
func EnvNames(defs []Def) []string {
	out := []string{"BLOSSOM_URL"}
	for _, d := range defs {
		if d.Env != "" && d.Env != "BLOSSOM_URL" {
			out = append(out, d.Env)
		}
	}
	return out
}
