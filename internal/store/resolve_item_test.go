package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// unknownThread saves a three-item thread whose first item is live and
// whose second was in flight when the process stopped: unknown, and its
// target given up.
func unknownThread(t *testing.T, s *Store, id string) *Post {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, id, now, 3, "bluesky")
	record(t, s, claim(t, s, now, ClaimFilter{}), ItemPublished)
	c := claim(t, s, now, ClaimFilter{})
	if c == nil || c.Ordinal != 1 {
		t.Fatalf("claim = %+v", c)
	}
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemUnknown, GiveUp: true,
		ErrorClass: "unknown_outcome", Error: "may have been posted"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPost(id)
	if err != nil || !p.Attention || p.Targets[0].GaveUpAt == nil {
		t.Fatalf("seed: %+v, %v", p, err)
	}
	return p
}

func TestResolveItemPublishedContinuesTheThread(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	p := unknownThread(t, s, "p")
	tg := p.Targets[0]
	if err := s.ResolveItem(ctx, Resolution{PostID: "p", TargetID: tg.ID, Ordinal: 1, Published: true,
		RemoteID: "at://x/1", RemoteURL: "https://bsky.app/x/1", Ref: []byte(`{"cid":"c1"}`), Continue: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPost("p")
	it := p.Targets[0].Items[1]
	if it.Status != ItemPublished || it.RemoteID != "at://x/1" || it.RemoteURL != "https://bsky.app/x/1" ||
		string(it.Ref) != `{"cid":"c1"}` || it.Error != "" || it.PublishedAt == nil {
		t.Fatalf("item = %+v", it)
	}
	if p.Targets[0].GaveUpAt != nil || p.Targets[0].Status != "sending" || p.Status != "sending" || p.Attention {
		t.Fatalf("target %+v, post %s, attention %v", p.Targets[0], p.Status, p.Attention)
	}
	c := claim(t, s, time.Now().UTC(), ClaimFilter{})
	if c == nil || c.Ordinal != 2 {
		t.Fatalf("the thread's last item is not due: %+v", c)
	}
	evs := postEvents(t, s, "p", 0)
	if !hasEvent(evs, EventItemPublished, 1) {
		t.Fatalf("no item.published for ordinal 1 in %+v", evs)
	}
	// Twice: the item is no longer unknown.
	if err := s.ResolveItem(ctx, Resolution{PostID: "p", TargetID: tg.ID, Ordinal: 1, Published: true}); !errors.Is(err, ErrNotUnknown) {
		t.Fatalf("second resolve: %v", err)
	}
}

func TestResolveItemPublishedWithoutReferenceStops(t *testing.T) {
	s := openEngineStore(t)
	p := unknownThread(t, s, "p")
	if err := s.ResolveItem(context.Background(), Resolution{PostID: "p", TargetID: p.Targets[0].ID, Ordinal: 1,
		Published: true, RemoteURL: "https://threads.net/x"}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPost("p")
	if p.Targets[0].Status != "partial" || p.Status != "partial" || p.Targets[0].Items[2].Status != ItemCanceled {
		t.Fatalf("target %+v, post %s", p.Targets[0], p.Status)
	}
	if c := claim(t, s, time.Now().UTC(), ClaimFilter{}); c != nil {
		t.Fatalf("the rest went out with nothing to reply to: %+v", c)
	}
}

// The same when the target isn't given up (a retry by hand made it due):
// the rest must not go out replying to nothing.
func TestResolveItemPublishedWithoutReferenceAfterRetry(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	p := unknownThread(t, s, "p")
	if _, err := s.MarkDue(ctx, "p", []int64{p.Targets[0].ID}, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveItem(ctx, Resolution{PostID: "p", TargetID: p.Targets[0].ID, Ordinal: 1, Published: true}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, time.Now().UTC().Add(2*time.Hour), ClaimFilter{}); c != nil {
		t.Fatalf("claimed %+v with nothing to reply to", c)
	}
}

func TestResolveItemNotPublishedSendsAgain(t *testing.T) {
	s := openEngineStore(t)
	p := unknownThread(t, s, "p")
	if err := s.ResolveItem(context.Background(), Resolution{PostID: "p", TargetID: p.Targets[0].ID, Ordinal: 1,
		Continue: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPost("p")
	if it := p.Targets[0].Items[1]; it.Status != ItemFailed || it.AttemptCount != 0 || p.Targets[0].Status != "sending" || p.Status != "sending" || p.Attention {
		t.Fatalf("target %+v, post %s", p.Targets[0], p.Status)
	}
	c := claim(t, s, time.Now().UTC(), ClaimFilter{})
	if c == nil || c.Ordinal != 1 {
		t.Fatalf("the item is not due again: %+v", c)
	}
	if !hasEvent(postEvents(t, s, "p", 0), EventItemFailed, 1) {
		t.Fatal("no item.failed event")
	}
}

func TestResolveItemOnACanceledPostSendsNothing(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	p := unknownThread(t, s, "p")
	if err := s.CancelPost(ctx, "p", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveItem(ctx, Resolution{PostID: "p", TargetID: p.Targets[0].ID, Ordinal: 1, Continue: true}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, time.Now().UTC(), ClaimFilter{}); c != nil {
		t.Fatalf("a canceled post sent %+v", c)
	}
	p, _ = s.GetPost("p")
	if st := p.Targets[0].Items[1].Status; st != ItemFailed || p.Status == "sending" {
		t.Fatalf("item %s, post %s", st, p.Status)
	}
	// Not an item of this post, or not unknown.
	if err := s.ResolveItem(ctx, Resolution{PostID: "p", TargetID: p.Targets[0].ID, Ordinal: 0}); !errors.Is(err, ErrNotUnknown) {
		t.Fatalf("published item: %v", err)
	}
	if err := s.ResolveItem(ctx, Resolution{PostID: "other", TargetID: p.Targets[0].ID, Ordinal: 1}); !errors.Is(err, ErrNotUnknown) {
		t.Fatalf("another post: %v", err)
	}
}

func hasEvent(evs []Event, typ string, ordinal int) bool {
	for _, e := range evs {
		if e.Type == typ && e.Data.Ordinal != nil && *e.Data.Ordinal == ordinal {
			return true
		}
	}
	return false
}
