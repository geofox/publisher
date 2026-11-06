package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdateAccountMeta(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	id := seedTestAccount(t, s, "mastodon", "default")
	a, err := s.Account(ctx, DefaultOwner, id)
	if err != nil || !a.DefaultSelected || !a.PublicFeed || !a.Enabled {
		t.Fatalf("new account = %+v, %v; want selected, in the feed, enabled", a, err)
	}
	// Setting what is already there changes nothing, not even updated_at.
	yes := true
	same, err := s.UpdateAccountMeta(ctx, DefaultOwner, id, AccountMeta{Enabled: &yes})
	if err != nil || !same.UpdatedAt.Equal(a.UpdatedAt) {
		t.Fatalf("no-op update moved updated_at: %v → %v (%v)", a.UpdatedAt, same.UpdatedAt, err)
	}
	label, no := "Work", false
	got, err := s.UpdateAccountMeta(ctx, DefaultOwner, id, AccountMeta{Label: &label, PublicFeed: &no})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Work" || got.PublicFeed || !got.DefaultSelected || !got.Enabled || !got.UpdatedAt.After(a.UpdatedAt) {
		t.Fatalf("updated = %+v", got)
	}
	if back, _ := s.Account(ctx, DefaultOwner, id); back != got {
		t.Fatalf("read back %+v, want %+v", back, got)
	}
	if _, err := s.UpdateAccountMeta(ctx, "own_other", id, AccountMeta{Label: &label}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("another owner's account: err = %v", err)
	}
}

// The network's identity is recorded without touching updated_at (it is
// not the owner's change).
func TestSetAccountIdentity(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	id := seedTestAccount(t, s, "bluesky", "default")
	before, _ := s.Account(ctx, DefaultOwner, id)
	if err := s.SetAccountIdentity(ctx, id, "@me.example", "did:plc:abc"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Account(ctx, DefaultOwner, id)
	if a.Handle != "@me.example" || a.RemoteID != "did:plc:abc" || !a.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("account = %+v", a)
	}
}

// A post's targets say whether their account takes part in the public
// feed, on the post page and in the feed alike.
func TestTargetsCarryFeedExclusion(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := &Post{ID: "fx", CreatedAt: now, Platforms: []string{"mastodon", "bluesky"}, Source: "test", Status: "success",
		Targets: []Target{
			{Platform: "mastodon", Status: "success", Items: []Item{{Ordinal: 0, Text: "a", Status: ItemPublished, IdemKey: "k1", RemoteURL: "https://m/1"}}},
			{Platform: "bluesky", Status: "success", Items: []Item{{Ordinal: 0, Text: "a", Status: ItemPublished, IdemKey: "k2", RemoteURL: "https://b/1"}}},
		}}
	if err := saveWithAccounts(s, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPost("fx")
	var masto string
	for _, tg := range got.Targets {
		if tg.Platform == "mastodon" {
			masto = tg.AccountID
		}
	}
	no := false
	if _, err := s.UpdateAccountMeta(ctx, DefaultOwner, masto, AccountMeta{PublicFeed: &no}); err != nil {
		t.Fatal(err)
	}
	check := func(where string, ts []Target) {
		t.Helper()
		for _, tg := range ts {
			if tg.FeedExcluded != (tg.Platform == "mastodon") {
				t.Errorf("%s: %s FeedExcluded = %v", where, tg.Platform, tg.FeedExcluded)
			}
		}
	}
	got, _ = s.GetPost("fx")
	check("GetPost", got.Targets)
	feed, err := s.PublicFeed(10)
	if err != nil || len(feed) != 1 {
		t.Fatalf("PublicFeed = %v, %v", feed, err)
	}
	check("PublicFeed", feed[0].Targets)

	// The feed names each link's account and its handle as last seen.
	bsky := ""
	for _, tg := range got.Targets {
		if tg.Platform == "bluesky" {
			bsky = tg.AccountID
		}
	}
	if err := s.SetAccountIdentity(ctx, bsky, "ann.example", "did:plc:ann"); err != nil {
		t.Fatal(err)
	}
	feed, _ = s.PublicFeed(10)
	for _, tg := range feed[0].Targets {
		want := map[string]string{"bluesky": "ann.example", "mastodon": ""}[tg.Platform]
		if tg.AccountID == "" || tg.AccountHandle != want {
			t.Errorf("PublicFeed %s target: account %q handle %q, want handle %q", tg.Platform, tg.AccountID, tg.AccountHandle, want)
		}
	}
}
