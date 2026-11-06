package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v2DB builds a database at schema v2 holding the four shapes v4 must carry
// over: a threaded Bluesky target (segments_json with a legacy CID), a
// single Mastodon post, a scheduled post, and a single Nostr post with a
// failed relay and a pending fan-out row.
func v2DB(t *testing.T, path string) {
	t.Helper()
	db, err := openWith(t, path, migrations[:2])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(time.Hour)
	mustExec(t, db, `INSERT INTO posts (id, created_at, master_text, platforms, source, status) VALUES
		('thread', ?, 'long', '["bluesky","mastodon"]', 'web', 'partial'),
		('nostr', ?, 'hi', '["nostr"]', 'web', 'partial')`, now, now)
	mustExec(t, db, `INSERT INTO posts (id, created_at, master_text, platforms, source, status, scheduled_at)
		VALUES ('sched', ?, 'later', '["threads"]', 'web', 'scheduled', ?)`, now, at)
	mustExec(t, db, `INSERT INTO media (post_id, ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt) VALUES
		('thread', 0, 'https://b/0', 'h0', 'image/png', '', '', 1, ''), ('thread', 1, 'https://b/1', 'h1', 'image/png', '', '', 1, '')`)

	mustExec(t, db, `INSERT INTO post_targets (id, post_id, platform, final_text, fields_json, status, remote_id, remote_url,
		latency_ms, attempt_count, last_attempt_at, segments_json, error_class) VALUES
		(1, 'thread', 'bluesky', 'long', '{"langs":["en"]}', 'partial', 'at://0', 'https://bsky/0', 120, 1, ?,
		 '[{"ordinal":0,"text":"one","remote_id":"at://0","remote_url":"https://bsky/0","cid":"cid0","status":"success","images":[0]},
		   {"ordinal":1,"text":"two","status":"failed","error":"boom","images":[1]}]', 'transient')`, now)
	mustExec(t, db, `INSERT INTO post_targets (id, post_id, platform, final_text, status, remote_id, remote_url, attempt_count, last_attempt_at)
		VALUES (2, 'thread', 'mastodon', 'long', 'success', '99', 'https://m/99', 1, ?)`, now)
	mustExec(t, db, `INSERT INTO post_targets (id, post_id, platform, final_text, status, segments_json)
		VALUES (3, 'sched', 'threads', 'later', 'scheduled',
		 '[{"ordinal":0,"text":"a","status":"pending"},{"ordinal":1,"text":"b","status":"pending"}]')`)
	mustExec(t, db, `INSERT INTO post_targets (id, post_id, platform, final_text, status, remote_id, remote_url,
		attempt_count, last_attempt_at, signed_event_json)
		VALUES (4, 'nostr', 'nostr', 'hi', 'partial', 'ev1', 'https://njump/ev1', 1, ?, '{"id":"ev1","sig":"s"}')`, now)
	mustExec(t, db, `INSERT INTO target_attempts (target_id, attempt_no, status, error, latency_ms, attempted_at) VALUES
		(1, 1, 'partial', 'boom', 120, ?), (2, 1, 'success', '', 80, ?), (4, 1, 'partial', '', 300, ?)`, now, now, now)
	mustExec(t, db, `INSERT INTO target_relays (target_id, relay_url, status, message, attempted_at, retry_count) VALUES
		(4, 'wss://a', 'ok', '', ?, 0), (4, 'wss://b', 'failed', 'timeout', ?, 2)`, now, now)
	mustExec(t, db, `INSERT INTO nostr_fanout (post_id, signed_event_json, relay_url, next_attempt_at, created_at)
		VALUES ('nostr', '{"id":"ev1","sig":"s"}', 'wss://c', ?, ?)`, now, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateTargetItemsCarriesEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	v2DB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Every type in history got an env account, and every target points at it.
	accts, err := s.Accounts(ctx, DefaultOwner)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]string{}
	for _, a := range accts {
		byType[a.Type] = a.ID
	}
	for _, typ := range []string{"bluesky", "mastodon", "threads", "nostr"} {
		if !strings.HasPrefix(byType[typ], "acc_") {
			t.Fatalf("no env account for %s: %+v", typ, accts)
		}
	}

	thread, err := s.GetPost("thread")
	if err != nil {
		t.Fatal(err)
	}
	bsky, masto := thread.Targets[0], thread.Targets[1]
	if bsky.AccountID != byType["bluesky"] || masto.AccountID != byType["mastodon"] {
		t.Fatalf("account ids: %q %q", bsky.AccountID, masto.AccountID)
	}
	// The thread's segments became items, and the /api view is unchanged.
	if len(bsky.Items) != 2 || len(bsky.Segments) != 2 {
		t.Fatalf("thread items %d segments %d", len(bsky.Items), len(bsky.Segments))
	}
	i0, i1 := bsky.Items[0], bsky.Items[1]
	if i0.Text != "one" || i0.Status != ItemPublished || i0.RemoteID != "at://0" || string(i0.Ref) != `{"cid":"cid0"}` ||
		len(i0.Media) != 1 || i0.Media[0] != 0 || i0.PublishedAt == nil || i0.IdemKey == "" {
		t.Errorf("item 0 = %+v", i0)
	}
	if i1.Text != "two" || i1.Status != ItemFailed || i1.Error != "boom" || i1.ErrorClass != "transient" || i1.Media[0] != 1 {
		t.Errorf("item 1 = %+v", i1)
	}
	if bsky.Segments[0].Status != "success" || bsky.Segments[1].Status != "failed" || bsky.RemoteURL != "https://bsky/0" {
		t.Errorf("segments view = %+v, remote %q", bsky.Segments, bsky.RemoteURL)
	}
	if bsky.FieldsJSON != `{"langs":["en"]}` || bsky.LatencyMS != 120 || bsky.ErrorClass != "transient" || len(bsky.Attempts) != 1 {
		t.Errorf("target fields = %+v", bsky)
	}
	// A single post is a one-item target carrying its own media.
	if len(masto.Items) != 1 || masto.Items[0].Status != ItemPublished || masto.RemoteID != "99" ||
		len(masto.Items[0].Media) != 2 || masto.Segments != nil {
		t.Errorf("single target = %+v items %+v", masto, masto.Items)
	}

	sched, _ := s.GetPost("sched")
	if st := sched.Targets[0]; st.Status != "scheduled" || len(st.Items) != 2 || st.Items[0].Status != ItemPlanned ||
		st.Segments[1].Status != "pending" {
		t.Errorf("scheduled target = %+v items %+v", st, st.Items)
	}
	// Planned items are due when the engine should claim them, and every
	// item keeps the key the dispatcher derived for it.
	for _, it := range sched.Targets[0].Items {
		if it.NextAttemptAt == nil || !it.NextAttemptAt.Equal(*sched.ScheduledAt) {
			t.Errorf("scheduled item %d due %v, want %v", it.Ordinal, it.NextAttemptAt, sched.ScheduledAt)
		}
		if it.IdemKey != LegacyItemKey("sched", "threads", it.Ordinal) {
			t.Errorf("scheduled item %d key %q", it.Ordinal, it.IdemKey)
		}
	}

	nostr, _ := s.GetPost("nostr")
	nt := nostr.Targets[0]
	if len(nt.Relays) != 2 || nt.Relays[1].URL != "wss://b" || nt.Relays[1].Status != "failed" || nt.Relays[1].RetryCount != 2 {
		t.Errorf("relays = %+v", nt.Relays)
	}
	if nt.SignedEventJSON != `{"id":"ev1","sig":"s"}` || nt.Items[0].Status != ItemPartial {
		t.Errorf("nostr target = %+v", nt)
	}
	// Attempts of single-item targets and fan-out rows point at their item.
	var attemptItem, fanoutItem int64
	if err := s.sql.QueryRow(`SELECT item_id FROM target_attempts WHERE target_id = 4`).Scan(&attemptItem); err != nil {
		t.Fatal(err)
	}
	if err := s.sql.QueryRow(`SELECT item_id FROM nostr_fanout`).Scan(&fanoutItem); err != nil {
		t.Fatal(err)
	}
	if attemptItem != nt.Items[0].ID || fanoutItem != nt.Items[0].ID {
		t.Errorf("attempt item %d, fan-out item %d, want %d", attemptItem, fanoutItem, nt.Items[0].ID)
	}
	var threadAttemptItem *int64
	_ = s.sql.QueryRow(`SELECT item_id FROM target_attempts WHERE target_id = 1`).Scan(&threadAttemptItem)
	if threadAttemptItem != nil {
		t.Errorf("a thread's attempt spans items; item_id = %d, want NULL", *threadAttemptItem)
	}

	// The relay retry and resume paths work on the migrated rows.
	if err := s.UpdateRelayStatus(4, "wss://b", "ok", ""); err != nil {
		t.Fatal(err)
	}
	nostr, _ = s.GetPost("nostr")
	if nostr.Status != "success" || nostr.Targets[0].Status != "success" || nostr.Targets[0].Items[0].Status != ItemPublished {
		t.Errorf("after relay retry: post %q target %q item %q", nostr.Status, nostr.Targets[0].Status, nostr.Targets[0].Items[0].Status)
	}
	// The engine resumes the migrated thread at its failed part.
	if n, err := s.MarkDue(ctx, "thread", []int64{1}, time.Now()); err != nil || n != 1 {
		t.Fatalf("mark due = %d, %v", n, err)
	}
	c, err := s.ClaimItem(ctx, time.Now(), ClaimFilter{TargetID: 1})
	if err != nil || c == nil || c.Ordinal != 1 || c.Attempts != 0 {
		t.Fatalf("claim = %+v, %v", c, err)
	}
	if _, err := s.RecordItemResult(ctx, ItemResult{Claim: *c, Status: ItemPublished, RemoteID: "at://1", RemoteURL: "https://bsky/1"}); err != nil {
		t.Fatal(err)
	}
	thread, _ = s.GetPost("thread")
	if got := thread.Targets[0]; got.Status != "success" || got.Items[1].Status != ItemPublished ||
		got.Items[1].AttemptCount != 1 || got.Items[0].AttemptCount != 0 || thread.Status != "success" {
		t.Errorf("after resume: post %q target %+v items %+v", thread.Status, got, got.Items)
	}
}

