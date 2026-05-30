package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Interaction records that a Post is a reply/repost/quote of an external source.
type Interaction struct {
	Action         string `json:"action"`          // reply|repost|quote
	SourcePlatform string `json:"source_platform"`
	SourceURL      string `json:"source_url"`
	SourceAuthor   string `json:"source_author"`
}

type Post struct {
	ID           string       `json:"id"`
	CreatedAt    time.Time    `json:"created_at"`
	MasterText   string       `json:"master_text"`
	Platforms    []string     `json:"platforms"`
	DelaySeconds int          `json:"delay_seconds"`
	Source       string       `json:"source"`
	Status       string       `json:"status"`
	ScheduledAt  *time.Time   `json:"scheduled_at,omitempty"`
	FiredAt      *time.Time   `json:"fired_at,omitempty"` // list view: latest target attempt time (actual publish/retry)
	// FirstSuccessAt is the earliest time the post went live on ANY platform
	// (MIN over successful attempts). Set only by PublicFeed, never serialized.
	// Retries append later attempt rows, so this never moves once set.
	FirstSuccessAt *time.Time `json:"-"`
	Targets      []Target     `json:"targets,omitempty"`
	Media        []Media      `json:"media,omitempty"`
	Interaction  *Interaction `json:"interaction,omitempty"`
}

type Target struct {
	ID              int64        `json:"id"`
	Platform        string       `json:"platform"`
	FinalText       string       `json:"final_text"`
	FieldsJSON      string       `json:"fields_json"`
	Status          string       `json:"status"`
	RemoteID        string       `json:"remote_id"`
	RemoteURL       string       `json:"remote_url"`
	LatencyMS       int          `json:"latency_ms"`
	AttemptCount    int          `json:"attempt_count"`
	LastAttempt     time.Time    `json:"last_attempt"`
	GaveUpAt        *time.Time   `json:"gave_up_at,omitempty"`
	Attempts        []Attempt    `json:"attempts,omitempty"`
	Relays          []RelayState `json:"relays,omitempty"`
	Segments        []Segment    `json:"segments,omitempty"`
	SignedEventJSON string       `json:"-"` // nostr only; never sent to the client
}

type Attempt struct {
	AttemptNo    int       `json:"attempt_no"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	LatencyMS    int       `json:"latency_ms"`
	RemoteID     string    `json:"remote_id"`
	RequestJSON  string    `json:"request_json"`
	ResponseJSON string    `json:"response_json"`
	AttemptedAt  time.Time `json:"attempted_at"`
}

type Media struct {
	Ordinal    int    `json:"ordinal"`
	BlossomURL string `json:"blossom_url"`
	SHA256     string `json:"sha256"`
	Mime       string `json:"mime"`
	Dim        string `json:"dim"`
	Blurhash   string `json:"blurhash"`
	SizeBytes  int64  `json:"size_bytes"`
	Alt        string `json:"alt"`
}

type RelayState struct {
	URL        string     `json:"url"`
	Status     string     `json:"status"` // ok | failed | skipped
	Message    string     `json:"message,omitempty"`
	GaveUpAt   *time.Time `json:"gave_up_at,omitempty"`
	RetryCount int        `json:"retry_count,omitempty"`
}

// Segment is one post in a platform's reply-chain. A non-threaded target has no
// segments; a threaded one has an ordered slice (ordinal 0 = the chain head).
type Segment struct {
	Ordinal   int    `json:"ordinal"`
	Text      string `json:"text"`
	RemoteID  string `json:"remote_id,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	CID       string `json:"cid,omitempty"` // bluesky only
	Status    string `json:"status"`        // success | failed | pending
	Error     string `json:"error,omitempty"`
}

