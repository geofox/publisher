package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestAuthTablesExist(t *testing.T) {
	s := openTestStore(t)
	for _, tbl := range []string{"users", "sessions", "api_tokens"} {
		var name string
		err := s.sql.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", tbl, err)
		}
	}
}

func TestUpsertUser(t *testing.T) {
	s := openTestStore(t)
	u1, err := s.UpsertUser("sub-1", "a@example.com", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID == "" || u1.Subject != "sub-1" {
		t.Fatalf("bad user: %+v", u1)
	}
	u2, err := s.UpsertUser("sub-1", "alice@example.com", "Alice B")
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("id changed on re-upsert: %s != %s", u2.ID, u1.ID)
	}
	if u2.Email != "alice@example.com" || u2.Name != "Alice B" {
		t.Fatalf("not updated: %+v", u2)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := openTestStore(t)
	u, _ := s.UpsertUser("sub-1", "a@e.com", "A")

	raw, err := s.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("empty session token")
	}
	got, err := s.SessionUser(raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("wrong user: %s", got.ID)
	}
	if err := s.DeleteSession(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(raw); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestSessionExpiryAndSweep(t *testing.T) {
	s := openTestStore(t)
	u, _ := s.UpsertUser("sub-1", "", "")
	raw, _ := s.CreateSession(u.ID, -time.Minute) // already expired
	if _, err := s.SessionUser(raw); err == nil {
		t.Fatal("expired session should not resolve")
	}
	n, err := s.SweepExpiredSessions()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d want 1", n)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	s := openTestStore(t)
	tok, raw, err := s.CreateAPIToken("n8n")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" || tok.ID == "" || tok.Name != "n8n" {
		t.Fatalf("bad token: %+v raw=%q", tok, raw)
	}
	got, err := s.APITokenByRaw(raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("wrong token")
	}
	list, err := s.ListAPITokens()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if err := s.RevokeAPIToken(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APITokenByRaw(raw); err == nil {
		t.Fatal("revoked token should not resolve")
	}
	// Revoking an unknown or already-revoked id reports sql.ErrNoRows so a
	// handler can surface 404 instead of a misleading success.
	if err := s.RevokeAPIToken("does-not-exist"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoke unknown id: got %v want sql.ErrNoRows", err)
	}
	if err := s.RevokeAPIToken(tok.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("re-revoke: got %v want sql.ErrNoRows", err)
	}
}
