package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// The event log: one row per change of a post's state, written in the
// transaction that makes the change. GET /v1/events and
// /v1/posts/{id}/events stream it, and a client that reconnects with
// Last-Event-ID resumes where it left off. It is also the shape of future
// webhooks.

// Event types.
const (
	EventPostCreated   = "post.created"
	EventPostUpdated   = "post.updated"   // status, schedule or visibility changed
	EventPostCompleted = "post.completed" // nothing left to send (until a retry)
	EventItemSending   = "item.sending"
	EventItemWaiting   = "item.waiting" // put back unsent: its account is locked
	EventItemPublished = "item.published"
	EventItemPartial   = "item.partial"
	EventItemFailed    = "item.failed"
	EventItemUnknown   = "item.unknown"
	EventItemCanceled  = "item.canceled"
	EventDelivery      = "delivery.updated"
	// EventAccountUpdated: the owner changed an account's settings. It
	// belongs to no post; its data names the fields, never their values.
	EventAccountUpdated = "account.updated"

	// Sign-in events: who signed in or failed to, from where, and changes
	// to the ways of signing in and to tokens. They belong to no post and
	// are read through GET /v1/me/sign-in-events, never streamed.
	EventLoginSucceeded = "login.succeeded"
	EventLoginFailed    = "login.failed"
	EventSignInAdded    = "signin.added"
	EventSignInRemoved  = "signin.removed"
	EventSignInChanged  = "signin.changed" // the password was changed
	EventTokenCreated   = "token.created"
)

// signInEventTypes are the sign-in events, kept out of the event stream.
const signInEventTypes = `('login.succeeded', 'login.failed', 'signin.added', 'signin.removed', 'signin.changed', 'token.created')`

// EventRetention is how long events are kept.
const EventRetention = 7 * 24 * time.Hour

// Event is one row of the log. Data holds store ids and the store's status
// vocabulary; the API maps both.
type Event struct {
	ID        int64
	OwnerID   string
	PostID    string
	Type      string
	Data      EventData
	CreatedAt time.Time
}

// EventData is an event's payload. Fields are set as the type needs.
type EventData struct {
	Status      string     `json:"status,omitempty"`
	TargetID    int64      `json:"target_id,omitempty"`
	AccountID   string     `json:"account_id,omitempty"`
	Platform    string     `json:"platform,omitempty"`
	Ordinal     *int       `json:"ordinal,omitempty"`
	RemoteURL   string     `json:"remote_url,omitempty"`
	ErrorClass  string     `json:"error_class,omitempty"`
	Error       string     `json:"error,omitempty"`
	Endpoint    string     `json:"endpoint,omitempty"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	Hidden      *bool      `json:"hidden,omitempty"`
	// Fields are the names of what an account event changed.
	Fields []string `json:"fields,omitempty"`
	// Actor and TokenID say who made an account change.
	Actor   string `json:"actor,omitempty"`
	TokenID string `json:"token_id,omitempty"`
	// Sign-in events: how (passkey, password, oidc, setup), from which
	// client address, the passkey's id, a token's scopes. Never a secret.
	Method   string   `json:"method,omitempty"`
	Addr     string   `json:"addr,omitempty"`
	Passkey  string   `json:"passkey,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	Sessions int      `json:"sessions_ended,omitempty"`
}

// eventSignal wakes stream readers when events are written. Readers also
// poll, so a wake that races a commit costs at most one poll interval.
type eventSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (e *eventSignal) wait() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ch == nil {
		e.ch = make(chan struct{})
	}
	return e.ch
}

func (e *eventSignal) fire() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ch != nil {
		close(e.ch)
		e.ch = nil
	}
}

// EventsChanged returns a channel closed at the next event write.
func (s *Store) EventsChanged() <-chan struct{} { return s.ev.wait() }

type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// emit writes an event for postID in the caller's transaction, owned by
// the post's owner. The post must exist (a missing one fails the insert, so
// no event can land on the wrong owner). Call s.ev.fire() after the commit.
func emit(ctx context.Context, tx execQuerier, postID, typ string, d EventData, at time.Time) error {
	b, _ := json.Marshal(d)
	_, err := tx.ExecContext(ctx, `INSERT INTO events (owner_id, post_id, type, data, created_at)
		VALUES ((SELECT owner_id FROM posts WHERE id = ?), ?, ?, ?, ?)`,
		postID, postID, typ, string(b), at.UTC())
	return err
}

// emitOwner writes an event that belongs to no post (an account change)
// in the caller's transaction. Call s.ev.fire() after the commit.
func emitOwner(ctx context.Context, tx execQuerier, owner, typ string, d EventData, at time.Time) error {
	b, _ := json.Marshal(d)
	_, err := tx.ExecContext(ctx, `INSERT INTO events (owner_id, post_id, type, data, created_at) VALUES (?, NULL, ?, ?, ?)`,
		owner, typ, string(b), at.UTC())
	return err
}

type ctxQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// postState is a post's status and whether it is settled: nothing left to
// send, an automatic retry included.
type postState struct {
	Status  string
	Settled bool
}

// retryPendingExpr is true for a post (aliased p) with an item the engine
// will still try on its own.
const retryPendingExpr = `EXISTS (SELECT 1 FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
	WHERE pt.post_id = p.id AND pt.gave_up_at IS NULL AND ti.next_attempt_at IS NOT NULL
	  AND ti.status IN ('planned', 'failed', 'unknown'))`

func readPostState(ctx context.Context, q ctxQuerier, postID string) (postState, error) {
	var st postState
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT p.status, `+retryPendingExpr+` FROM posts p WHERE p.id = ?`, postID).
		Scan(&st.Status, &pending)
	st.Settled = err == nil && PostSettled(st.Status) && !pending
	return st, err
}

// emitPostChange logs how a post moved from before: post.updated when its
// status changed, post.completed when it has just settled.
func emitPostChange(ctx context.Context, tx *sql.Tx, postID string, before postState, at time.Time) error {
	now, err := readPostState(ctx, tx, postID)
	if err != nil {
		return err
	}
	if now.Status != before.Status {
		if err := emit(ctx, tx, postID, EventPostUpdated, EventData{Status: now.Status}, at); err != nil {
			return err
		}
	}
	if now.Settled && (!before.Settled || now.Status != before.Status) {
		return emit(ctx, tx, postID, EventPostCompleted, EventData{Status: now.Status}, at)
	}
	return nil
}

// PostState reports a post's status and whether it is settled.
func (s *Store) PostState(ctx context.Context, id string) (status string, settled bool, err error) {
	st, err := readPostState(ctx, s.sql, id)
	return st.Status, st.Settled, err
}

// PostSettled reports whether a post status has nothing left to send
// (ignoring automatic retries; see PostState).
func PostSettled(status string) bool {
	switch status {
	case "sending", "scheduled", "cancelling", "pending":
		return false
	}
	return true
}

// EventFilter narrows EventsSince.
type EventFilter struct {
	OwnerID string
	PostID  string // "" for all the owner's posts
	// PostsOnly leaves out events that belong to no post (account
	// changes), for a caller that may not read accounts.
	PostsOnly bool
	After     int64 // events with a larger id
	Limit     int
}

// EventsSince returns events after f.After, oldest first.
func (s *Store) EventsSince(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 500
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id, COALESCE(post_id, ''), type, data, created_at FROM events
		WHERE owner_id = ? AND id > ? AND (? = '' OR post_id = ?) AND (? = 0 OR post_id IS NOT NULL)
		  AND type NOT IN `+signInEventTypes+` ORDER BY id LIMIT ?`,
		f.OwnerID, f.After, f.PostID, f.PostID, f.PostsOnly, f.Limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.ID, &e.OwnerID, &e.PostID, &e.Type, &data, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(data), &e.Data)
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// maxFailedLoginEvents bounds login.failed events: at most this many in
// ten minutes are recorded (failures beyond are still logged and
// throttled), so someone guessing from many addresses can't fill the
// database.
const maxFailedLoginEvents = 100

// AddSignInEvent records a sign-in event for the owner.
func (s *Store) AddSignInEvent(ctx context.Context, owner, typ string, d EventData) error {
	b, _ := json.Marshal(d)
	now := time.Now().UTC()
	_, err := s.sql.ExecContext(ctx, `INSERT INTO events (owner_id, post_id, type, data, created_at)
		SELECT ?, NULL, ?, ?, ? WHERE ? != 'login.failed'
		   OR (SELECT COUNT(*) FROM events WHERE type = 'login.failed' AND created_at > ?) < ?`,
		owner, typ, string(b), now, typ, now.Add(-10*time.Minute), maxFailedLoginEvents)
	return err
}

// SignInEvents are the owner's newest sign-in events, newest first.
func (s *Store) SignInEvents(ctx context.Context, owner string, limit int) ([]Event, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id, COALESCE(post_id, ''), type, data, created_at FROM events
		WHERE owner_id = ? AND type IN `+signInEventTypes+` ORDER BY id DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// LastEventID is the newest event's id (0 when there are none).
func (s *Store) LastEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.sql.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&id)
	return id.Int64, err
}

// PurgeEvents deletes events older than EventRetention before now.
func (s *Store) PurgeEvents(ctx context.Context, now time.Time) (int, error) {
	res, err := s.sql.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, now.Add(-EventRetention).UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
