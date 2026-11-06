package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// The publish engine's view of the store. A post is written whole before
// anything is sent (SavePost with planned items); workers then claim one due
// item at a time, publish it, and record the result. These are the claim and
// record steps, and the passes around them: retries made due by hand,
// scheduled posts missed while the process was down, and items a stopped
// process left in flight.

// Claim is an item a worker has marked sending.
type Claim struct {
	PostID   string
	TargetID int64
	ItemID   int64
	Ordinal  int
	Platform string
	// AccountID is the account the item is sent from.
	AccountID string
	// IdemKey is the item's stored idempotency key: always read back,
	// never recomputed.
	IdemKey string
	// Attempts is how many times the item was tried before this claim.
	Attempts int
}

// ClaimFilter narrows ClaimItem.
type ClaimFilter struct {
	// TargetID, when set, claims only that target's items.
	TargetID int64
	// Exclude skips these posts (another caller is driving them).
	Exclude []string
	// MissedBefore, when set, skips scheduled posts due before it: they are
	// missed, not late (MarkOverdueMissed records them).
	MissedBefore time.Time
}

// ErrClaimLost is returned when an item is no longer in the state its
// caller claimed it in.
var ErrClaimLost = errors.New("item is no longer claimed")

// ClaimItem marks the next due item sending and returns it, or nil when none
// is due. An item is due when it is planned, failed or unknown with
// next_attempt_at at or before now, its target has not given up, no other
// item of its target is sending, and every item before it is live. So a
// thread posts in order, one item at a time, and different targets post in
// parallel. The claim also moves its target and post to sending.
//
// For a sending item, next_attempt_at holds when the send started: startup
// recovery reads it to tell whether an idempotency key is still remembered.
func (s *Store) ClaimItem(ctx context.Context, now time.Time, f ClaimFilter) (*Claim, error) {
	now = now.UTC()
	exclude, _ := json.Marshal(f.Exclude)
	if f.Exclude == nil {
		exclude = []byte("[]")
	}
	var missed any
	if !f.MissedBefore.IsZero() {
		missed = f.MissedBefore.UTC()
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// One statement, so the write lock is taken before the candidate is
	// chosen and two workers can't claim the same item.
	var c Claim
	err = tx.QueryRowContext(ctx, `UPDATE target_items SET status = 'sending', next_attempt_at = ?, committing_at = NULL
		WHERE id = (
		  SELECT ti.id FROM target_items ti
		    JOIN post_targets pt ON pt.id = ti.target_id
		    JOIN posts p ON p.id = pt.post_id
		   WHERE ti.status IN ('planned', 'failed', 'unknown')
		     AND ti.next_attempt_at IS NOT NULL AND ti.next_attempt_at <= ?
		     AND pt.gave_up_at IS NULL AND p.canceled_at IS NULL
		     AND p.status NOT IN ('cancelling', 'missed')
		     AND (? = 0 OR pt.id = ?)
		     AND p.id NOT IN (SELECT value FROM json_each(?))
		     AND NOT (p.status = 'scheduled' AND ? IS NOT NULL AND p.scheduled_at < ?)
		     AND NOT EXISTS (SELECT 1 FROM target_items x WHERE x.target_id = ti.target_id AND x.status = 'sending')
		     AND NOT EXISTS (SELECT 1 FROM target_items x WHERE x.target_id = ti.target_id AND x.ordinal < ti.ordinal
		                       AND x.status NOT IN ('published', 'partial'))
		   ORDER BY ti.next_attempt_at, ti.id LIMIT 1)
		RETURNING id, target_id, ordinal, attempt_count, idem_key`,
		now, now, f.TargetID, f.TargetID, string(exclude), missed, missed).Scan(&c.ItemID, &c.TargetID, &c.Ordinal, &c.Attempts, &c.IdemKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT post_id, platform, account_id FROM post_targets WHERE id = ?`, c.TargetID).
		Scan(&c.PostID, &c.Platform, &c.AccountID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = 'sending' WHERE id = ?`, c.TargetID); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE posts SET status = 'sending' WHERE id = ? AND status <> 'sending'`, c.PostID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := emit(ctx, tx, c.PostID, EventPostUpdated, EventData{Status: "sending"}, now); err != nil {
			return nil, err
		}
	}
	ord := c.Ordinal
	if err := emit(ctx, tx, c.PostID, EventItemSending, EventData{Status: ItemSending, TargetID: c.TargetID,
		AccountID: c.AccountID, Platform: c.Platform, Ordinal: &ord}, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.ev.fire()
	return &c, nil
}

// SetItemRef records a claimed item's ref before it is sent, so every
// attempt sends the same thing (a Nostr item's signed event).
func (s *Store) SetItemRef(ctx context.Context, itemID int64, ref json.RawMessage) error {
	res, err := s.sql.ExecContext(ctx, `UPDATE target_items SET ref_json = ? WHERE id = ? AND status = 'sending'`,
		string(ref), itemID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrClaimLost
	}
	return nil
}

// ItemResult is the outcome of one attempt at a claimed item.
type ItemResult struct {
	Claim
	Status              string // published | partial | failed | unknown
	RemoteID, RemoteURL string
	Ref                 json.RawMessage // replaces the item's ref when set
	Error, ErrorClass   string
	LatencyMS           int
	RequestJSON         string
	ResponseJSON        string
	Deliveries          []RelayState
	// NextAttemptAt is when a failed or unknown item is tried again; nil
	// leaves it for the owner.
	NextAttemptAt *time.Time
	// GiveUp stamps the target's gave_up_at: nothing automatic will try it
	// again.
	GiveUp bool
	// NotSent records a claim released without sending (a retry the
	// engine decided against): no attempt row, no attempt counted.
	NotSent bool
	At      time.Time
}

// Outcome is what RecordItemResult changed.
type Outcome struct {
	PostBefore, PostAfter string // the post's status
	TargetStatus          string
	GaveUp                bool // this result stamped the target's gave_up_at
}

// RecordItemResult records an attempt at a claimed item in one transaction:
// the item, its attempt row, its endpoint rows, and its target's and post's
// status. ErrClaimLost when the item is not sending.
func (s *Store) RecordItemResult(ctx context.Context, r ItemResult) (Outcome, error) {
	at := r.At.UTC()
	if r.At.IsZero() {
		at = time.Now().UTC()
	}
	var out Outcome
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var ref any
	if len(r.Ref) > 0 {
		ref = string(r.Ref)
	}
	before, err := readPostState(ctx, tx, r.PostID)
	if err != nil {
		return out, err
	}
	// A canceled post retries nothing on its own: an item in flight at the
	// cancel ends with its outcome, and its target is given up.
	var canceled bool
	if err := tx.QueryRowContext(ctx, `SELECT canceled_at IS NOT NULL FROM posts WHERE id = ?`, r.PostID).Scan(&canceled); err != nil {
		return out, err
	}
	var next any
	if r.NextAttemptAt != nil && !canceled {
		next = r.NextAttemptAt.UTC()
	}
	live := r.Status == ItemPublished || r.Status == ItemPartial
	res, err := tx.ExecContext(ctx, `UPDATE target_items SET status = ?,
		  remote_id = COALESCE(NULLIF(?, ''), remote_id), remote_url = COALESCE(NULLIF(?, ''), remote_url),
		  ref_json = COALESCE(?, ref_json), error_class = ?, error = ?, attempt_count = attempt_count + ?,
		  next_attempt_at = ?, published_at = CASE WHEN ? THEN COALESCE(published_at, ?) ELSE published_at END
		WHERE id = ? AND status = 'sending'`,
		r.Status, r.RemoteID, r.RemoteURL, ref, nullStr(r.ErrorClass), nullStr(Scrub(r.Error)), boolInt(!r.NotSent),
		next, live, at, r.ItemID)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return out, ErrClaimLost
	}
	if err := upsertDeliveries(ctx, tx, r.ItemID, r.Deliveries, at); err != nil {
		return out, err
	}
	if r.NotSent {
		// nothing was sent: no attempt to record
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO target_attempts (target_id, attempt_no, status, error, latency_ms, remote_id,
		  request_json, response_json, attempted_at, error_class, item_id)
		VALUES (?, (SELECT COALESCE(MAX(attempt_no), 0) + 1 FROM target_attempts WHERE target_id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TargetID, r.TargetID, segmentStatusOf(r.Status), Scrub(r.Error), r.LatencyMS, r.RemoteID,
		Scrub(r.RequestJSON), Scrub(r.ResponseJSON), at, nullStr(r.ErrorClass), r.ItemID); err != nil {
		return out, err
	}
	statuses, err := itemStatuses(ctx, tx, r.TargetID)
	if err != nil {
		return out, err
	}
	out.TargetStatus = targetStatusOf(statuses)
	var gaveUp sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT gave_up_at FROM post_targets WHERE id = ?`, r.TargetID).Scan(&gaveUp); err != nil {
		return out, err
	}
	out.GaveUp = r.GiveUp && !gaveUp.Valid
	var latency any
	if r.Ordinal == 0 {
		latency = r.LatencyMS
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = ?, error_class = ?,
		  latency_ms = COALESCE(?, latency_ms), last_attempt_at = ?,
		  attempt_count = (SELECT MAX(attempt_count) FROM target_items WHERE target_id = ?),
		  gave_up_at = CASE WHEN ? THEN COALESCE(gave_up_at, ?) ELSE gave_up_at END
		WHERE id = ?`,
		out.TargetStatus, nullStr(r.ErrorClass), latency, at, r.TargetID, r.GiveUp || (canceled && !live), at, r.TargetID); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM posts WHERE id = ?`, r.PostID).Scan(&out.PostBefore); err != nil {
		return out, err
	}
	if err := recomputeStatus(tx, r.PostID); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM posts WHERE id = ?`, r.PostID).Scan(&out.PostAfter); err != nil {
		return out, err
	}
	// Events are dated now: a result recorded late (startup recovery) is
	// still news to a stream.
	if err := emitItemResult(ctx, tx, r, before, time.Now().UTC()); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	s.ev.fire()
	return out, nil
}

// emitItemResult logs what RecordItemResult changed: the item, its
// endpoints, and the post when its status moved.
func emitItemResult(ctx context.Context, tx *sql.Tx, r ItemResult, before postState, at time.Time) error {
	var account, url string
	if err := tx.QueryRowContext(ctx, `SELECT pt.account_id, COALESCE(ti.remote_url, '') FROM target_items ti
		JOIN post_targets pt ON pt.id = ti.target_id WHERE ti.id = ?`, r.ItemID).Scan(&account, &url); err != nil {
		return err
	}
	ord := r.Ordinal
	base := EventData{TargetID: r.TargetID, AccountID: account, Platform: r.Platform, Ordinal: &ord}
	typ := map[string]string{ItemPublished: EventItemPublished, ItemPartial: EventItemPartial,
		ItemFailed: EventItemFailed, ItemUnknown: EventItemUnknown, ItemCanceled: EventItemCanceled}[r.Status]
	if typ != "" && !r.NotSent {
		d := base
		d.Status, d.RemoteURL, d.ErrorClass, d.Error = r.Status, url, r.ErrorClass, Scrub(r.Error)
		if err := emit(ctx, tx, r.PostID, typ, d, at); err != nil {
			return err
		}
	}
	for _, dl := range r.Deliveries {
		d := base
		d.Status, d.Endpoint, d.Error = dl.Status, dl.URL, Scrub(dl.Message)
		if err := emit(ctx, tx, r.PostID, EventDelivery, d, at); err != nil {
			return err
		}
	}
	return emitPostChange(ctx, tx, r.PostID, before, at)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itemStatuses(ctx context.Context, tx *sql.Tx, targetID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT status FROM target_items WHERE target_id = ? ORDER BY ordinal`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// targetStatusOf derives a target's status (the /api vocabulary) from its
// items' statuses in ordinal order: failed until the first item is live,
// success when every item is published, partial when something after a
// live first item failed (or an item is live but incomplete), and sending
// while items are still to go.
func targetStatusOf(items []string) string {
	if len(items) == 0 {
		return "failed"
	}
	if !slices.ContainsFunc(items, func(st string) bool { return st != ItemCanceled }) {
		return "canceled"
	}
	if slices.Contains(items, ItemSending) {
		return "sending" // an item in flight decides nothing yet
	}
	switch items[0] {
	case ItemPlanned:
		return "sending"
	case ItemPublished, ItemPartial:
	default:
		return "failed"
	}
	allPublished, failed, pending := true, false, false
	for _, st := range items {
		switch st {
		case ItemPlanned, ItemSending:
			pending = true
		case ItemFailed, ItemUnknown, ItemCanceled:
			failed = true
		}
		if st != ItemPublished {
			allPublished = false
		}
	}
	switch {
	case allPublished:
		return "success"
	case failed:
		return "partial"
	case pending:
		return "sending"
	}
	return "partial"
}

// MarkDue makes the given targets of a post due now: their unfinished items
// (planned, failed or unknown) get next_attempt_at = now, their give-up is
// cleared, and they and the post move to sending. A target with nothing
// unfinished is left as it is. Returns how many targets were made due. Used
// by a retry by hand.
func (s *Store) MarkDue(ctx context.Context, postID string, targetIDs []int64, now time.Time) (int, error) {
	if len(targetIDs) == 0 {
		return 0, nil
	}
	now = now.UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := readPostState(ctx, tx, postID)
	if err != nil {
		return 0, err
	}
	due := 0
	for _, id := range targetIDs {
		// Canceled items go back to planned: a retry by hand sends what a
		// cancel stopped.
		// A retry by hand is the owner's word to send: earlier attempts no
		// longer count as maybe-posted for the duplicate guard.
		res, err := tx.ExecContext(ctx, `UPDATE target_items
			SET next_attempt_at = ?, doubt_reset_at = ?, status = CASE WHEN status = 'canceled' THEN 'planned' ELSE status END
			WHERE target_id = ? AND status IN ('planned', 'failed', 'unknown', 'canceled')
			  AND target_id IN (SELECT id FROM post_targets WHERE post_id = ?)`, now, now, id, postID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		due++
		if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET gave_up_at = NULL, status = 'sending' WHERE id = ?`, id); err != nil {
			return 0, err
		}
	}
	if due > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET status = 'sending', canceled_at = NULL WHERE id = ?`, postID); err != nil {
			return 0, err
		}
		if err := emitPostChange(ctx, tx, postID, before, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if due > 0 {
		s.ev.fire()
	}
	return due, nil
}

// SendingItem is an item a stopped process left in flight.
type SendingItem struct {
	Claim
	SentAt time.Time
	// Committing: the publishing request was started (MarkCommitting).
	Committing bool
}

// MarkCommitting records that the item's publishing request is about to be
// sent. Only an item being sent is marked.
func (s *Store) MarkCommitting(ctx context.Context, itemID int64, at time.Time) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE target_items SET committing_at = ? WHERE id = ? AND status = 'sending'`,
		at.UTC(), itemID)
	return err
}

// SendingItems lists the items left sending. Only startup recovery may call
// it, before any worker runs.
func (s *Store) SendingItems(ctx context.Context) ([]SendingItem, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT ti.id, ti.target_id, ti.ordinal, ti.attempt_count, ti.idem_key, ti.next_attempt_at,
		  pt.post_id, pt.platform, pt.account_id, ti.committing_at IS NOT NULL
		FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
		WHERE ti.status = 'sending' ORDER BY ti.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SendingItem
	for rows.Next() {
		var si SendingItem
		var sent sql.NullTime
		if err := rows.Scan(&si.ItemID, &si.TargetID, &si.Ordinal, &si.Attempts, &si.IdemKey, &sent, &si.PostID, &si.Platform,
			&si.AccountID, &si.Committing); err != nil {
			return nil, err
		}
		if sent.Valid {
			si.SentAt = sent.Time.UTC()
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// ResetUnstarted returns scheduled posts that were claimed but had nothing
// sent (a process stopped between the claim and the first send) to
// scheduled, so the missed check applies to them. Startup only, after
// SendingItems' items are recorded.
func (s *Store) ResetUnstarted(ctx context.Context) (int, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	untouched := `NOT EXISTS (SELECT 1 FROM post_targets pt JOIN target_items ti ON ti.target_id = pt.id
		WHERE pt.post_id = posts.id AND (ti.attempt_count > 0 OR ti.status <> 'planned'))`
	rows, err := tx.QueryContext(ctx, `UPDATE posts SET status = 'scheduled'
		WHERE status = 'sending' AND scheduled_at IS NOT NULL AND `+untouched+` RETURNING id`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = 'scheduled' WHERE post_id = ?`, id); err != nil {
			return 0, err
		}
		if err := emit(ctx, tx, id, EventPostUpdated, EventData{Status: "scheduled"}, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		s.ev.fire()
	}
	return len(ids), nil
}

// MissedPost is a scheduled post marked missed.
type MissedPost struct {
	ID          string
	ScheduledAt time.Time
}

// MarkOverdueMissed marks scheduled posts due before cutoff missed: the
// post and its targets, with their items no longer due (a retry makes them
// due again).
func (s *Store) MarkOverdueMissed(ctx context.Context, cutoff time.Time) ([]MissedPost, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, scheduled_at FROM posts
		WHERE status = 'scheduled' AND scheduled_at < ? ORDER BY scheduled_at`, cutoff.UTC())
	if err != nil {
		return nil, err
	}
	var due []MissedPost
	for rows.Next() {
		var m MissedPost
		if err := rows.Scan(&m.ID, &m.ScheduledAt); err != nil {
			rows.Close()
			return nil, err
		}
		m.ScheduledAt = m.ScheduledAt.UTC()
		due = append(due, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []MissedPost
	for _, m := range due {
		ok, err := s.markMissed(ctx, m.ID)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) markMissed(ctx context.Context, id string) (bool, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := readPostState(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Guarded on 'scheduled': a post claimed since the query isn't missed.
	res, err := tx.ExecContext(ctx, `UPDATE posts SET status = 'missed' WHERE id = ? AND status = 'scheduled'`, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = 'missed' WHERE post_id = ? AND status = 'scheduled'`, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE target_items SET next_attempt_at = NULL
		WHERE status = 'planned' AND target_id IN (SELECT id FROM post_targets WHERE post_id = ?)`, id); err != nil {
		return false, err
	}
	if err := emitPostChange(ctx, tx, id, before, time.Now()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.ev.fire()
	return true, nil
}

// Unscheduled is a failed item with no retry time whose target hasn't
// given up: one recorded before the engine decided retries (the v4
// migration's), which the engine's policy then schedules or gives up.
type Unscheduled struct {
	Claim
	ErrorClass  string
	LastAttempt time.Time
}

// UnscheduledFailures lists failed and unknown items that have no retry
// time and whose target hasn't given up.
func (s *Store) UnscheduledFailures(ctx context.Context) ([]Unscheduled, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT ti.id, ti.target_id, ti.ordinal, ti.attempt_count, ti.idem_key, COALESCE(ti.error_class, ''),
		  pt.post_id, pt.platform, pt.account_id, pt.last_attempt_at
		FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
		WHERE ti.status IN ('failed', 'unknown') AND ti.next_attempt_at IS NULL AND pt.gave_up_at IS NULL
		ORDER BY ti.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unscheduled
	for rows.Next() {
		var u Unscheduled
		var la sql.NullTime
		if err := rows.Scan(&u.ItemID, &u.TargetID, &u.Ordinal, &u.Attempts, &u.IdemKey, &u.ErrorClass, &u.PostID, &u.Platform,
			&u.AccountID, &la); err != nil {
			return nil, err
		}
		if la.Valid {
			u.LastAttempt = la.Time.UTC()
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ScheduleItemRetry sets a failed or unknown item's retry time.
func (s *Store) ScheduleItemRetry(ctx context.Context, itemID int64, at time.Time) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE target_items SET next_attempt_at = ?
		WHERE id = ? AND status IN ('failed', 'unknown')`, at.UTC(), itemID)
	return err
}

