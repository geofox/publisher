package store

import (
	"context"
	"testing"
	"time"
)

// A claim carries the item's stored key and account: the engine reads the
// key back and never recomputes it, so an item planned under the legacy
// scheme keeps its key after items start being keyed by account.
func TestClaimCarriesStoredKeyAndAccount(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	due := time.Now().UTC().Add(-time.Minute)
	p := &Post{ID: "k1", CreatedAt: time.Now().UTC(), Platforms: []string{"mastodon"}, Source: "test", Status: "sending",
		Targets: []Target{{Platform: "mastodon", Status: "sending", Items: []Item{
			{Ordinal: 0, Text: "a", Status: ItemPlanned, IdemKey: "stored-key", NextAttemptAt: &due}}}}}
	if err := s.SavePost(p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPost("k1")
	account := got.Targets[0].AccountID
	if account == "" {
		t.Fatal("target has no account")
	}
	c := claim(t, s, time.Now(), ClaimFilter{})
	if c == nil || c.IdemKey != "stored-key" || c.AccountID != account {
		t.Fatalf("claim = %+v, want key stored-key and account %s", c, account)
	}
	sending, err := s.SendingItems(ctx)
	if err != nil || len(sending) != 1 || sending[0].IdemKey != "stored-key" || sending[0].AccountID != account {
		t.Fatalf("SendingItems = %+v, %v", sending, err)
	}
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "rejected"}); err != nil {
		t.Fatal(err)
	}
	un, err := s.UnscheduledFailures(ctx)
	if err != nil || len(un) != 1 || un[0].IdemKey != "stored-key" || un[0].AccountID != account {
		t.Fatalf("UnscheduledFailures = %+v, %v", un, err)
	}
}

func TestAccountItemKey(t *testing.T) {
	a := AccountItemKey("p", "acc_1", 0)
	if len(a) != 32 || a == AccountItemKey("p", "acc_2", 0) || a == AccountItemKey("p", "acc_1", 1) {
		t.Errorf("keys don't separate accounts and ordinals: %s", a)
	}
	// The two schemes never collide, even for an account id equal to a type.
	if AccountItemKey("p", "mastodon", 0) == LegacyItemKey("p", "mastodon", 0) {
		t.Error("account key equals legacy key")
	}
}
