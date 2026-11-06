package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The oldest schema (baseline, no later columns) with the rows a real
// install would hold, migrated to the latest version, then read by every
// reader the server runs at start and on the main pages.
func TestLegacyRealisticUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db := rawDB(t, path)
	if _, err := db.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	// posts: a success, a failed, a partial nostr, a "scheduled" (no scheduled_at column yet)
	mustExec(t, db, `INSERT INTO posts (id, created_at, master_text, platforms, delay_seconds, source, status) VALUES
		('ok', ?, 'hello', '["mastodon","bluesky"]', 0, 'ui', 'success'),
		('bad', ?, 'nope', '["bluesky"]', 0, 'api', 'failed'),
		('nos', ?, 'gm', '["nostr"]', 0, 'ui', 'partial'),
		('sch', ?, 'later', '["mastodon"]', 0, 'ui', 'scheduled')`, now, now.Add(time.Second), now.Add(2*time.Second), now.Add(3*time.Second))
	mustExec(t, db, `INSERT INTO post_targets (post_id, platform, final_text, fields_json, status, remote_id, remote_url, latency_ms, attempt_count, last_attempt_at) VALUES
		('ok', 'mastodon', 'hello', '{"visibility":"public"}', 'success', '1', 'https://m/1', 80, 1, ?),
		('ok', 'bluesky', 'hello', NULL, 'success', 'at://x', 'https://bsky/x', 90, 1, ?),
		('bad', 'bluesky', 'nope', NULL, 'failed', NULL, NULL, NULL, 3, ?),
		('nos', 'nostr', 'gm', NULL, 'partial', 'ev', 'https://njump/ev', 300, 1, ?),
		('sch', 'mastodon', 'later', NULL, 'scheduled', NULL, NULL, NULL, 0, NULL)`, now, now, now, now)
	mustExec(t, db, `INSERT INTO target_attempts (target_id, attempt_no, status, error, latency_ms, remote_id, attempted_at) VALUES
		(1, 1, 'success', NULL, 80, '1', ?), (2, 1, 'success', NULL, 90, 'at://x', ?),
		(3, 1, 'failed', 'boom', 0, '', ?), (3, 2, 'failed', 'boom', 0, '', ?), (3, 3, 'failed', 'boom', 0, '', ?),
		(4, 1, 'partial', NULL, 300, 'ev', ?)`, now, now, now, now, now, now)
	mustExec(t, db, `INSERT INTO media (post_id, ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt) VALUES
		('ok', 0, 'https://b/h0', 'h0', 'image/jpeg', '10x10', '', 100, '')`)
	mustExec(t, db, `INSERT INTO target_relays (target_id, relay_url, status, message, attempted_at) VALUES
		(4, 'wss://a', 'ok', NULL, ?), (4, 'wss://b', 'failed', 'timeout', ?)`, now, now)
	mustExec(t, db, `INSERT INTO nostr_fanout (post_id, signed_event_json, relay_url, next_attempt_at, created_at) VALUES
		(NULL, '{"id":"ev"}', 'wss://c', ?, ?)`, now, now)
	mustExec(t, db, `INSERT INTO drafts (id, created_at, updated_at, title, master_text, tags_json, spec_json) VALUES
		('d1', ?, ?, 'T', 'draft text', '["a"]', '{"master_text":"draft text","platforms":["mastodon"]}')`, now, now)
	mustExec(t, db, `INSERT INTO draft_media (draft_id, ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt) VALUES
		('d1', 0, 'https://b/h0', 'h0', 'image/jpeg', NULL, NULL, NULL, NULL)`)
	mustExec(t, db, `INSERT INTO users (id, subject, email, name, created_at) VALUES ('u1', 'sub', NULL, NULL, ?)`, now)
	mustExec(t, db, `INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES ('s1', 'u1', ?, ?)`, now, now.Add(time.Hour))
	mustExec(t, db, `INSERT INTO api_tokens (id, name, token_hash, created_at) VALUES ('t1', 'ci', 'hash', ?)`, now)
	mustExec(t, db, `INSERT INTO sync_relays (url, added_at) VALUES ('wss://c', ?)`, now)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{"ok", "bad", "nos", "sch"} {
		if _, err := s.GetPost(id); err != nil {
			t.Errorf("GetPost(%s): %v", id, err)
		}
	}
	if ps, err := s.ListPostsPage(ctx, PostQuery{OwnerID: DefaultOwner}); err != nil || len(ps) != 4 {
		t.Errorf("ListPostsPage: %d %v", len(ps), err)
	}
	if ps, err := s.PublicFeed(20); err != nil || len(ps) != 2 {
		t.Errorf("PublicFeed: %d %v", len(ps), err)
	} else if ps[0].FirstSuccessAt == nil {
		t.Errorf("PublicFeed: no first success time for %s", ps[0].ID)
	}
	if _, err := s.ListPosts(50, 0); err != nil {
		t.Errorf("ListPosts: %v", err)
	}
	if _, err := s.AttentionCount(); err != nil {
		t.Errorf("AttentionCount: %v", err)
	}
	if _, err := s.PostsNeedingRetry(); err != nil {
		t.Errorf("PostsNeedingRetry: %v", err)
	}
	if _, err := s.UnscheduledFailures(ctx); err != nil {
		t.Errorf("UnscheduledFailures: %v", err)
	}
	if _, err := s.MarkOverdueMissed(ctx, now.Add(time.Hour)); err != nil {
		t.Errorf("MarkOverdueMissed: %v", err)
	}
	olds, err := s.OldDrafts(ctx)
	if err != nil || len(olds) != 1 {
		t.Errorf("OldDrafts: %d %v", len(olds), err)
	}
	if _, err := s.SendingItems(ctx); err != nil {
		t.Errorf("SendingItems: %v", err)
	}
	if c, err := s.ClaimItem(ctx, now.Add(time.Hour), ClaimFilter{}); err != nil {
		t.Errorf("ClaimItem: %v", err)
	} else if c != nil {
		t.Logf("claimed legacy item: post %s target %d ordinal %d", c.PostID, c.TargetID, c.Ordinal)
	}
}
