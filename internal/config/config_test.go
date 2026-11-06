package config

import (
	"strings"
	"testing"
	"time"
)

func TestAutoRetryDefaults(t *testing.T) {
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AutoRetryEnabled {
		t.Error("AutoRetryEnabled should default true")
	}
	if c.AutoRetryMaxAttempts != 6 {
		t.Errorf("AutoRetryMaxAttempts = %d, want 6", c.AutoRetryMaxAttempts)
	}
	if c.AutoRetryBaseDelay != 2*time.Minute {
		t.Errorf("AutoRetryBaseDelay = %v, want 2m", c.AutoRetryBaseDelay)
	}
	if c.AutoRetryMaxDelay != time.Hour {
		t.Errorf("AutoRetryMaxDelay = %v, want 1h", c.AutoRetryMaxDelay)
	}
	if c.RetrierTick != time.Minute {
		t.Errorf("RetrierTick = %v, want 1m", c.RetrierTick)
	}
}

func TestAutoRetryDisabled(t *testing.T) {
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	t.Setenv("AUTO_RETRY_ENABLED", "false")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AutoRetryEnabled {
		t.Error("AUTO_RETRY_ENABLED=false should disable")
	}
}

func TestVerifyConfigDefaults(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")
	t.Setenv("VERIFY_HTTP_TIMEOUT", "")
	t.Setenv("BLOSSOM_URL", "https://blossom.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PLCDirectoryURL != "https://plc.directory" {
		t.Errorf("PLCDirectoryURL default = %q", cfg.PLCDirectoryURL)
	}
	if cfg.VerifyHTTPTimeout != 10*time.Second {
		t.Errorf("VerifyHTTPTimeout default = %v", cfg.VerifyHTTPTimeout)
	}
	if len(cfg.UserLanguages) != 0 {
		t.Errorf("UserLanguages default = %v, want empty", cfg.UserLanguages)
	}
}

func TestUserLanguagesParsedFromEnv(t *testing.T) {
	t.Setenv("BLOSSOM_URL", "https://blossom.example.com")
	t.Setenv("USER_LANGUAGES", "en, fr , de")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"en", "fr", "de"}
	if len(cfg.UserLanguages) != len(want) {
		t.Fatalf("UserLanguages = %v, want %v", cfg.UserLanguages, want)
	}
	for i, w := range want {
		if cfg.UserLanguages[i] != w {
			t.Errorf("UserLanguages[%d] = %q, want %q", i, cfg.UserLanguages[i], w)
		}
	}
}

// loadWithRequiredBase sets the always-required env vars using the valid test
// keypair constants and then calls Load(). Any test that exercises optional
// config should call this instead of repeating the boilerplate.
func loadWithRequiredBase(t *testing.T) (Config, error) {
	t.Helper()
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	return Load()
}

func TestOIDCConfig(t *testing.T) {
	t.Setenv("OIDC_ISSUER", "")
	c, err := loadWithRequiredBase(t)
	if err != nil {
		t.Fatalf("dormant config errored: %v", err)
	}
	if c.OIDCEnabled() {
		t.Fatal("should be disabled when issuer unset")
	}

	t.Setenv("OIDC_ISSUER", "https://auth.example.com")
	t.Setenv("OIDC_CLIENT_ID", "publisher")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")
	t.Setenv("OIDC_ALLOWED_SUBJECTS", "")
	t.Setenv("OIDC_ALLOWED_EMAILS", "")
	if _, err := loadWithRequiredBase(t); err == nil {
		t.Fatal("expected error: enabled OIDC with empty allowlist")
	}

	t.Setenv("OIDC_ALLOWED_SUBJECTS", "sub-1")
	c, err = loadWithRequiredBase(t)
	if err != nil {
		t.Fatalf("valid OIDC config errored: %v", err)
	}
	if !c.OIDCEnabled() || c.OIDCClientID != "publisher" {
		t.Fatalf("bad config: %+v", c)
	}
}

