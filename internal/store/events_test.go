package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

func postEvents(t *testing.T, s *Store, postID string, after int64) []Event {
	t.Helper()
	ev, err := s.EventsSince(context.Background(), EventFilter{OwnerID: DefaultOwner, PostID: postID, After: after})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func eventTypes(ev []Event) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Type)
	}
	return out
}

func wantTypes(t *testing.T, ev []Event, want ...string) {
	t.Helper()
	if got := eventTypes(ev); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func lastID(ev []Event) int64 {
	if len(ev) == 0 {
		return 0
	}
	return ev[len(ev)-1].ID
}

// scheduledPost saves a scheduled post: one target per platform, each with
// n planned items due at at.
func scheduledPost(t *testing.T, s *Store, id string, at time.Time, n int, platforms ...string) *Post {
	t.Helper()
	p := &Post{ID: id, CreatedAt: time.Now().UTC(), Platforms: platforms, Source: "test", Status: "scheduled", ScheduledAt: &at}
	for _, pl := range platforms {
		tg := Target{Platform: pl, Status: "scheduled"}
		for i := 0; i < n; i++ {
			d := at
			tg.Items = append(tg.Items, Item{Ordinal: i, Text: pl, Status: ItemPlanned, IdemKey: LegacyItemKey(id, pl, i), NextAttemptAt: &d})
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

// A post's life is logged in order: created, each item, and the post
// settling.
func TestEventsFollowAPost(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 2, "bluesky")
	ev := postEvents(t, s, "p", 0)
	wantTypes(t, ev, EventPostCreated)
	if ev[0].Data.Status != "sending" || ev[0].OwnerID != DefaultOwner || ev[0].PostID != "p" {
		t.Fatalf("created = %+v", ev[0])
	}

	changed := s.EventsChanged()
	c := claim(t, s, now, ClaimFilter{})
	select {
	case <-changed:
	default:
		t.Fatal("a claim didn't wake EventsChanged")
	}
	record(t, s, c, ItemPublished)
	c = claim(t, s, now, ClaimFilter{})
	record(t, s, c, ItemPublished)
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventItemSending, EventItemPublished, EventItemSending, EventItemPublished, EventPostUpdated, EventPostCompleted)
	if o := ev[3].Data.Ordinal; o == nil || *o != 1 {
		t.Fatalf("ordinal = %v, want 1", o)
	}
	if ev[3].Data.AccountID == "" || ev[3].Data.Platform != "bluesky" || ev[3].Data.TargetID == 0 {
		t.Fatalf("item event = %+v", ev[3].Data)
	}
	if ev[4].Data.Status != "success" || ev[5].Data.Status != "success" {
		t.Fatalf("settled = %+v / %+v", ev[4].Data, ev[5].Data)
	}
}

// A failure logs the error, scrubbed, and a give-up settles the post.
func TestEventsLogFailures(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	if _, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c, Status: ItemFailed,
		ErrorClass: "permanent", Error: "bad Bearer abc.def.ghi", GiveUp: true}); err != nil {
		t.Fatal(err)
	}
	ev := postEvents(t, s, "p", 0)
	wantTypes(t, ev, EventPostCreated, EventItemSending, EventItemFailed, EventPostUpdated, EventPostCompleted)
	if d := ev[2].Data; d.ErrorClass != "permanent" || d.Error != Scrub("bad Bearer abc.def.ghi") {
		t.Fatalf("failed = %+v", d)
	}
}

// A claim released unsent logs nothing about the item.
func TestEventsSkipNotSent(t *testing.T) {
	s := openEngineStore(t)
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	after := lastID(postEvents(t, s, "p", 0))
	if _, err := s.RecordItemResult(context.Background(), ItemResult{Claim: *c, Status: ItemFailed, NotSent: true}); err != nil {
		t.Fatal(err)
	}
	for _, e := range postEvents(t, s, "p", after) {
		if e.Type == EventItemFailed {
			t.Fatalf("a claim released unsent was logged: %+v", e)
		}
	}
}