// FirstUncertainAttempt is when the item was first attempted with a
// failure that may still have published, or nil: an unknown outcome, a
// transient failure (a 5xx can come after the server committed), or an
// unclassified failure from before error classes were recorded. Attempts
// before the owner last said to send the item again (doubt_reset_at) don't
// count.
func (s *Store) FirstUncertainAttempt(ctx context.Context, itemID int64) (*time.Time, error) {
	var at sql.NullTime
	// MIN over a TIMESTAMP column returns the stored text, which the driver
	// can't scan as a time; ORDER BY + LIMIT keeps the column's type.
	err := s.sql.QueryRowContext(ctx, `SELECT a.attempted_at FROM target_attempts a
		JOIN target_items ti ON ti.id = a.item_id
		WHERE a.item_id = ? AND (a.error_class IN ('unknown_outcome', 'transient')
		  OR (a.error_class IS NULL AND a.status NOT IN ('success', 'published')))
		  AND (ti.doubt_reset_at IS NULL OR a.attempted_at > ti.doubt_reset_at)
		ORDER BY a.attempted_at LIMIT 1`, itemID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !at.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := at.Time.UTC()
	return &t, nil
}

// PostStatus returns a post's status.
func (s *Store) PostStatus(ctx context.Context, id string) (string, error) {
	var st string
	err := s.sql.QueryRowContext(ctx, `SELECT status FROM posts WHERE id = ?`, id).Scan(&st)
	return st, err
}

// ErrNothingToCancel is returned by CancelPost when nothing of the post
// is still to be sent.
var ErrNothingToCancel = errors.New("nothing left to cancel")

// CancelPost stops what a post still has to send: its planned items (a
// scheduled post, or the rest of a thread) and the failed ones the engine
// would retry become canceled. Unknown items keep their status (they may
// be live) but are no longer retried. An item being sent right now
// finishes; a thread stops after it. Targets left failed or partial are
// marked given up, so the retrier leaves them; a retry by hand still
// works. The post ends up canceled when nothing went out, partial or
// failed otherwise. Returns ErrNothingToCancel when nothing was still to
// be sent, sql.ErrNoRows for no post.
func (s *Store) CancelPost(ctx context.Context, postID string, now time.Time) error {
	now = now.UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := readPostState(ctx, tx, postID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE target_items SET status = 'canceled', next_attempt_at = NULL
		WHERE target_id IN (SELECT id FROM post_targets WHERE post_id = ?)
		  AND (status = 'planned'
		       OR (status = 'failed' AND target_id IN (SELECT id FROM post_targets WHERE post_id = ? AND gave_up_at IS NULL)))
		RETURNING id, target_id, ordinal`, postID, postID)
	if err != nil {
		return err
	}
	type canceled struct {
		item, target int64
		ordinal      int
	}
	var done []canceled
	for rows.Next() {
		var c canceled
		if err := rows.Scan(&c.item, &c.target, &c.ordinal); err != nil {
			rows.Close()
			return err
		}
		done = append(done, c)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Unknown items still to be retried stop being retried.
	res, err := tx.ExecContext(ctx, `UPDATE target_items SET next_attempt_at = NULL
		WHERE status = 'unknown' AND target_id IN (SELECT id FROM post_targets WHERE post_id = ? AND gave_up_at IS NULL)`, postID)
	if err != nil {
		return err
	}
	unknown, _ := res.RowsAffected()
	if len(done) == 0 && unknown == 0 {
		return ErrNothingToCancel
	}
	targets := map[int64]bool{}
	for _, c := range done {
		var account, platform string
		if err := tx.QueryRowContext(ctx, `SELECT account_id, platform FROM post_targets WHERE id = ?`, c.target).
			Scan(&account, &platform); err != nil {
			return err
		}
		ord := c.ordinal
		if err := emit(ctx, tx, postID, EventItemCanceled, EventData{Status: ItemCanceled, TargetID: c.target,
			AccountID: account, Platform: platform, Ordinal: &ord}, now); err != nil {
			return err
		}
		targets[c.target] = true
	}
	for id := range targets {
		statuses, err := itemStatuses(ctx, tx, id)
		if err != nil {
			return err
		}
		// A target still sending its current item keeps 'sending' until
		// that item is recorded.
		if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = ? WHERE id = ?`, targetStatusOf(statuses), id); err != nil {
			return err
		}
	}
	// What is left failed or partial is not the retrier's to send, and an
	// item in flight won't be retried either (RecordItemResult reads
	// canceled_at). A retry by hand clears both.
	if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET gave_up_at = ?
		WHERE post_id = ? AND gave_up_at IS NULL AND status IN ('failed', 'partial')`, now, postID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posts SET canceled_at = ? WHERE id = ?`, now, postID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	if err := emitPostChange(ctx, tx, postID, before, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
	return nil
}
