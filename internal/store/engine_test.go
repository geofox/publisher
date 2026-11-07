package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openEngineStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// plannedPost saves a post with one target per platform, each with n
// planned items due at due.
func plannedPost(t *testing.T, s *Store, id string, due time.Time, n int, platforms ...string) *Post {
	t.Helper()
	p := &Post{ID: id, CreatedAt: time.Now().UTC(), Platforms: platforms, Source: "test", Status: "sending"}
	for _, pl := range platforms {
		tg := Target{Platform: pl, Status: "sending"}
		for i := 0; i < n; i++ {
			d := due
			tg.Items = append(tg.Items, Item{Ordinal: i, Text: pl + "-" + string(rune('a'+i)), Status: ItemPlanned,
				IdemKey: LegacyItemKey(id, pl, i), NextAttemptAt: &d})
		}
		p.Targets = append(p.Targets, tg)
	}
	if err := saveWithAccounts(s, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPost(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func claim(t *testing.T, s *Store, now time.Time, f ClaimFilter) *Claim {
	t.Helper()
	c, err := s.ClaimItem(context.Background(), now, f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func record(t *testing.T, s *Store, c *Claim, status string) Outcome {
	t.Helper()
	out, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c, Status: status, RemoteID: "r" + c.Platform})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A thread posts in order, one item at a time per target; targets go in
// parallel.
func TestClaimOrdersThreadItems(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 2, "bluesky", "mastodon")

	a := claim(t, s, now, ClaimFilter{})
	b := claim(t, s, now, ClaimFilter{})
	if a == nil || b == nil || a.Ordinal != 0 || b.Ordinal != 0 || a.TargetID == b.TargetID {
		t.Fatalf("want the head of each target, got %+v and %+v", a, b)
	}
	if c := claim(t, s, now, ClaimFilter{}); c != nil {
		t.Fatalf("an item was claimed while its target had one sending: %+v", c)
	}
	record(t, s, a, ItemPublished)
	next := claim(t, s, now, ClaimFilter{})
	if next == nil || next.TargetID != a.TargetID || next.Ordinal != 1 {
		t.Fatalf("want item 1 of %d, got %+v", a.TargetID, next)
	}
	// A failed head blocks the rest of its thread.
	record(t, s, b, ItemFailed)
	if c := claim(t, s, now, ClaimFilter{TargetID: b.TargetID}); c != nil {
		t.Fatalf("item after a failed head was claimed: %+v", c)
	}
}

func TestClaimHonoursDueTimesAndFilters(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Due half a second from now: not yet due now, due a second later
	// (sub-second times compare correctly).
	plannedPost(t, s, "later", now.Add(500*time.Millisecond), 1, "nostr")
	if c := claim(t, s, now, ClaimFilter{}); c != nil {
		t.Fatalf("claimed before due: %+v", c)
	}
	if c := claim(t, s, now.Add(time.Second), ClaimFilter{Exclude: []string{"later"}}); c != nil {
		t.Fatalf("claimed an excluded post: %+v", c)
	}
	c := claim(t, s, now.Add(time.Second), ClaimFilter{})
	if c == nil || c.PostID != "later" || c.Platform != "nostr" {
		t.Fatalf("claim = %+v", c)
	}
	p, _ := s.GetPost("later")
	if p.Status != "sending" || p.Targets[0].Status != "sending" || p.Targets[0].Items[0].Status != ItemSending {
		t.Errorf("claim didn't move the post to sending: %+v", p)
	}
}

func TestClaimSkipsMissedScheduledPosts(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	at := now.Add(-3 * time.Hour)
	p := plannedPost(t, s, "old", at, 1, "nostr")
	if _, err := s.sql.Exec(`UPDATE posts SET status='scheduled', scheduled_at=? WHERE id='old'`, at); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, now, ClaimFilter{MissedBefore: now.Add(-2 * time.Hour)}); c != nil {
		t.Fatalf("claimed a post past its grace: %+v", c)
	}
	missed, err := s.MarkOverdueMissed(context.Background(), now.Add(-2*time.Hour))
	if err != nil || len(missed) != 1 || missed[0].ID != "old" {
		t.Fatalf("missed = %+v, %v", missed, err)
	}
	got, _ := s.GetPost("old")
	if got.Status != "missed" || got.Targets[0].Items[0].NextAttemptAt != nil {
		t.Errorf("missed post = %+v items %+v", got, got.Targets[0].Items)
	}
	// A retry by hand makes it due again.
	if n, err := s.MarkDue(context.Background(), "old", []int64{p.Targets[0].ID}, now); err != nil || n != 1 {
		t.Fatalf("mark due = %d, %v", n, err)
	}
	if c := claim(t, s, now, ClaimFilter{}); c == nil {
		t.Fatal("retried missed post not claimable")
	}
}

func TestRecordItemResultStatuses(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "t", now, 3, "bluesky")
	c0 := claim(t, s, now, ClaimFilter{})
	if out := record(t, s, c0, ItemPublished); out.TargetStatus != "sending" || out.PostAfter != "sending" {
		t.Fatalf("after the head: %+v", out)
	}
	c1 := claim(t, s, now, ClaimFilter{})
	next := now.Add(time.Minute)
	out, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c1, Status: ItemFailed, Error: "boom",
		ErrorClass: "transient", NextAttemptAt: &next})
	if err != nil {
		t.Fatal(err)
	}
	if out.TargetStatus != "partial" || out.PostBefore != "sending" || out.PostAfter != "partial" || out.GaveUp {
		t.Fatalf("after a failed middle item: %+v", out)
	}
	p, _ := s.GetPost("t")
	tg := p.Targets[0]
	if tg.AttemptCount != 1 || len(tg.Attempts) != 2 || tg.Segments[1].Status != "failed" || tg.Segments[2].Status != "pending" {
		t.Errorf("target = %+v", tg)
	}
	if _, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c1, Status: ItemPublished}); err != ErrClaimLost {
		t.Errorf("recording an unclaimed item: err = %v, want ErrClaimLost", err)
	}
	// The retry, once due, finishes the thread.
	c1b := claim(t, s, next, ClaimFilter{})
	if c1b == nil || c1b.Ordinal != 1 || c1b.Attempts != 1 {
		t.Fatalf("retry claim = %+v", c1b)
	}
	record(t, s, c1b, ItemPublished)
	c2 := claim(t, s, next, ClaimFilter{})
	if out := record(t, s, c2, ItemPublished); out.TargetStatus != "success" || out.PostAfter != "success" {
		t.Fatalf("finished thread: %+v", out)
	}
}

