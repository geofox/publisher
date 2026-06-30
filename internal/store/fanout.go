package store

import "time"

// FanoutJob is one (signed event × secondary relay) delivery owed by the async
// fan-out worker.
type FanoutJob struct {
	ID              int64
	SignedEventJSON string
	RelayURL        string
	RetryCount      int
}

// EnqueueFanout records one pending fan-out row per relay for a signed event,
// due immediately. post_id is a loose tag for tracing/cleanup (no FK), so events
// already live on the primary relay fan out regardless of post-row timing.
func (s *Store) EnqueueFanout(postID, signedEventJSON string, relays []string) error {
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
			`INSERT INTO nostr_fanout(post_id,signed_event_json,relay_url,status,retry_count,next_attempt_at,created_at)
			 VALUES(?,?,?,'pending',0,?,?)`,
			postID, signedEventJSON, r, now, now,
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
	rows, err := s.sql.Query(
		`SELECT id,signed_event_json,relay_url,retry_count FROM nostr_fanout
		  WHERE status IN ('pending','failed') AND next_attempt_at <= ?
		  ORDER BY next_attempt_at, id LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FanoutJob
	for rows.Next() {
		var j FanoutJob
		if err := rows.Scan(&j.ID, &j.SignedEventJSON, &j.RelayURL, &j.RetryCount); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
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