func TestEventsForScheduleHideAndMissed(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	scheduledPost(t, s, "p", at, 1, "bluesky")
	ev := postEvents(t, s, "p", 0)
	wantTypes(t, ev, EventPostCreated)
	if ev[0].Data.ScheduledAt == nil || !ev[0].Data.ScheduledAt.Equal(at) {
		t.Fatalf("created scheduled_at = %v", ev[0].Data.ScheduledAt)
	}

	later := at.Add(time.Hour)
	if err := s.RescheduleScheduled("p", later); err != nil {
		t.Fatal(err)
	}
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventPostUpdated)
	if ev[0].Data.ScheduledAt == nil || !ev[0].Data.ScheduledAt.Equal(later) {
		t.Fatalf("rescheduled to %v", ev[0].Data.ScheduledAt)
	}

	if err := s.SetPostHidden("p", true); !errors.Is(err, ErrNotHideable) {
		t.Fatalf("hiding a scheduled post: %v", err)
	}
	if _, err := s.MarkOverdueMissed(ctx, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventPostUpdated, EventPostCompleted)
	if ev[0].Data.Status != "missed" {
		t.Fatalf("missed = %+v", ev[0].Data)
	}

	if err := s.SetPostHidden("p", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPostHidden("p", true); err != nil { // already hidden: no-op
		t.Fatal(err)
	}
	if err := s.SetPostHidden("p", false); err != nil {
		t.Fatal(err)
	}
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventPostUpdated, EventPostUpdated)
	if h := ev[0].Data.Hidden; h == nil || !*h {
		t.Fatalf("hide event = %+v", ev[0].Data)
	}
	if h := ev[1].Data.Hidden; h == nil || *h {
		t.Fatalf("show event = %+v", ev[1].Data)
	}
	if err := s.SetPostHidden("nope", true); !errors.Is(err, ErrNotHideable) {
		t.Fatalf("hiding a missing post: %v", err)
	}
}

func TestCancelScheduledPost(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	scheduledPost(t, s, "p", time.Now().Add(time.Hour), 2, "bluesky", "mastodon")
	after := lastID(postEvents(t, s, "p", 0))
	if err := s.CancelPost(ctx, "p", time.Now()); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPost("p")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "canceled" {
		t.Fatalf("post status = %s", p.Status)
	}
	for _, tg := range p.Targets {
		if tg.Status != "canceled" || tg.GaveUpAt != nil {
			t.Fatalf("target = %s gave_up %v", tg.Status, tg.GaveUpAt)
		}
		for _, it := range tg.Items {
			if it.Status != ItemCanceled || it.NextAttemptAt != nil {
				t.Fatalf("item = %+v", it)
			}
		}
	}
	wantTypes(t, postEvents(t, s, "p", after), EventItemCanceled, EventItemCanceled, EventItemCanceled, EventItemCanceled,
		EventPostUpdated, EventPostCompleted)
	if c := claim(t, s, time.Now().Add(2*time.Hour), ClaimFilter{}); c != nil {
		t.Fatalf("a canceled item was claimed: %+v", c)
	}
	if err := s.CancelPost(ctx, "p", time.Now()); !errors.Is(err, ErrNothingToCancel) {
		t.Fatalf("second cancel: %v", err)
	}
	if err := s.CancelPost(ctx, "nope", time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cancel of a missing post: %v", err)
	}
}

// Canceling a thread mid-send lets the item in flight finish and stops
// the rest.
func TestCancelThreadWhileSending(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 3, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.PostStatus(ctx, "p"); st != "sending" {
		t.Fatalf("post with an item in flight = %s, want sending", st)
	}
	out := record(t, s, c, ItemPublished)
	if out.TargetStatus != "partial" || out.PostAfter != "partial" {
		t.Fatalf("after the in-flight item: %+v", out)
	}
	if c := claim(t, s, now.Add(time.Hour), ClaimFilter{}); c != nil {
		t.Fatalf("the rest of a canceled thread was claimed: %+v", c)
	}
}

