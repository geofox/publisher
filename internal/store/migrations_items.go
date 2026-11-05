package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// DefaultOwner is the one owner every row belongs to until there are more.
const DefaultOwner = "own_default"

// migrateOwnersAccounts (v3) adds owners and destination accounts, and an
// owner id on the tables that hold an owner's data. There is one owner; the
// column exists so data never has to be re-homed when there are more.
func migrateOwnersAccounts(ctx context.Context, tx execer) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE owners (
  id         TEXT PRIMARY KEY,
  created_at TIMESTAMP NOT NULL
);
INSERT INTO owners (id, created_at) VALUES ('`+DefaultOwner+`', CURRENT_TIMESTAMP);
CREATE TABLE accounts (
  id          TEXT PRIMARY KEY,
  owner_id    TEXT NOT NULL REFERENCES owners(id),
  type        TEXT NOT NULL,
  label       TEXT NOT NULL,
  handle      TEXT,
  source      TEXT NOT NULL,
  env_key     TEXT,
  config_json TEXT NOT NULL DEFAULT '{}',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  UNIQUE(owner_id, type, env_key)
);
ALTER TABLE posts ADD COLUMN owner_id TEXT NOT NULL DEFAULT '`+DefaultOwner+`' REFERENCES owners(id);
ALTER TABLE drafts ADD COLUMN owner_id TEXT NOT NULL DEFAULT '`+DefaultOwner+`' REFERENCES owners(id);
ALTER TABLE api_tokens ADD COLUMN owner_id TEXT NOT NULL DEFAULT '`+DefaultOwner+`' REFERENCES owners(id);
ALTER TABLE users ADD COLUMN owner_id TEXT NOT NULL DEFAULT '`+DefaultOwner+`' REFERENCES owners(id);`)
	return err
}

// migrateTargetItems (v4) gives every target an account and a list of items.
// post_targets is rebuilt (its UNIQUE(post_id, platform) becomes
// UNIQUE(post_id, account_id)); each target's segments_json entries, or the
// target itself when it was a single post, become target_items rows;
// target_relays become item_deliveries of the target's first item; attempts
// and fan-out rows point at the item they were for where that is known.
func migrateTargetItems(ctx context.Context, tx execer) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE target_items (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  target_id       INTEGER NOT NULL REFERENCES post_targets(id),
  ordinal         INTEGER NOT NULL,
  content_json    TEXT NOT NULL,
  status          TEXT NOT NULL,
  idem_key        TEXT NOT NULL,
  remote_id       TEXT,
  remote_url      TEXT,
  ref_json        TEXT,
  error_class     TEXT,
  error           TEXT,
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMP,
  published_at    TIMESTAMP,
  UNIQUE(target_id, ordinal)
);
CREATE INDEX idx_target_items_due ON target_items(status, next_attempt_at);
CREATE TABLE item_deliveries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  item_id      INTEGER NOT NULL REFERENCES target_items(id),
  endpoint     TEXT NOT NULL,
  status       TEXT NOT NULL,
  message      TEXT,
  attempted_at TIMESTAMP NOT NULL,
  retry_count  INTEGER NOT NULL DEFAULT 0,
  gave_up_at   TIMESTAMP,
  UNIQUE(item_id, endpoint)
);
ALTER TABLE target_attempts ADD COLUMN item_id INTEGER REFERENCES target_items(id);
ALTER TABLE nostr_fanout ADD COLUMN item_id INTEGER REFERENCES target_items(id);`); err != nil {
		return err
	}
	if err := backfillTargetAccounts(ctx, tx); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	if err := backfillItems(ctx, tx); err != nil {
		return fmt.Errorf("items: %w", err)
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO item_deliveries (item_id, endpoint, status, message, attempted_at, retry_count, gave_up_at)
  SELECT ti.id, tr.relay_url, tr.status, tr.message, tr.attempted_at, tr.retry_count, tr.gave_up_at
    FROM target_relays tr JOIN target_items ti ON ti.target_id = tr.target_id AND ti.ordinal = 0;
UPDATE target_attempts SET item_id = (
  SELECT ti.id FROM target_items ti
   WHERE ti.target_id = target_attempts.target_id AND ti.ordinal = 0
     AND (SELECT COUNT(*) FROM target_items x WHERE x.target_id = target_attempts.target_id) = 1);
UPDATE nostr_fanout SET item_id = (
  SELECT ti.id FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
   WHERE pt.post_id = nostr_fanout.post_id AND ti.ref_json = nostr_fanout.signed_event_json
   ORDER BY ti.id LIMIT 1);
CREATE INDEX idx_item_deliveries_item ON item_deliveries(item_id);
CREATE INDEX idx_target_attempts_target ON target_attempts(target_id);
DROP INDEX idx_target_relays_target;
DROP TABLE target_relays;

CREATE TABLE post_targets_new (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  post_id         TEXT NOT NULL REFERENCES posts(id),
  account_id      TEXT NOT NULL REFERENCES accounts(id),
  platform        TEXT NOT NULL,
  final_text      TEXT,
  fields_json     TEXT,
  status          TEXT NOT NULL,
  error_class     TEXT,
  latency_ms      INTEGER,
  attempt_count   INTEGER NOT NULL DEFAULT 0,
  last_attempt_at TIMESTAMP,
  gave_up_at      TIMESTAMP,
  UNIQUE(post_id, account_id)
);
INSERT INTO post_targets_new (id, post_id, account_id, platform, final_text, fields_json, status,
                              error_class, latency_ms, attempt_count, last_attempt_at, gave_up_at)
  SELECT pt.id, pt.post_id,
         (SELECT a.id FROM accounts a WHERE a.owner_id = COALESCE(p.owner_id, '`+DefaultOwner+`')
             AND a.type = pt.platform AND a.source = 'env' AND a.env_key = 'default'),
         pt.platform, pt.final_text, pt.fields_json, pt.status,
         pt.error_class, pt.latency_ms, pt.attempt_count, pt.last_attempt_at, pt.gave_up_at
    FROM post_targets pt LEFT JOIN posts p ON p.id = pt.post_id;
DROP INDEX idx_targets_post;
DROP TABLE post_targets;
ALTER TABLE post_targets_new RENAME TO post_targets;
CREATE INDEX idx_targets_post ON post_targets(post_id);`)
	return err
}

// backfillTargetAccounts creates the env account of every destination type
// history mentions, so every target can point at one. Startup seeding
// (SeedEnvAccounts) later enables the configured ones and disables the rest.
func backfillTargetAccounts(ctx context.Context, tx execer) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT COALESCE(p.owner_id, '`+DefaultOwner+`'), pt.platform
		FROM post_targets pt LEFT JOIN posts p ON p.id = pt.post_id`)
	if err != nil {
		return err
	}
	type pair struct{ owner, typ string }
	var pairs []pair
	for rows.Next() {
		var pr pair
		if err := rows.Scan(&pr.owner, &pr.typ); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, pr)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, pr := range pairs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (id, owner_id, type, label, source, env_key, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'env', 'default', ?, ?) ON CONFLICT (owner_id, type, env_key) DO NOTHING`,
			newAccountID(), pr.owner, pr.typ, pr.typ, now, now); err != nil {
			return err
		}
	}
	return nil
}

// legacyTarget is the part of a pre-v4 post_targets row that moves to items.
type legacyTarget struct {
	id                    int64
	postID, platform      string
	scheduledAt           sql.NullTime
	finalText, status     string
	remoteID, remoteURL   sql.NullString
	signedEvent, segments sql.NullString
	errorClass            sql.NullString
	attempts, mediaCount  int
	lastAttempt           sql.NullTime
}

// backfillItems turns each target's segments, or the target itself when it
// was a single post, into target_items rows.
func backfillItems(ctx context.Context, tx execer) error {
	rows, err := tx.QueryContext(ctx, `SELECT pt.id, pt.post_id, pt.platform, p.scheduled_at,
		COALESCE(pt.final_text, ''), pt.status, pt.remote_id, pt.remote_url,
		pt.signed_event_json, pt.segments_json, pt.error_class, pt.attempt_count, pt.last_attempt_at,
		(SELECT COUNT(*) FROM media m WHERE m.post_id = pt.post_id)
		FROM post_targets pt LEFT JOIN posts p ON p.id = pt.post_id ORDER BY pt.id`)
	if err != nil {
		return err
	}
	var targets []legacyTarget
	for rows.Next() {
		var t legacyTarget
		if err := rows.Scan(&t.id, &t.postID, &t.platform, &t.scheduledAt, &t.finalText, &t.status, &t.remoteID, &t.remoteURL, &t.signedEvent, &t.segments,
			&t.errorClass, &t.attempts, &t.lastAttempt, &t.mediaCount); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range targets {
		items, err := legacyItems(t)
		if err != nil {
			return fmt.Errorf("target %d: %w", t.id, err)
		}
		for _, it := range items {
			if err := insertItem(ctx, tx, t.id, it); err != nil {
				return fmt.Errorf("target %d item %d: %w", t.id, it.Ordinal, err)
			}
		}
	}
	return nil
}

// legacyItems derives a pre-v4 target's items, keyed as the dispatcher
// keyed them (ItemKey), with planned items due when the engine should claim
// them: a scheduled target's at its time, a missed one's never (until a
// retry), any other's now (they wait for the item before them).
func legacyItems(t legacyTarget) ([]Item, error) {
	items, err := legacyItemsOf(t)
	if err != nil {
		return nil, err
	}
	var due *time.Time
	switch {
	case t.status == "missed":
	case t.status == "scheduled" && t.scheduledAt.Valid:
		at := t.scheduledAt.Time.UTC()
		due = &at
	default:
		at := time.Now().UTC()
		if t.lastAttempt.Valid {
			at = t.lastAttempt.Time.UTC()
		}
		due = &at
	}
	for i := range items {
		items[i].IdemKey = ItemKey(t.postID, t.platform, items[i].Ordinal)
		if items[i].Status == ItemPlanned {
			items[i].NextAttemptAt = due
		}
	}
	return items, nil
}

func legacyItemsOf(t legacyTarget) ([]Item, error) {
	var published *time.Time
	if t.lastAttempt.Valid {
		at := t.lastAttempt.Time.UTC()
		published = &at
	}
	if t.segments.String != "" {
		var segs []storedSegment
		if err := json.Unmarshal([]byte(t.segments.String), &segs); err != nil {
			// One unreadable thread must not stop the upgrade (and so every
			// start after it): keep the target as a single item from its own
			// columns, which hold the head's text and remote ids.
			slog.Warn("migration: unreadable thread parts, keeping the target as one item",
				"post_id", t.postID, "target_id", t.id, "err", err)
			segs = nil
		}
		if len(segs) > 0 {
			// The failure that stopped the chain belongs to the first unsent
			// part with an error, else the first unsent part.
			stop := -1
			for i, sg := range segs {
				if sg.RemoteID == "" && (stop < 0 || (sg.Error != "" && segs[stop].Error == "")) {
					stop = i
				}
			}
			items := make([]Item, 0, len(segs))
			for i, sg := range segs {
				sg.Segment.Ref = sg.Ref
				ref := []byte(segmentRef(sg.Segment))
				if len(ref) == 0 && i == 0 && t.signedEvent.String != "" {
					ref = []byte(t.signedEvent.String)
				}
				it := Item{Ordinal: i, Text: sg.Text, Media: sg.Images, Status: itemStatusOf(sg.Status),
					RemoteID: sg.RemoteID, RemoteURL: sg.RemoteURL, Ref: ref, Error: sg.Error}
				if it.Status == ItemPublished || it.Status == ItemPartial {
					it.PublishedAt = published
				}
				if i == stop {
					it.ErrorClass = t.errorClass.String
				}
				items = append(items, it)
			}
			return items, nil
		}
	}
	it := Item{Ordinal: 0, Text: t.finalText, Media: allMedia(t.mediaCount), Status: itemStatusOf(t.status),
		RemoteID: t.remoteID.String, RemoteURL: t.remoteURL.String, Ref: []byte(t.signedEvent.String),
		ErrorClass: t.errorClass.String, AttemptCount: t.attempts}
	if it.Status == ItemPublished || it.Status == ItemPartial {
		it.PublishedAt = published
	}
	return []Item{it}, nil
}

func allMedia(n int) []int {
	if n == 0 {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
