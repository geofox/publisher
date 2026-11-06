package store

import (
	"context"
	"database/sql"
	"time"
)

// FanoutJob is one (signed event × secondary relay) delivery owed by the async
// fan-out worker.
type FanoutJob struct {
	ID              int64
	SignedEventJSON string
	RelayURL        string
	RetryCount      int
	// ItemID is the item the event published (0 for a job queued before
	// v11 whose item couldn't be found); AccountID the account that signed
	// it ("" when unknown).
	ItemID    int64
	AccountID string
}

// EnqueueFanout records one pending fan-out row per relay for a signed event,
// due immediately. post_id is a loose tag for tracing/cleanup (no FK), so events
// already live on the primary relay fan out regardless of post-row timing.
func (s *Store) EnqueueFanout(postID string, itemID int64, signedEventJSON string, relays []string) error {
	if signedEventJSON == "" || len(relays) == 0 {
		return nil
	}
	now := time.Now().UTC()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range relays {
		if _, err := tx.Exec(
			`INSERT INTO nostr_fanout(post_id,item_id,signed_event_json,relay_url,status,retry_count,next_attempt_at,created_at)
			 VALUES(?,?,?,?,'pending',0,?,?)`,
			postID, nullInt(itemID), signedEventJSON, r, now, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DueFanout returns deliverable jobs (pending or failed-and-due), oldest first.
func (s *Store) DueFanout(now time.Time, limit int) ([]FanoutJob, error) {
	if limit <= 0 {
		limit = 50
	}
	// A job without an item id (queued before v11, its item not found then)
	// is matched by post and event, else has no account.
	rows, err := s.sql.Query(
		`SELECT f.id, f.signed_event_json, f.relay_url, f.retry_count,
		        COALESCE(f.item_id, (SELECT ti.id FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
		                              WHERE pt.post_id = f.post_id AND ti.ref_json = f.signed_event_json
		                              ORDER BY ti.id LIMIT 1), 0)
		   FROM nostr_fanout f
		  WHERE f.status IN ('pending','failed') AND f.next_attempt_at <= ?
		  ORDER BY f.next_attempt_at, f.id LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FanoutJob
	for rows.Next() {
		var j FanoutJob
		if err := rows.Scan(&j.ID, &j.SignedEventJSON, &j.RelayURL, &j.RetryCount, &j.ItemID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].ItemID == 0 {
			continue
		}
		if err := s.sql.QueryRow(`SELECT pt.account_id FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
			WHERE ti.id = ?`, out[i].ItemID).Scan(&out[i].AccountID); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}

// MarkFanoutOK marks a delivered job done.
func (s *Store) MarkFanoutOK(id int64) error {
	_, err := s.sql.Exec(`UPDATE nostr_fanout SET status='ok' WHERE id=?`, id)
	return err
}

// MarkFanoutRetry bumps the attempt count and defers the next try.
func (s *Store) MarkFanoutRetry(id int64, next time.Time) error {
	_, err := s.sql.Exec(
		`UPDATE nostr_fanout SET status='failed', retry_count=retry_count+1, next_attempt_at=? WHERE id=?`,
		next.UTC(), id)
	return err
}

// MarkFanoutGaveUp stops retrying a job after the cap.
func (s *Store) MarkFanoutGaveUp(id int64) error {
	_, err := s.sql.Exec(`UPDATE nostr_fanout SET status='gave_up' WHERE id=?`, id)
	return err
}

// FanoutRetention is how long settled fan-out jobs (delivered or given up)
// are kept before PurgeFanout deletes them.
const FanoutRetention = 7 * 24 * time.Hour

// PurgeFanout deletes fan-out jobs that settled more than FanoutRetention
// before now. Each holds a whole signed event, one per item and relay.
func (s *Store) PurgeFanout(ctx context.Context, now time.Time) (int, error) {
	res, err := s.sql.ExecContext(ctx, `DELETE FROM nostr_fanout WHERE status IN ('ok', 'gave_up') AND created_at < ?`,
		now.Add(-FanoutRetention).UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