// Canceling a thread stopped at a failure cancels the failure's retry and
// the rest; the target is given up so the retrier leaves it.
func TestCancelThreadAfterFailure(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 3, "bluesky")
	record(t, s, claim(t, s, now, ClaimFilter{}), ItemPublished)
	c := claim(t, s, now, ClaimFilter{})
	retry := now.Add(time.Minute)
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "transient", NextAttemptAt: &retry}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPost("p")
	tg := p.Targets[0]
	got := []string{tg.Items[0].Status, tg.Items[1].Status, tg.Items[2].Status}
	if !slices.Equal(got, []string{ItemPublished, ItemCanceled, ItemCanceled}) {
		t.Fatalf("items = %v", got)
	}
	if p.Status != "partial" || tg.Status != "partial" || tg.GaveUpAt == nil || p.Attention {
		t.Fatalf("post %s attention %v, target %s gave_up %v", p.Status, p.Attention, tg.Status, tg.GaveUpAt)
	}
	ids, _ := s.PostsNeedingRetry()
	if slices.Contains(ids, "p") {
		t.Fatal("the retrier would still look at a canceled post")
	}
	if c := claim(t, s, now.Add(time.Hour), ClaimFilter{}); c != nil {
		t.Fatalf("claimed after cancel: %+v", c)
	}
}

// A post that failed and was given up keeps its failure: there is nothing
// to cancel.
func TestCancelKeepsAGivenUpFailure(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "permanent", GiveUp: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPost(ctx, "p", now); !errors.Is(err, ErrNothingToCancel) {
		t.Fatalf("cancel: %v", err)
	}
	if st, _ := s.PostStatus(ctx, "p"); st != "failed" {
		t.Fatalf("status = %s", st)
	}
}

// An unknown item keeps its status (it may be live) but stops being
// retried, and the post still needs attention.
func TestCancelStopsRetryingAnUnknownItem(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	retry := now.Add(time.Minute)
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemUnknown, NextAttemptAt: &retry}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPost("p")
	it := p.Targets[0].Items[0]
	if it.Status != ItemUnknown || it.NextAttemptAt != nil || p.Targets[0].GaveUpAt == nil || !p.Attention {
		t.Fatalf("item %+v, target gave_up %v, attention %v", it, p.Targets[0].GaveUpAt, p.Attention)
	}
	us, _ := s.UnscheduledFailures(ctx)
	if len(us) != 0 {
		t.Fatalf("the engine would reschedule %+v", us)
	}
	if err := s.CancelPost(ctx, "p", now); !errors.Is(err, ErrNothingToCancel) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestEventsSinceFiltersAndPurge(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "a", now, 1, "bluesky")
	plannedPost(t, s, "b", now, 1, "bluesky")
	all, err := s.EventsSince(ctx, EventFilter{OwnerID: DefaultOwner})
	if err != nil || len(all) != 2 || all[0].PostID != "a" || all[1].PostID != "b" {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if last, _ := s.LastEventID(ctx); last != all[1].ID {
		t.Fatalf("LastEventID = %d, want %d", last, all[1].ID)
	}
	if ev, _ := s.EventsSince(ctx, EventFilter{OwnerID: DefaultOwner, After: all[0].ID}); len(ev) != 1 || ev[0].PostID != "b" {
		t.Fatalf("after = %+v", ev)
	}
	if ev, _ := s.EventsSince(ctx, EventFilter{OwnerID: DefaultOwner, Limit: 1}); len(ev) != 1 {
		t.Fatalf("limit = %+v", ev)
	}
	if ev, _ := s.EventsSince(ctx, EventFilter{OwnerID: "someone-else"}); len(ev) != 0 {
		t.Fatalf("another owner's = %+v", ev)
	}

	if n, err := s.PurgeEvents(ctx, now.Add(EventRetention-time.Minute)); err != nil || n != 0 {
		t.Fatalf("purge of fresh events = %d, %v", n, err)
	}
	if n, err := s.PurgeEvents(ctx, now.Add(EventRetention+time.Minute)); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if last, _ := s.LastEventID(ctx); last != 0 {
		t.Fatalf("LastEventID after purge = %d", last)
	}
}