func (s *Store) SavePost(p *Post) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	platforms, err := json.Marshal(p.Platforms)
	if err != nil {
		return err
	}
	var interactionJSON string
	if p.Interaction != nil {
		b, _ := json.Marshal(p.Interaction)
		interactionJSON = string(b)
	}
	if _, err = tx.Exec(
		`INSERT INTO posts(id,created_at,master_text,platforms,delay_seconds,source,status,scheduled_at,interaction_json)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		p.ID, p.CreatedAt, p.MasterText, string(platforms), p.DelaySeconds, p.Source, p.Status, p.ScheduledAt, interactionJSON,
	); err != nil {
		return err
	}
	for _, m := range p.Media {
		if _, err = tx.Exec(
			`INSERT INTO media(post_id,ordinal,blossom_url,sha256,mime,dim,blurhash,size_bytes,alt)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			p.ID, m.Ordinal, m.BlossomURL, m.SHA256, m.Mime, m.Dim, m.Blurhash, m.SizeBytes, m.Alt,
		); err != nil {
			return err
		}
	}
	for _, tg := range p.Targets {
		segJSON := ""
		if len(tg.Segments) > 0 {
			b, mErr := json.Marshal(tg.Segments)
			if mErr != nil {
				return mErr
			}
			segJSON = string(b)
		}
		res, err := tx.Exec(
			`INSERT INTO post_targets(post_id,platform,final_text,fields_json,status,remote_id,remote_url,latency_ms,attempt_count,last_attempt_at,signed_event_json,segments_json)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, tg.Platform, tg.FinalText, tg.FieldsJSON, tg.Status, tg.RemoteID, tg.RemoteURL,
			tg.LatencyMS, len(tg.Attempts), time.Now().UTC(), tg.SignedEventJSON, segJSON,
		)
		if err != nil {
			return err
		}
		tid, _ := res.LastInsertId()
		for _, rl := range tg.Relays {
			if _, err = tx.Exec(
				`INSERT INTO target_relays(target_id,relay_url,status,message,attempted_at)
				 VALUES(?,?,?,?,?)
				 ON CONFLICT(target_id,relay_url) DO UPDATE SET status=excluded.status, message=excluded.message, attempted_at=excluded.attempted_at`,
				tid, rl.URL, rl.Status, rl.Message, time.Now().UTC(),
			); err != nil {
				return err
			}
		}
		for _, at := range tg.Attempts {
			if _, err = tx.Exec(
				`INSERT INTO target_attempts(target_id,attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at)
				 VALUES(?,?,?,?,?,?,?,?,?)`,
				tid, at.AttemptNo, at.Status, at.Error, at.LatencyMS, at.RemoteID, Scrub(at.RequestJSON), Scrub(at.ResponseJSON), at.AttemptedAt,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) GetPost(id string) (*Post, error) {
	p := &Post{}
	var platforms string
	var schedAt sql.NullTime
	var interactionJSON sql.NullString // NULL on rows that predate the column migration
	err := s.sql.QueryRow(
		`SELECT id,created_at,master_text,platforms,delay_seconds,source,status,scheduled_at,interaction_json FROM posts WHERE id=?`, id,
	).Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.DelaySeconds, &p.Source, &p.Status, &schedAt, &interactionJSON)
	if err != nil {
		return nil, err
	}
	if schedAt.Valid {
		t := schedAt.Time.UTC()
		p.ScheduledAt = &t
	}
	_ = json.Unmarshal([]byte(platforms), &p.Platforms)
	if interactionJSON.String != "" {
		var ix Interaction
		if json.Unmarshal([]byte(interactionJSON.String), &ix) == nil {
			p.Interaction = &ix
		}
	}

	mrows, err := s.sql.Query(`SELECT ordinal,blossom_url,sha256,mime,dim,blurhash,size_bytes,alt FROM media WHERE post_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m Media
		if err := mrows.Scan(&m.Ordinal, &m.BlossomURL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes, &m.Alt); err != nil {
			return nil, err
		}
		p.Media = append(p.Media, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	trows, err := s.sql.Query(`SELECT id,platform,final_text,fields_json,status,remote_id,remote_url,latency_ms,attempt_count,last_attempt_at,gave_up_at,signed_event_json,segments_json FROM post_targets WHERE post_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var tg Target
		var fields, rid, rurl, sej, segs sql.NullString
		var la, gu sql.NullTime
		if err := trows.Scan(&tg.ID, &tg.Platform, &tg.FinalText, &fields, &tg.Status, &rid, &rurl, &tg.LatencyMS, &tg.AttemptCount, &la, &gu, &sej, &segs); err != nil {
			return nil, err
		}
		tg.FieldsJSON, tg.RemoteID, tg.RemoteURL, tg.SignedEventJSON = fields.String, rid.String, rurl.String, sej.String
		if la.Valid {
			tg.LastAttempt = la.Time.UTC()
		}
		if gu.Valid {
			t := gu.Time.UTC()
			tg.GaveUpAt = &t
		}
		if segs.String != "" {
			if err := json.Unmarshal([]byte(segs.String), &tg.Segments); err != nil {
				return nil, err
			}
		}
		p.Targets = append(p.Targets, tg)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	for i := range p.Targets {
		arows, err := s.sql.Query(`SELECT attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at FROM target_attempts WHERE target_id=? ORDER BY attempt_no`, p.Targets[i].ID)
		if err != nil {
			return nil, err
		}
		for arows.Next() {
			var a Attempt
			var e, rid, req, resp sql.NullString
			if err := arows.Scan(&a.AttemptNo, &a.Status, &e, &a.LatencyMS, &rid, &req, &resp, &a.AttemptedAt); err != nil {
				arows.Close()
				return nil, err
			}
			a.Error, a.RemoteID, a.RequestJSON, a.ResponseJSON = e.String, rid.String, req.String, resp.String
			p.Targets[i].Attempts = append(p.Targets[i].Attempts, a)
		}
		if err := arows.Err(); err != nil {
			arows.Close()
			return nil, err
		}
		arows.Close()
	}
	for i := range p.Targets {
		rrows, err := s.sql.Query(`SELECT relay_url,status,message,gave_up_at,retry_count FROM target_relays WHERE target_id=? ORDER BY id`, p.Targets[i].ID)
		if err != nil {
			return nil, err
		}
		for rrows.Next() {
			var rs RelayState
			var msg sql.NullString
			var rgu sql.NullTime
			if err := rrows.Scan(&rs.URL, &rs.Status, &msg, &rgu, &rs.RetryCount); err != nil {
				rrows.Close()
				return nil, err
			}
			rs.Message = msg.String
			if rgu.Valid {
				t := rgu.Time.UTC()
				rs.GaveUpAt = &t
			}
			p.Targets[i].Relays = append(p.Targets[i].Relays, rs)
		}
		if err := rrows.Err(); err != nil {
			rrows.Close()
			return nil, err
		}
		rrows.Close()
	}
	return p, nil
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx.
type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

func recomputeStatus(q rowQuerier, postID string) error {
	rows, err := q.Query(`SELECT status FROM post_targets WHERE post_id=?`, postID)
	if err != nil {
		return err
	}
	defer rows.Close()
	total, succ, fail := 0, 0, 0
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return err
		}
		total++
		switch st {
		case "success":
			succ++
		case "failed":
			fail++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	status := "partial"
	switch {
	case total == 0 || fail == total:
		status = "failed"
	case succ == total:
		status = "success"
	}
	_, err = q.Exec(`UPDATE posts SET status=? WHERE id=?`, status, postID)
	return err
}

// recomputeTargetStatus derives a target's status from its relay rows
// (skipped rows ignored): success = all attempted ok, failed = none ok,
// partial = some ok. No-op for targets without relay rows (non-Nostr).
func recomputeTargetStatus(q rowQuerier, targetID int64) error {
	rows, err := q.Query(`SELECT status FROM target_relays WHERE target_id=?`, targetID)
	if err != nil {
		return err
	}
	defer rows.Close()
	attempted, ok, seen := 0, 0, false
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return err
		}
		seen = true
		if st == "skipped" {
			continue
		}
		attempted++
		if st == "ok" {
			ok++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !seen {
		return nil // non-nostr target: leave status untouched
	}
	// All-skipped (e.g. only an unreachable .onion) → nothing was delivered → failed.
	status := "partial"
	switch {
	case ok == 0:
		status = "failed"
	case ok == attempted:
		status = "success"
	}
	_, err = q.Exec(`UPDATE post_targets SET status=? WHERE id=?`, status, targetID)
	return err
}

// UpdateRelayStatus flips one relay row after a per-relay retry, then recomputes
// the target's status and the post's overall status, all in one transaction.
func (s *Store) UpdateRelayStatus(targetID int64, relayURL, status, message string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE target_relays
		    SET status=?, message=?, attempted_at=?, retry_count = retry_count + 1,
		        gave_up_at = CASE WHEN ?='ok' THEN NULL ELSE gave_up_at END
		  WHERE target_id=? AND relay_url=?`,
		status, message, time.Now().UTC(), status, targetID, relayURL,
	); err != nil {
		return err
	}
	if err := recomputeTargetStatus(tx, targetID); err != nil {
		return err
	}
	var postID string
	if err := tx.QueryRow(`SELECT post_id FROM post_targets WHERE id=?`, targetID).Scan(&postID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendTargetAttempt records a retry: inserts a new target_attempts row
// (attempt_no = current+1, request/response scrubbed) and updates the target's
// latest state. For a Nostr whole-platform retry (relays non-empty) it also
// refreshes the signed event and per-relay rows, so the archive can't drift
// into showing a green target above stale failed relays or rebroadcasting a
// superseded event. Recomputes the post's overall status in the same transaction.
func (s *Store) AppendTargetAttempt(targetID int64, status, errMsg, remoteID, remoteURL string, latencyMS int, requestJSON, responseJSON string, relays []RelayState, signedEventJSON string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	var n int
	if err := tx.QueryRow(`SELECT attempt_count FROM post_targets WHERE id=?`, targetID).Scan(&n); err != nil {
		return err
	}
	n++
	if _, err := tx.Exec(
		`INSERT INTO target_attempts(target_id,attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		targetID, n, status, errMsg, latencyMS, remoteID, Scrub(requestJSON), Scrub(responseJSON), now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE post_targets
		    SET status=?, remote_id=?, remote_url=?, latency_ms=?, attempt_count=?, last_attempt_at=?,
		        gave_up_at = CASE WHEN ?='success' THEN NULL ELSE gave_up_at END
		  WHERE id=?`,
		status, remoteID, remoteURL, latencyMS, n, now, status, targetID,
	); err != nil {
		return err
	}
	if len(relays) > 0 { // nostr: refresh the signed event + per-relay rows
		if _, err := tx.Exec(`UPDATE post_targets SET signed_event_json=? WHERE id=?`, signedEventJSON, targetID); err != nil {
			return err
		}
		for _, rl := range relays {
			if _, err := tx.Exec(
				`INSERT INTO target_relays(target_id,relay_url,status,message,attempted_at)
				 VALUES(?,?,?,?,?)
				 ON CONFLICT(target_id,relay_url) DO UPDATE SET status=excluded.status, message=excluded.message, attempted_at=excluded.attempted_at`,
				targetID, rl.URL, rl.Status, rl.Message, now,
			); err != nil {
				return err
			}
		}
	}
	var postID string
	if err := tx.QueryRow(`SELECT post_id FROM post_targets WHERE id=?`, targetID).Scan(&postID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTargetSegments overwrites a threaded target's segment chain + status
// (used when resuming a partial thread), records a new attempt, and recomputes
// the post's aggregate status. Mirrors AppendTargetAttempt's bookkeeping.
func (s *Store) UpdateTargetSegments(targetID int64, segments []Segment, status, headRemoteID, headRemoteURL string, latencyMS int, errMsg string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	segJSON, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var n int
	if err := tx.QueryRow(`SELECT attempt_count FROM post_targets WHERE id=?`, targetID).Scan(&n); err != nil {
		return err
	}
	n++
	if _, err := tx.Exec(
		`INSERT INTO target_attempts(target_id,attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		targetID, n, status, errMsg, latencyMS, headRemoteID, "", "", now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE post_targets
		    SET status=?, remote_id=?, remote_url=?, latency_ms=?, attempt_count=?, last_attempt_at=?, segments_json=?,
		        gave_up_at = CASE WHEN ?='success' THEN NULL ELSE gave_up_at END
		  WHERE id=?`,
		status, headRemoteID, headRemoteURL, latencyMS, n, now, string(segJSON), status, targetID,
	); err != nil {
		return err
	}
	var postID string
	if err := tx.QueryRow(`SELECT post_id FROM post_targets WHERE id=?`, targetID).Scan(&postID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecomputeStatus recomputes a post's overall status from its targets.
func (s *Store) RecomputeStatus(postID string) error {
	return recomputeStatus(s.sql, postID)
}

// ErrNotPending is returned by cancel/reschedule when a post is not 'scheduled'.
var ErrNotPending = errors.New("post is not pending")

// ErrNotHideable is returned by HidePost when the post is missing or still
// pending (scheduled/sending) — pending posts must be cancelled, not hidden.
var ErrNotHideable = errors.New("post cannot be hidden")

// HidePost soft-deletes a terminal post: it stays in the archive (data, retry
// history, backups) but is excluded from the history list. Pending posts
// (scheduled/sending) can't be hidden.
func (s *Store) HidePost(id string) error {
	res, err := s.sql.Exec(`UPDATE posts SET hidden=1 WHERE id=? AND status NOT IN ('scheduled','sending')`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotHideable
	}
	return nil
}

// DueScheduledPosts returns IDs of scheduled posts whose time has arrived.
func (s *Store) DueScheduledPosts(now time.Time) ([]string, error) {
	rows, err := s.sql.Query(
		`SELECT id FROM posts WHERE status='scheduled' AND scheduled_at <= ? ORDER BY scheduled_at`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimScheduled atomically flips scheduled→sending. Returns true iff this call
// won the row (guards double-fire).
func (s *Store) ClaimScheduled(id string) (bool, error) {
	res, err := s.sql.Exec(`UPDATE posts SET status='sending' WHERE id=? AND status='scheduled'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkMissed marks the post and its still-scheduled targets as missed.
func (s *Store) MarkMissed(id string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Only the worker calls this, after ClaimScheduled flipped the post to
	// 'sending'; guard on that so a stray call can't clobber a posted record.
	if _, err := tx.Exec(`UPDATE posts SET status='missed' WHERE id=? AND status='sending'`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE post_targets SET status='missed' WHERE post_id=? AND status='scheduled'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetSendingToScheduled reverts crash-leftover 'sending' rows to 'scheduled'.
// Safe only at startup, before the worker runs (the worker is the sole writer of
// 'sending').
func (s *Store) ResetSendingToScheduled() (int, error) {
	res, err := s.sql.Exec(`UPDATE posts SET status='scheduled' WHERE status='sending'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RescheduleScheduled changes a pending post's time; ErrNotPending otherwise.
func (s *Store) RescheduleScheduled(id string, at time.Time) error {
	res, err := s.sql.Exec(`UPDATE posts SET scheduled_at=? WHERE id=? AND status='scheduled'`, at.UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	return nil
}

// CancelScheduled deletes a pending post (and its rows); ErrNotPending otherwise.
func (s *Store) CancelScheduled(id string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Atomically claim the cancel: this guarded write is the first statement, so
	// it takes the write lock and blocks a racing ClaimScheduled. RowsAffected==0
	// means the post is gone or no longer pending (e.g. just claimed for sending).
	res, err := tx.Exec(`UPDATE posts SET status='cancelling' WHERE id=? AND status='scheduled'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	// FK has no ON DELETE CASCADE + foreign_keys=ON → delete children first.
	sub := `(SELECT id FROM post_targets WHERE post_id=?)`
	if _, err := tx.Exec(`DELETE FROM target_relays WHERE target_id IN `+sub, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM target_attempts WHERE target_id IN `+sub, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM post_targets WHERE post_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM media WHERE post_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM posts WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// PostsNeedingRetry returns IDs of posts that have at least one target in a
// recoverable state (failed or partial) that has not yet given up. 'missed'
// (scheduling miss) and 'success' are excluded. The caller (Retrier) loads
// each post and applies backoff/cap per target.
func (s *Store) PostsNeedingRetry() ([]string, error) {
	rows, err := s.sql.Query(
		`SELECT DISTINCT post_id FROM post_targets
		  WHERE status IN ('failed','partial') AND gave_up_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkTargetGaveUp stamps gave_up_at on a platform target, but only if it is
// still NULL. Returns true iff this call set it (so the caller alerts once).
func (s *Store) MarkTargetGaveUp(targetID int64, at time.Time) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE post_targets SET gave_up_at=? WHERE id=? AND gave_up_at IS NULL`,
		at.UTC(), targetID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkRelayGaveUp stamps gave_up_at on a single relay row, only if still NULL.
// Returns true iff this call set it.
func (s *Store) MarkRelayGaveUp(targetID int64, relayURL string, at time.Time) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE target_relays SET gave_up_at=? WHERE target_id=? AND relay_url=? AND gave_up_at IS NULL`,
		at.UTC(), targetID, relayURL)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AttentionCount returns the number of non-hidden posts needing attention
// (delivery failed or partial). Drives the History "attention" badge.
func (s *Store) AttentionCount() (int, error) {
	var n int
	err := s.sql.QueryRow(
		`SELECT COUNT(*) FROM posts WHERE hidden=0 AND status IN ('failed','partial')`).Scan(&n)
	return n, err
}

// PostFilter controls filtering/paging for ListPostsFiltered.
type PostFilter struct {
	Status string // "sent", "scheduled", "failed", "" / "all" → no filter
	Query  string // substring match on master_text; SQLite LIKE → ASCII-only case-insensitive
	Limit  int
	Offset int
}

// statusClause returns the SQL predicate fragment for p.status, or "" for no filter.
func statusClause(status string) string {
	switch status {
	case "scheduled":
		return "p.status IN ('scheduled','sending')"
	case "attention":
		return "p.status IN ('failed','partial')"
	case "failed":
		return "p.status IN ('failed','partial','missed')"
	case "sent":
		return "p.status='success'"
	default:
		return "" // "", "all", or any unknown value → no predicate
	}
}

// escapeLike escapes LIKE metacharacters so the search term matches literally
// (a substring), not as a wildcard pattern — otherwise a query like "100%" or
// "a_b" would match unrelated rows. Pairs with the `ESCAPE '\'` clause.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListPosts returns posts newest-first. Each post carries a lightweight Targets
// slice (platform + status only) so the list view can color per-platform pills;
// media and full attempt history are not hydrated (use GetPost for detail).
func (s *Store) ListPosts(limit, offset int) ([]Post, error) {
	return s.ListPostsFiltered(PostFilter{Limit: limit, Offset: offset})
}

// ListPostsFiltered is the generalized form of ListPosts supporting optional
// status filtering, full-text search, and explicit paging.
func (s *Store) ListPostsFiltered(f PostFilter) ([]Post, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 { // cap to bound memory if a caller passes a huge page size
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	predicates := []string{"p.hidden=0"}
	var args []any

	if sc := statusClause(f.Status); sc != "" {
		predicates = append(predicates, sc)
	}
	if f.Query != "" {
		predicates = append(predicates, `p.master_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(f.Query)+"%")
	}

	where := "WHERE " + strings.Join(predicates, " AND ")
	args = append(args, f.Limit, f.Offset)

	q := `SELECT p.id,p.created_at,p.master_text,p.platforms,p.source,p.status,p.scheduled_at,p.interaction_json,
		        (SELECT MAX(ta.attempted_at) FROM target_attempts ta
		           JOIN post_targets pt ON ta.target_id=pt.id
		          WHERE pt.post_id=p.id) AS fired_at
		   FROM posts p
		  ` + where + `
		  ORDER BY COALESCE((SELECT MAX(ta.attempted_at) FROM target_attempts ta
		           JOIN post_targets pt ON ta.target_id=pt.id
		          WHERE pt.post_id=p.id), p.created_at) DESC
		  LIMIT ? OFFSET ?`

	rows, err := s.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Post, 0)
	for rows.Next() {
		var p Post
		var platforms string
		var sa sql.NullTime
		var fa sql.NullString // MAX() loses the column's TIMESTAMP affinity → comes back as a Go time.String()
		var interactionJSON sql.NullString
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.Source, &p.Status, &sa, &interactionJSON, &fa); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(platforms), &p.Platforms)
		if interactionJSON.String != "" {
			var ix Interaction
			if json.Unmarshal([]byte(interactionJSON.String), &ix) == nil {
				p.Interaction = &ix
			}
		}
		if sa.Valid {
			t := sa.Time.UTC()
			p.ScheduledAt = &t
		}
		if fa.Valid && fa.String != "" {
			if t, perr := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", fa.String); perr == nil {
				u := t.UTC()
				p.FiredAt = &u
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Lightweight per-target status (platform + status) for the list view.
	for i := range out {
		trows, err := s.sql.Query(`SELECT platform,status FROM post_targets WHERE post_id=? ORDER BY id`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for trows.Next() {
			var t Target
			if err := trows.Scan(&t.Platform, &t.Status); err != nil {
				trows.Close()
				return nil, err
			}
			out[i].Targets = append(out[i].Targets, t)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return nil, err
		}
		trows.Close()
	}
	return out, nil
}
