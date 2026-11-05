package store

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func idemStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestIdempotencyLifecycle(t *testing.T) {
	ctx := context.Background()
	s := idemStore(t)
	now := time.Now()

	prev, err := s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp1", now)
	if err != nil || prev != nil {
		t.Fatalf("first claim = %v, %v; want nil, nil", prev, err)
	}
	if _, err := s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp1", now); !errors.Is(err, ErrIdempotencyBusy) {
		t.Fatalf("repeat in flight: err = %v, want ErrIdempotencyBusy", err)
	}
	// Another payload while in flight is a reuse, not a wait.
	prev, err = s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp2", now)
	if err != nil || prev == nil || prev.Print != "fp1" || prev.State != IdemInFlight {
		t.Fatalf("other payload in flight = %+v, %v; want the fp1 record", prev, err)
	}
	// Keys are per principal.
	if prev, err := s.BeginIdempotent(ctx, "token:b", "k", "POST", "/v1/x", "fp1", now); err != nil || prev != nil {
		t.Fatalf("other principal = %v, %v; want a fresh claim", prev, err)
	}

	h := http.Header{"Content-Type": {"application/json"}, "Location": {"/v1/x/1"}}
	if err := s.FinishIdempotent(ctx, "token:a", "k", 201, h, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	prev, err = s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp1", now)
	if err != nil || prev == nil || prev.State != IdemDone || prev.Status != 201 ||
		string(prev.Body) != `{"id":1}` || prev.Header.Get("Location") != "/v1/x/1" {
		t.Fatalf("replay = %+v, %v", prev, err)
	}
	// A finished key can't be finished or abandoned again.
	if err := s.AbandonIdempotent(ctx, "token:a", "k"); err != nil {
		t.Fatal(err)
	}
	if prev, _ := s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp1", now); prev == nil || prev.State != IdemDone {
		t.Fatalf("abandon removed a finished key: %+v", prev)
	}

	// After the TTL the key is free again.
	later := now.Add(IdempotencyTTL + time.Second)
	if prev, err := s.BeginIdempotent(ctx, "token:a", "k", "POST", "/v1/x", "fp9", later); err != nil || prev != nil {
		t.Fatalf("after TTL = %+v, %v; want a fresh claim", prev, err)
	}
}

func TestIdempotencyStaleClaimAndAbandon(t *testing.T) {
	ctx := context.Background()
	s := idemStore(t)
	now := time.Now()
	if _, err := s.BeginIdempotent(ctx, "p", "k", "POST", "/v1/x", "fp", now); err != nil {
		t.Fatal(err)
	}
	// A claim older than IdempotencyStale died with its process.
	if prev, err := s.BeginIdempotent(ctx, "p", "k", "POST", "/v1/x", "fp", now.Add(IdempotencyStale+time.Second)); err != nil || prev != nil {
		t.Fatalf("stale claim = %+v, %v; want a fresh claim", prev, err)
	}
	if err := s.AbandonIdempotent(ctx, "p", "k"); err != nil {
		t.Fatal(err)
	}
	if prev, err := s.BeginIdempotent(ctx, "p", "k", "POST", "/v1/x", "fp", now); err != nil || prev != nil {
		t.Fatalf("after abandon = %+v, %v; want a fresh claim", prev, err)
	}
}

func TestPurgeIdempotency(t *testing.T) {
	ctx := context.Background()
	s := idemStore(t)
	now := time.Now()
	for _, k := range []string{"a", "b"} {
		if _, err := s.BeginIdempotent(ctx, "p", k, "POST", "/v1/x", "fp", now); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PurgeIdempotency(ctx, now); err != nil || n != 0 {
		t.Fatalf("purge now = %d, %v; want 0", n, err)
	}
	if n, err := s.PurgeIdempotency(ctx, now.Add(IdempotencyTTL)); err != nil || n != 2 {
		t.Fatalf("purge after TTL = %d, %v; want 2", n, err)
	}
}

func TestTokenScopesAndExpiry(t *testing.T) {
	s := idemStore(t)
	legacy, rawLegacy, err := s.CreateAPIToken("old")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.APITokenByRaw(rawLegacy)
	if err != nil || len(got.Scopes) != 2 || got.Scopes[0] != "posts:write" || got.Scopes[1] != "media:write" ||
		got.OwnerID != DefaultOwner || !got.ExpiresAt.IsZero() {
		t.Fatalf("legacy token = %+v, %v", got, err)
	}
	_, rawExp, err := s.CreateScopedToken("short", []string{"admin"}, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.APITokenByRaw(rawExp); err == nil {
		t.Fatal("an expired token authenticated")
	}
	tok, rawOK, err := s.CreateScopedToken("ok", []string{"accounts:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.APITokenByRaw(rawOK); err != nil || got.ID != tok.ID || got.ExpiresAt.IsZero() {
		t.Fatalf("live token = %+v, %v", got, err)
	}
	if err := s.RevokeAPIToken(legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APITokenByRaw(rawLegacy); err == nil {
		t.Fatal("a revoked token authenticated")
	}
	if got, err := s.APIToken(legacy.ID); err != nil || !got.Revoked || got.RevokedAt.IsZero() {
		t.Fatalf("APIToken(revoked) = %+v, %v", got, err)
	}
}

// TestMigrateV5KeepsTokensWorking: a token made before v5 keeps the scopes
// /publish and /upload-media gave it, and no expiry.
func TestMigrateV5KeepsTokensWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := openWith(t, path, migrations[:4])
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO api_tokens (id, name, token_hash, created_at) VALUES ('t1', 'n8n', ?, ?)`,
		hashToken("secret"), time.Now().UTC())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.APITokenByRaw("secret")
	if err != nil || got.ID != "t1" || len(got.Scopes) != 2 || got.Scopes[0] != "posts:write" ||
		got.Scopes[1] != "media:write" || !got.ExpiresAt.IsZero() {
		t.Fatalf("migrated token = %+v, %v", got, err)
	}
}