func TestListPostsPage(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	save := func(id, status, text string, at time.Time, hidden bool, platforms ...string) {
		t.Helper()
		p := &Post{ID: id, CreatedAt: at, MasterText: text, Platforms: platforms, Source: "test", Status: status}
		for _, pl := range platforms {
			p.Targets = append(p.Targets, Target{Platform: pl, Status: status})
		}
		if err := saveWithAccounts(s, p); err != nil {
			t.Fatal(err)
		}
		if hidden {
			if err := s.SetPostHidden(id, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	save("p1", "success", "hello world", base, false, "bluesky")
	save("p2", "failed", "50% off", base.Add(time.Hour), false, "mastodon")
	save("p3", "success", "same time a", base.Add(2*time.Hour), false, "bluesky", "mastodon")
	save("p4", "success", "same time b", base.Add(2*time.Hour), false, "bluesky")
	save("p5", "success", "hidden one", base.Add(3*time.Hour), true, "bluesky")
	// p2's target gave up with a failed item: it needs attention.
	if _, err := s.sql.Exec(`UPDATE post_targets SET gave_up_at = ? WHERE post_id = 'p2'`, base); err != nil {
		t.Fatal(err)
	}
	list := func(q PostQuery) []string {
		t.Helper()
		if q.OwnerID == "" {
			q.OwnerID = DefaultOwner
		}
		ps, err := s.ListPostsPage(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		return ids
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("all", list(PostQuery{}), "p4", "p3", "p2", "p1")
	check("hidden", list(PostQuery{Hidden: ptr(true)}), "p5")
	check("published", list(PostQuery{Statuses: []string{"success"}}), "p4", "p3", "p1")
	check("failed (needs attention instead)", list(PostQuery{Statuses: []string{"failed"}}))
	check("attention", list(PostQuery{Attention: true}), "p2")
	check("attention or published", list(PostQuery{Attention: true, Statuses: []string{"success"}}), "p4", "p3", "p2", "p1")
	check("text with %", list(PostQuery{Text: "50%"}), "p2")
	check("text", list(PostQuery{Text: "same time"}), "p4", "p3")
	check("since", list(PostQuery{Since: base.Add(time.Hour)}), "p4", "p3", "p2")
	check("until", list(PostQuery{Until: base.Add(time.Hour)}), "p1")
	check("limit", list(PostQuery{Limit: 2}), "p4", "p3")
	check("after p4 (same time)", list(PostQuery{AfterTime: base.Add(2 * time.Hour), AfterID: "p4"}), "p3", "p2", "p1")
	check("after p3", list(PostQuery{AfterTime: base.Add(2 * time.Hour), AfterID: "p3"}), "p2", "p1")
	check("other owner", list(PostQuery{OwnerID: "x"}))

	ps, _ := s.ListPostsPage(ctx, PostQuery{OwnerID: DefaultOwner, Text: "same time a"})
	if len(ps) != 1 || len(ps[0].Targets) != 2 || ps[0].Targets[0].AccountID == "" || ps[0].Targets[0].Platform != "bluesky" {
		t.Fatalf("targets = %+v", ps)
	}
	check("account", list(PostQuery{AccountID: ps[0].Targets[1].AccountID}), "p3", "p2")
}

func ptr[T any](v T) *T { return &v }

// A relay retry logs the delivery, and the post when it changes.
func TestEventsForRelayRetry(t *testing.T) {
	s := openEngineStore(t)
	if err := saveWithAccounts(s, &Post{
		ID: "p", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []Target{{Platform: "nostr", Status: "partial", FinalText: "hi",
			Relays:   []RelayState{{URL: "wss://a", Status: "ok"}, {URL: "wss://b", Status: "failed", Message: "503"}},
			Attempts: []Attempt{{AttemptNo: 1, Status: "partial", AttemptedAt: time.Now()}}}},
	}); err != nil {
		t.Fatal(err)
	}
	ev := postEvents(t, s, "p", 0)
	wantTypes(t, ev, EventPostCreated, EventPostCompleted)
	p, _ := s.GetPost("p")
	if err := s.UpdateRelayStatus(p.Targets[0].ID, "wss://b", "failed", "still 503"); err != nil {
		t.Fatal(err)
	}
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventDelivery)
	if d := ev[0].Data; d.Endpoint != "wss://b" || d.Status != "failed" || d.Error != "still 503" {
		t.Fatalf("delivery = %+v", d)
	}
	if err := s.UpdateRelayStatus(p.Targets[0].ID, "wss://b", "ok", ""); err != nil {
		t.Fatal(err)
	}
	ev = postEvents(t, s, "p", lastID(ev))
	wantTypes(t, ev, EventDelivery, EventPostUpdated, EventPostCompleted)
	if ev[1].Data.Status != "success" {
		t.Fatalf("post = %+v", ev[1].Data)
	}
}

// ─── review regressions ─────────────────────────────────────────────────

// An item in flight at a cancel isn't retried when it fails, on its own
// thread or on another target of the post.
func TestCancelSticksForAnItemInFlight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		n         int
		platforms []string
	}{{"thread", 2, []string{"bluesky"}}, {"two targets", 1, []string{"bluesky", "mastodon"}}} {
		t.Run(tc.name, func(t *testing.T) {
			s := openEngineStore(t)
			ctx := context.Background()
			now := time.Now().UTC()
			plannedPost(t, s, "p", now, tc.n, tc.platforms...)
			c := claim(t, s, now, ClaimFilter{})
			if err := s.CancelPost(ctx, "p", now); err != nil {
				t.Fatal(err)
			}
			retry := now.Add(time.Minute)
			out, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "transient", NextAttemptAt: &retry})
			if err != nil {
				t.Fatal(err)
			}
			if out.GaveUp {
				t.Fatal("a cancel reported as an engine give-up (it would alert)")
			}
			if c := claim(t, s, now.Add(time.Hour), ClaimFilter{}); c != nil {
				t.Fatalf("a canceled post's item was claimed again: %+v", c)
			}
			_, settled, _ := s.PostState(ctx, "p")
			if !settled {
				t.Fatal("canceled post not settled")
			}
			ev := postEvents(t, s, "p", 0)
			if ev[len(ev)-1].Type != EventPostCompleted {
				t.Fatalf("events = %v", eventTypes(ev))
			}
		})
	}
}