func TestTargetStatusOf(t *testing.T) {
	cases := []struct {
		items []string
		want  string
	}{
		{[]string{ItemPlanned}, "sending"},
		{[]string{ItemSending, ItemPlanned}, "sending"},
		{[]string{ItemFailed, ItemPlanned}, "failed"},
		{[]string{ItemUnknown}, "failed"},
		{[]string{ItemPublished}, "success"},
		{[]string{ItemPartial}, "partial"},
		{[]string{ItemPublished, ItemPlanned}, "sending"},
		{[]string{ItemPublished, ItemFailed, ItemPlanned}, "partial"},
		{[]string{ItemPublished, ItemUnknown}, "partial"},
		{[]string{ItemPublished, ItemPartial}, "partial"},
	}
	for _, c := range cases {
		if got := targetStatusOf(c.items); got != c.want {
			t.Errorf("targetStatusOf(%v) = %q, want %q", c.items, got, c.want)
		}
	}
}

func TestSendingItemsAndResetUnstarted(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "inflight", now, 1, "mastodon")
	c := claim(t, s, now, ClaimFilter{})
	items, err := s.SendingItems(context.Background())
	if err != nil || len(items) != 1 || items[0].ItemID != c.ItemID || items[0].SentAt.Sub(now).Abs() > time.Second {
		t.Fatalf("sending items = %+v, %v", items, err)
	}
	// A scheduled post claimed by a worker that stopped before sending
	// anything goes back to scheduled.
	at := now.Add(-time.Minute)
	plannedPost(t, s, "claimed", at, 1, "nostr")
	if _, err := s.sql.Exec(`UPDATE posts SET scheduled_at=? WHERE id='claimed'`, at); err != nil {
		t.Fatal(err)
	}
	n, err := s.ResetUnstarted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v", n, err)
	}
	if p, _ := s.GetPost("claimed"); p.Status != "scheduled" || p.Targets[0].Status != "scheduled" {
		t.Errorf("claimed post = %q / %q", p.Status, p.Targets[0].Status)
	}
	if p, _ := s.GetPost("inflight"); p.Status != "sending" {
		t.Errorf("a post with an item in flight was reset: %q", p.Status)
	}
}