// The Nostr fan-out settings moved to the Nostr driver (TestFromEnv there);
// the queue's tick stays here.
func TestFanoutTickDefault(t *testing.T) {
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.FanoutTick != 5*time.Second {
		t.Errorf("FanoutTick=%v, want 5s", c.FanoutTick)
	}
}

func TestFeedEnvVars(t *testing.T) {
	if c, _ := LoadFrom(func(string) string { return "" }); c.PublicFeedToken != "" {
		t.Errorf("default PUBLIC_FEED_TOKEN = %q, want empty", c.PublicFeedToken)
	}

	// Verify that Load() reads the feed env vars into the correct Config fields.
	t.Setenv("BLOSSOM_URL", "https://blossom.example.com")
	t.Setenv("PUBLIC_FEED_TOKEN", "tok")
	t.Setenv("FEED_WEBHOOK_URL", "https://hook")
	t.Setenv("FEED_WEBHOOK_TOKEN", "wtok")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicFeedToken != "tok" {
		t.Errorf("PublicFeedToken = %q, want %q", cfg.PublicFeedToken, "tok")
	}
	if cfg.FeedWebhookURL != "https://hook" {
		t.Errorf("FeedWebhookURL = %q, want %q", cfg.FeedWebhookURL, "https://hook")
	}
	if cfg.FeedWebhookToken != "wtok" {
		t.Errorf("FeedWebhookToken = %q, want %q", cfg.FeedWebhookToken, "wtok")
	}
}

// Values whose meaning would silently flip under the engine are refused.
func TestRetryAndGraceBounds(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"SCHEDULE_GRACE", "0s"},
		{"AUTO_RETRY_MAX_ATTEMPTS", "0"},
		{"AUTO_RETRY_BASE_DELAY", "0s"},
		{"AUTO_RETRY_MAX_DELAY", "1s"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv("BLOSSOM_URL", "https://b.example.com")
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%s should be refused", tc.key, tc.val)
			}
		})
	}
}

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BLOSSOM_URL", "https://b.example.com")
}

// Booleans accept the usual spellings in any case and refuse anything else,
// so a typo can't silently keep the default.
func TestStrictBooleans(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {" yes ", true}, {"On", true},
		{"0", false}, {"false", false}, {"False", false}, {"no", false}, {"OFF", false},
	} {
		t.Run(tc.val, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("AUTO_RETRY_ENABLED", tc.val)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.AutoRetryEnabled != tc.want {
				t.Errorf("AUTO_RETRY_ENABLED=%q → %v, want %v", tc.val, c.AutoRetryEnabled, tc.want)
			}
		})
	}
	for _, key := range []string{"AUTO_RETRY_ENABLED", "OIDC_END_SESSION"} {
		t.Run(key+"=flase", func(t *testing.T) {
			baseEnv(t)
			t.Setenv(key, "flase")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), `"flase"`) {
				t.Fatalf("err = %v, want one naming %s and the value", err, key)
			}
		})
	}
}

// Every invalid variable is reported at once.
func TestLoadReportsEveryError(t *testing.T) {
	baseEnv(t)
	t.Setenv("AUTO_RETRY_ENABLED", "maybe")
	t.Setenv("POW_TIMEOUT", "soon")
	t.Setenv("PUBLISH_WORKERS", "0")
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted invalid values")
	}
	for _, want := range []string{"AUTO_RETRY_ENABLED", "POW_TIMEOUT", "PUBLISH_WORKERS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}
}

// Nothing is required: a server with no media store (and no Nostr key)
// starts, and the variables are read through the getenv given.
func TestLoadFromNothingSet(t *testing.T) {
	c, err := LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadFrom with nothing set: %v", err)
	}
	if c.BlossomURL != "" || c.Port != DefaultPort {
		t.Fatalf("config = %+v", c)
	}
	env := map[string]string{"BLOSSOM_URL": "https://b.example/", "AUTO_RETRY_ENABLED": "no"}
	c, err = LoadFrom(func(k string) string { return env[k] })
	if err != nil || c.BlossomURL != "https://b.example" || c.AutoRetryEnabled {
		t.Fatalf("config = %+v, %v", c, err)
	}
}
