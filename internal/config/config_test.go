package config

import (
	"testing"
	"time"
)

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

func TestFeedEnvVars(t *testing.T) {
	if got := getEnv("PUBLIC_FEED_TOKEN", ""); got != "" {
		t.Errorf("default PUBLIC_FEED_TOKEN = %q, want empty", got)
	}
	t.Setenv("PUBLIC_FEED_TOKEN", "tok")
	t.Setenv("FEED_WEBHOOK_URL", "https://hook")
	t.Setenv("FEED_WEBHOOK_TOKEN", "wtok")
	if getEnv("PUBLIC_FEED_TOKEN", "") != "tok" ||
		getEnv("FEED_WEBHOOK_URL", "") != "https://hook" ||
		getEnv("FEED_WEBHOOK_TOKEN", "") != "wtok" {
		t.Error("feed env vars not read via getEnv")
	}
}