func TestRescheduleMovesPlannedItems(t *testing.T) {
	s := openEngineStore(t)
	at := time.Now().UTC().Add(time.Hour)
	plannedPost(t, s, "r", at, 2, "nostr")
	if _, err := s.sql.Exec(`UPDATE posts SET status='scheduled', scheduled_at=? WHERE id='r'`, at); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Hour)
	if err := s.RescheduleScheduled("r", later); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPost("r")
	for _, it := range p.Targets[0].Items {
		if it.NextAttemptAt == nil || !it.NextAttemptAt.Equal(later) {
			t.Errorf("item %d due %v, want %v", it.Ordinal, it.NextAttemptAt, later)
		}
	}
}

func TestSetItemRefNeedsTheClaim(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	p := plannedPost(t, s, "n", now, 1, "nostr")
	if err := s.SetItemRef(context.Background(), p.Targets[0].Items[0].ID, []byte(`{"id":"e"}`)); err != ErrClaimLost {
		t.Fatalf("ref set on an unclaimed item: %v", err)
	}
	c := claim(t, s, now, ClaimFilter{})
	if err := s.SetItemRef(context.Background(), c.ItemID, []byte(`{"id":"e"}`)); err != nil {
		t.Fatal(err)
	}
	// A failed attempt keeps the prepared ref.
	if _, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c, Status: ItemFailed}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPost("n")
	if string(got.Targets[0].Items[0].Ref) != `{"id":"e"}` {
		t.Errorf("ref = %s", got.Targets[0].Items[0].Ref)
	}
}

func TestFirstUncertainAttempt(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "u", now, 1, "mastodon")
	c := claim(t, s, now, ClaimFilter{})
	if at, err := s.FirstUncertainAttempt(context.Background(), c.ItemID); err != nil || at != nil {
		t.Fatalf("before any attempt: %v, %v", at, err)
	}
	when := now.Add(-time.Hour)
	if _, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c, Status: ItemFailed,
		ErrorClass: "unknown_outcome", NextAttemptAt: &now, At: when}); err != nil {
		t.Fatal(err)
	}
	at, err := s.FirstUncertainAttempt(context.Background(), c.ItemID)
	if err != nil || at == nil || !at.Equal(when) {
		t.Fatalf("first unknown = %v, %v; want %v", at, err, when)
	}
}

// A retry of a target with nothing left to send changes nothing.
func TestMarkDueLeavesFinishedTargets(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	p := plannedPost(t, s, "done", now, 1, "nostr")
	record(t, s, claim(t, s, now, ClaimFilter{}), ItemPartial)
	n, err := s.MarkDue(context.Background(), "done", []int64{p.Targets[0].ID}, now)
	if err != nil || n != 0 {
		t.Fatalf("mark due = %d, %v", n, err)
	}
	if got, _ := s.GetPost("done"); got.Status != "partial" || got.Targets[0].Status != "partial" {
		t.Errorf("finished post moved to %q / %q", got.Status, got.Targets[0].Status)
	}
}

// With automatic retries off, a failure waiting for one isn't claimed;
// first attempts and what the owner retried by hand are.
func TestClaimHoldsAutoRetriesButNotManualOnes(t *testing.T) {
	ctx := context.Background()
	s := openEngineStore(t)
	now := time.Now().UTC()
	p := plannedPost(t, s, "held", now, 1, "mastodon")
	c := claim(t, s, now, ClaimFilter{HoldAutoRetries: true})
	if c == nil {
		t.Fatal("a first attempt was held")
	}
	retry := now.Add(-time.Second)
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "transient", NextAttemptAt: &retry, At: now}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, now, ClaimFilter{HoldAutoRetries: true}); c != nil {
		t.Fatal("an automatic retry was claimed with retries off")
	}
	if n, err := s.MarkDue(ctx, "held", []int64{p.Targets[0].ID}, now); err != nil || n != 1 {
		t.Fatalf("mark due %d %v", n, err)
	}
	c = claim(t, s, now, ClaimFilter{HoldAutoRetries: true})
	if c == nil {
		t.Fatal("a retry by hand was held")
	}
	// The claim spends the mark: failing again, it waits like any other.
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "transient", NextAttemptAt: &retry, At: now}); err != nil {
		t.Fatal(err)
	}
	if c := claim(t, s, now, ClaimFilter{HoldAutoRetries: true}); c != nil {
		t.Fatal("the manual mark outlived its claim")
	}
	if c := claim(t, s, now, ClaimFilter{}); c == nil {
		t.Fatal("retries on again: the waiting failure wasn't claimed")
	}
}
