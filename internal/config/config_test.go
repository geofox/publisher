package config

import (
	"testing"
	"time"
)

// These are the exact valid test keys already used elsewhere in config_test.go.
const (
	tNSEC = "0000000000000000000000000000000000000000000000000000000000000001"
	tPUB  = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func TestAutoRetryDefaults(t *testing.T) {
	t.Setenv("NSEC_HEX", tNSEC)
	t.Setenv("OWNER_PUBKEY", tPUB)
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
	t.Setenv("NSEC_HEX", tNSEC)
	t.Setenv("OWNER_PUBKEY", tPUB)
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
	// Real matching keypair: secret 00..01, its derived x-only pubkey.
	t.Setenv("NSEC_HEX", "0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("OWNER_PUBKEY", "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
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
	t.Setenv("NSEC_HEX", "0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("OWNER_PUBKEY", "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
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
	t.Setenv("NSEC_HEX", tNSEC)
	t.Setenv("OWNER_PUBKEY", tPUB)
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

func TestFeedEnvVars(t *testing.T) {
	if got := getEnv("PUBLIC_FEED_TOKEN", ""); got != "" {
		t.Errorf("default PUBLIC_FEED_TOKEN = %q, want empty", got)
	}

	// Verify that Load() reads the feed env vars into the correct Config fields.
	t.Setenv("NSEC_HEX", "0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("OWNER_PUBKEY", "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
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