// Canceling a thread while a later item is in flight leaves it sending
// until that item ends.
func TestCancelMidThreadWithALaterItemInFlight(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 3, "bluesky")
	record(t, s, claim(t, s, now, ClaimFilter{}), ItemPublished)
	c := claim(t, s, now, ClaimFilter{})
	after := lastID(postEvents(t, s, "p", 0))
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPost("p")
	if p.Status != "sending" || p.Targets[0].Status != "sending" {
		t.Fatalf("post %s target %s, want sending", p.Status, p.Targets[0].Status)
	}
	if err := s.SetPostHidden("p", true); !errors.Is(err, ErrNotHideable) {
		t.Fatalf("hid a post still sending: %v", err)
	}
	wantTypes(t, postEvents(t, s, "p", after), EventItemCanceled)
	record(t, s, c, ItemPublished)
	ev := postEvents(t, s, "p", after)
	wantTypes(t, ev, EventItemCanceled, EventItemPublished, EventPostUpdated, EventPostCompleted)
	if ev[2].Data.Status != "partial" {
		t.Fatalf("settled as %s", ev[2].Data.Status)
	}
}

// A failure the engine will retry doesn't complete the post; the retry's
// outcome does, once.
func TestEventsCompleteOnlyWhenNothingIsLeft(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	retry := now.Add(time.Minute)
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "transient", NextAttemptAt: &retry}); err != nil {
		t.Fatal(err)
	}
	if _, settled, _ := s.PostState(ctx, "p"); settled {
		t.Fatal("a post with a retry due is settled")
	}
	record(t, s, claim(t, s, retry, ClaimFilter{}), ItemPublished)
	wantTypes(t, postEvents(t, s, "p", 0), EventPostCreated, EventItemSending, EventItemFailed, EventPostUpdated,
		EventPostUpdated, EventItemSending, EventItemPublished, EventPostUpdated, EventPostCompleted)
}

// Canceling a failed post whose unknown item was still retrying completes
// it once, without repeating its status.
func TestCancelOfARetryingFailureCompletesOnce(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	plannedPost(t, s, "p", now, 1, "bluesky")
	c := claim(t, s, now, ClaimFilter{})
	retry := now.Add(time.Minute)
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemUnknown, NextAttemptAt: &retry}); err != nil {
		t.Fatal(err)
	}
	after := lastID(postEvents(t, s, "p", 0))
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	wantTypes(t, postEvents(t, s, "p", after), EventPostCompleted)
}