// Startup seeding creates a row per configured account, keeps ids, and
// never changes an existing row (enabled is the owner's; whether an env
// account is configured is the registry's call). Rows added in the app
// are left alone.
func TestSeedEnvAccounts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	byKey := func(rows []Account) map[string]Account {
		out := map[string]Account{}
		for _, a := range rows {
			out[a.Type+"/"+a.EnvKey] = a
		}
		return out
	}
	first, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "bluesky", Label: "Bluesky"},
		{Type: "nostr", Key: EnvKeyDefault, Label: "Nostr"}})
	if err != nil {
		t.Fatal(err)
	}
	f := byKey(first)
	if len(first) != 2 || f["bluesky/default"].Label != "Bluesky" || !f["bluesky/default"].Enabled {
		t.Fatalf("first seed = %+v", first)
	}
	// The owner turned Bluesky off and renamed Nostr; the environment
	// dropped Bluesky and added a second Nostr account.
	off, label := false, "Mine"
	if _, err := s.UpdateAccountMeta(ctx, DefaultOwner, f["bluesky/default"].ID, AccountMeta{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccountMeta(ctx, DefaultOwner, f["nostr/default"].ID, AccountMeta{Label: &label}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAccount(ctx, Account{ID: "acc_db", Type: "nostr", Label: "Added", Source: "db", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	second, err := s.SeedEnvAccounts(ctx, DefaultOwner, []EnvAccount{{Type: "nostr", Label: "Nostr"},
		{Type: "nostr", Key: "2", Label: "Nostr"}})
	if err != nil {
		t.Fatal(err)
	}
	g := byKey(second)
	if len(second) != 3 {
		t.Fatalf("second seed = %+v; want every env row, and not the app's", second)
	}
	if g["nostr/default"].ID != f["nostr/default"].ID || g["nostr/default"].Label != "Mine" {
		t.Errorf("nostr = %+v; the id must be stable and the owner's label kept", g["nostr/default"])
	}
	if b := g["bluesky/default"]; b.ID != f["bluesky/default"].ID || b.Enabled {
		t.Errorf("bluesky = %+v; an unconfigured account keeps its row and the owner's enabled", b)
	}
	if n := g["nostr/2"]; n.ID == "" || n.ID == g["nostr/default"].ID || !n.Enabled {
		t.Errorf("second nostr = %+v", n)
	}
	if a, err := s.Account(ctx, DefaultOwner, "acc_db"); err != nil || a.Label != "Added" || a.Source != "db" {
		t.Errorf("app account = %+v, %v", a, err)
	}
}
