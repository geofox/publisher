package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func flowSecret(owner, id, name string) SealedSecret {
	return SealedSecret{Owner: owner, Scope: SecretScopeOAuthFlow, Ref: id, Name: name, Sealed: "v1.k1.x", KeyID: "k1"}
}

// A flow is found by its state's hash, claimed once (two racing callbacks
// get one claim), not after it expired, and keeps its outcome; its sealed
// data goes when it ends, and with it when purged.
func TestOAuthFlows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := DefaultOwner
	f := OAuthFlow{ID: NewFlowID(), OwnerID: owner, UserID: "u1", Type: "mastodon", Method: "oauth",
		RedirectURI: "https://p.example/oauth/callback/mastodon", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if err := s.CreateOAuthFlow(ctx, f, "hash1", []SealedSecret{flowSecret(owner, f.ID, "data")}); err != nil {
		t.Fatal(err)
	}
	// Secrets bound elsewhere are refused.
	g := f
	g.ID = NewFlowID()
	if err := s.CreateOAuthFlow(ctx, g, "hash2", []SealedSecret{flowSecret(owner, f.ID, "data")}); err == nil {
		t.Fatal("a secret bound to another flow was taken")
	}
	got, err := s.OAuthFlowByState(ctx, "hash1")
	if err != nil || got.ID != f.ID || got.Status != FlowPending || got.UsedAt != nil || got.AccountID != "" {
		t.Fatalf("by state: %+v %v", got, err)
	}
	if _, err := s.OAuthFlowByState(ctx, "nope"); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("unknown state: %v", err)
	}
	if _, err := s.OAuthFlow(ctx, "someone-else", f.ID); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("another owner's flow: %v", err)
	}

	var wg sync.WaitGroup
	wins := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wins <- s.ClaimOAuthFlow(ctx, f.ID, time.Now()) == nil
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d claims won", n)
	}

	// A choice keeps the data and replaces what it names.
	cand := flowSecret(owner, f.ID, "candidates")
	if err := s.FinishOAuthFlow(ctx, owner, f.ID, FlowOutcome{Status: FlowChoose, Put: []SealedSecret{cand}}); err != nil {
		t.Fatal(err)
	}
	if ss, _ := s.Secrets(ctx, owner, SecretScopeOAuthFlow, f.ID); len(ss) != 2 {
		t.Fatalf("after choose: %d secrets", len(ss))
	}
	if err := s.FinishOAuthFlow(ctx, owner, f.ID, FlowOutcome{Status: FlowCompleted, AccountID: "acc_1"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OAuthFlow(ctx, owner, f.ID)
	if got.Status != FlowCompleted || got.ResultAccountID != "acc_1" || got.UsedAt == nil {
		t.Fatalf("completed: %+v", got)
	}
	if ss, _ := s.Secrets(ctx, owner, SecretScopeOAuthFlow, f.ID); len(ss) != 0 {
		t.Fatalf("data kept after completion: %d", len(ss))
	}
	// An ended flow keeps its outcome.
	if err := s.FinishOAuthFlow(ctx, owner, f.ID, FlowOutcome{Status: FlowFailed, ErrorCode: "denied"}); !errors.Is(err, ErrFlowClaimed) {
		t.Fatalf("finished twice: %v", err)
	}

	// Expired: no claim, then purged with its data.
	old := OAuthFlow{ID: NewFlowID(), OwnerID: owner, UserID: "u1", Type: "mastodon", Method: "oauth",
		RedirectURI: f.RedirectURI, ExpiresAt: time.Now().Add(-time.Minute)}
	if err := s.CreateOAuthFlow(ctx, old, "hash3", []SealedSecret{flowSecret(owner, old.ID, "data")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimOAuthFlow(ctx, old.ID, time.Now()); !errors.Is(err, ErrFlowClaimed) {
		t.Fatalf("expired claimed: %v", err)
	}
	if n, err := s.PurgeOAuthFlows(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if ss, _ := s.Secrets(ctx, owner, SecretScopeOAuthFlow, old.ID); len(ss) != 0 {
		t.Fatalf("purged flow's data kept: %d", len(ss))
	}
	if _, err := s.OAuthFlow(ctx, owner, f.ID); err != nil {
		t.Fatalf("a live flow was purged: %v", err)
	}
}

// One client per server and redirect URI: saving again replaces it and
// its secret; deleting forgets both.
func TestOAuthClients(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := DefaultOwner
	sec := func(id string) []SealedSecret {
		return []SealedSecret{{Owner: owner, Scope: SecretScopeOAuthClient, Ref: id, Name: "client_secret", Sealed: "v1.k1.x", KeyID: "k1"}}
	}
	c := OAuthClient{ID: NewOAuthClientID(), OwnerID: owner, Type: "mastodon", Issuer: "https://m.example",
		RedirectURI: "https://p.example/oauth/callback/mastodon", ClientID: "c1", Scopes: "read:accounts"}
	if _, err := s.OAuthClientFor(ctx, owner, c.Type, c.Issuer, c.RedirectURI); !errors.Is(err, ErrOAuthClientNotFound) {
		t.Fatalf("before: %v", err)
	}
	if err := s.SaveOAuthClient(ctx, c, sec(c.ID)); err != nil {
		t.Fatal(err)
	}
	d := c
	d.ID, d.ClientID = NewOAuthClientID(), "c2"
	if err := s.SaveOAuthClient(ctx, d, sec(d.ID)); err != nil {
		t.Fatal(err)
	}
	got, err := s.OAuthClientFor(ctx, owner, c.Type, c.Issuer, c.RedirectURI)
	if err != nil || got.ClientID != "c2" {
		t.Fatalf("after replacing: %+v %v", got, err)
	}
	if ss, _ := s.Secrets(ctx, owner, SecretScopeOAuthClient, c.ID); len(ss) != 0 {
		t.Fatal("the replaced client's secret was kept")
	}
	if err := s.DeleteOAuthClient(ctx, owner, d.ID); err != nil {
		t.Fatal(err)
	}
	if ss, _ := s.Secrets(ctx, owner, SecretScopeOAuthClient, d.ID); len(ss) != 0 {
		t.Fatal("a deleted client's secret was kept")
	}
	if _, err := s.OAuthClientFor(ctx, owner, c.Type, c.Issuer, c.RedirectURI); !errors.Is(err, ErrOAuthClientNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