func TestMarkDueAfterCancelSendsTheRest(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p := plannedPost(t, s, "p", now, 3, "bluesky")
	record(t, s, claim(t, s, now, ClaimFilter{}), ItemPublished)
	if err := s.CancelPost(ctx, "p", now); err != nil {
		t.Fatal(err)
	}
	after := lastID(postEvents(t, s, "p", 0))
	if n, err := s.MarkDue(ctx, "p", []int64{p.Targets[0].ID}, now); err != nil || n != 1 {
		t.Fatalf("MarkDue = %d, %v", n, err)
	}
	wantTypes(t, postEvents(t, s, "p", after), EventPostUpdated)
	for i := 1; i <= 2; i++ {
		c := claim(t, s, now, ClaimFilter{})
		if c == nil || c.Ordinal != i {
			t.Fatalf("claim %d = %+v", i, c)
		}
		record(t, s, c, ItemPublished)
	}
	if st, _ := s.PostStatus(ctx, "p"); st != "success" {
		t.Fatalf("status = %s", st)
	}
	// Marking an already sending post due again says nothing new.
	q := plannedPost(t, s, "q", now.Add(time.Hour), 1, "bluesky")
	after = lastID(postEvents(t, s, "q", 0))
	if _, err := s.MarkDue(ctx, "q", []int64{q.Targets[0].ID}, now); err != nil {
		t.Fatal(err)
	}
	wantTypes(t, postEvents(t, s, "q", after))
}

func TestResetUnstartedLogsTheSchedule(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Minute).UTC()
	scheduledPost(t, s, "p", at, 1, "bluesky")
	c := claim(t, s, time.Now(), ClaimFilter{})
	if c == nil {
		t.Fatal("no claim")
	}
	// A stop before the send: the item goes back to planned.
	if _, err := s.sql.Exec(`UPDATE target_items SET status = 'planned'`); err != nil {
		t.Fatal(err)
	}
	after := lastID(postEvents(t, s, "p", 0))
	if n, err := s.ResetUnstarted(ctx); err != nil || n != 1 {
		t.Fatalf("ResetUnstarted = %d, %v", n, err)
	}
	ev := postEvents(t, s, "p", after)
	wantTypes(t, ev, EventPostUpdated)
	if ev[0].Data.Status != "scheduled" {
		t.Fatalf("status = %s", ev[0].Data.Status)
	}
}

// A missed post retried on one target that fails again is failed: nothing
// went out.
func TestMissedThenFailedIsFailed(t *testing.T) {
	s := openEngineStore(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).UTC()
	p := scheduledPost(t, s, "p", at, 1, "bluesky", "mastodon")
	if _, err := s.MarkOverdueMissed(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDue(ctx, "p", []int64{p.Targets[0].ID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	c := claim(t, s, time.Now(), ClaimFilter{})
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemFailed, ErrorClass: "permanent", GiveUp: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.PostStatus(ctx, "p"); st != "failed" {
		t.Fatalf("status = %s, want failed", st)
	}
}

func TestDeliveryMessagesAreScrubbed(t *testing.T) {
	s := openEngineStore(t)
	secret := "bad Bearer abc.def.ghi"
	if err := saveWithAccounts(s, &Post{
		ID: "p", CreatedAt: time.Now().UTC(), Platforms: []string{"nostr"}, Source: "web", Status: "partial",
		Targets: []Target{{Platform: "nostr", Status: "partial", FinalText: "hi",
			Relays: []RelayState{{URL: "wss://a", Status: "ok"}, {URL: "wss://b", Status: "failed", Message: secret}}}},
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPost("p")
	if err := s.UpdateRelayStatus(p.Targets[0].ID, "wss://a", "failed", secret); err != nil {
		t.Fatal(err)
	}
	p, _ = s.GetPost("p")
	for _, d := range p.Targets[0].Items[0].Deliveries {
		if d.Message != Scrub(secret) || d.Message == secret {
			t.Fatalf("delivery message = %q", d.Message)
		}
	}
}

func TestEmitNeedsThePost(t *testing.T) {
	s := openEngineStore(t)
	if err := emit(context.Background(), s.sql, "nope", EventPostUpdated, EventData{}, time.Now()); err == nil {
		t.Fatal("an event was logged for a missing post (it would go to the default owner)")
	}
}
