package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotUnknown is returned by ResolveItem when the item's outcome is known.
var ErrNotUnknown = errors.New("store: the item's outcome is not unknown")

// Resolution is the owner's word on an item whose outcome is unknown (a
// send was in flight when the process stopped, on a destination that can't
// drop repeats).
type Resolution struct {
	PostID   string
	TargetID int64
	Ordinal  int
	// Published: the owner found the post. RemoteID, RemoteURL and Ref are
	// what is known of it (all may be empty). Not published: the item is
	// sent again.
	Published           bool
	RemoteID, RemoteURL string
	Ref                 json.RawMessage
	// Continue makes the target's later unsent items due, so a thread goes
	// on. Set it for a published item only when RemoteID is known: the next
	// item replies to it.
	Continue bool
	At       time.Time
}

// ResolveItem records a Resolution in one transaction: the item, its
// target's and post's status, and their events. An item not published is
// due at once, as is the rest of its thread when Continue is set; the
// target's give-up is cleared then. A canceled post sends nothing more.
func (s *Store) ResolveItem(ctx context.Context, r Resolution) error {
	at := r.At.UTC()
	if r.At.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var itemID int64
	var account, platform string
	var canceled bool
	err = tx.QueryRowContext(ctx, `SELECT ti.id, pt.account_id, pt.platform, p.canceled_at IS NOT NULL
		FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id JOIN posts p ON p.id = pt.post_id
		WHERE pt.post_id = ? AND pt.id = ? AND ti.ordinal = ? AND ti.status = ?`,
		r.PostID, r.TargetID, r.Ordinal, ItemUnknown).Scan(&itemID, &account, &platform, &canceled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotUnknown
	}
	if err != nil {
		return err
	}
	before, err := readPostState(ctx, tx, r.PostID)
	if err != nil {
		return err
	}
	var ref any
	if len(r.Ref) > 0 {
		ref = string(r.Ref)
	}
	evType, evStatus := EventItemPublished, ItemPublished
	if r.Published {
		_, err = tx.ExecContext(ctx, `UPDATE target_items SET status = ?, remote_id = ?, remote_url = ?,
			ref_json = COALESCE(?, ref_json), error_class = NULL, error = NULL, next_attempt_at = NULL,
			published_at = COALESCE(published_at, ?) WHERE id = ?`,
			ItemPublished, nullStr(r.RemoteID), nullStr(r.RemoteURL), ref, at, itemID)
	} else {
		evType, evStatus = EventItemFailed, ItemFailed
		var next any
		if !canceled {
			next = at
		}
		// attempt_count restarts: the owner says nothing went out, so the
		// engine's duplicate guard (an uncertain attempt older than the
		// destination remembers keys) no longer applies.
		_, err = tx.ExecContext(ctx, `UPDATE target_items SET status = ?, next_attempt_at = ?, attempt_count = 0,
			doubt_reset_at = ?, manual_due = 1 WHERE id = ?`,
			ItemFailed, next, at, itemID)
	}
	if err != nil {
		return err
	}
	if r.Published && !r.Continue {
		// Nothing to reply to: the rest of the thread can't go on under
		// this item, and must not be sent replying to nothing.
		if _, err := tx.ExecContext(ctx, `UPDATE target_items SET status = ?, next_attempt_at = NULL
			WHERE target_id = ? AND ordinal > ? AND status IN ('planned', 'failed')`, ItemCanceled, r.TargetID, r.Ordinal); err != nil {
			return err
		}
	}
	// due: something of the target is to be sent now.
	due := !canceled && !r.Published
	if !canceled && (!r.Published || r.Continue) {
		// The rest of the thread: what was planned, and failures the engine
		// had stopped retrying when it gave the target up.
		res, err := tx.ExecContext(ctx, `UPDATE target_items SET next_attempt_at = ?, manual_due = 1
			WHERE target_id = ? AND ordinal > ? AND status IN ('planned', 'failed')`, at, r.TargetID, r.Ordinal)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			due = true
		}
	}
	statuses, err := itemStatuses(ctx, tx, r.TargetID)
	if err != nil {
		return err
	}
	tstatus := targetStatusOf(statuses)
	switch {
	case due:
		tstatus = "sending"
	case tstatus == "sending":
		// Published, but the thread can't go on under it: what is left
		// stays unsent, for a :retry by hand.
		tstatus = "partial"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_targets SET status = ?,
		  gave_up_at = CASE WHEN ? THEN NULL ELSE gave_up_at END,
		  error_class = CASE WHEN ? THEN NULL ELSE error_class END WHERE id = ?`,
		tstatus, due, r.Published, r.TargetID); err != nil {
		return err
	}
	if err := recomputeStatus(tx, r.PostID); err != nil {
		return err
	}
	ord := r.Ordinal
	d := EventData{TargetID: r.TargetID, AccountID: account, Platform: platform, Ordinal: &ord, Status: evStatus}
	if r.Published {
		d.RemoteURL = r.RemoteURL
	} else if canceled {
		d.ErrorClass, d.Error = "unknown_outcome", "not published, as the owner confirmed; the post is canceled, so it isn't sent"
	} else {
		d.ErrorClass, d.Error = "unknown_outcome", "not published, as the owner confirmed; sending it again"
	}
	if err := emit(ctx, tx, r.PostID, evType, d, at); err != nil {
		return err
	}
	if err := emitPostChange(ctx, tx, r.PostID, before, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
	return nil
}
