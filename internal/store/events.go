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
	EventItemPublished = "item.published"
	EventItemPartial   = "item.partial"
	EventItemFailed    = "item.failed"
	EventItemUnknown   = "item.unknown"
	EventItemCanceled  = "item.canceled"
	EventDelivery      = "delivery.updated"
)

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
// the post's owner. Call s.ev.fire() after the commit.
func emit(ctx context.Context, tx execQuerier, postID, typ string, d EventData, at time.Time) error {
	b, _ := json.Marshal(d)
	_, err := tx.ExecContext(ctx, `INSERT INTO events (owner_id, post_id, type, data, created_at)
		VALUES ((SELECT COALESCE((SELECT owner_id FROM posts WHERE id = ?), ?)), ?, ?, ?, ?)`,
		postID, DefaultOwner, postID, typ, string(b), at.UTC())
	return err
}

// emitPostStatus records the post's status as a post.updated event, and a
// post.completed one when nothing is left to send.
func emitPostStatus(ctx context.Context, tx *sql.Tx, postID string, at time.Time) error {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM posts WHERE id = ?`, postID).Scan(&status); err != nil {
		return err
	}
	if err := emit(ctx, tx, postID, EventPostUpdated, EventData{Status: status}, at); err != nil {
		return err
	}
	if PostSettled(status) {
		return emit(ctx, tx, postID, EventPostCompleted, EventData{Status: status}, at)
	}
	return nil
}

// PostSettled reports whether a post status has nothing left to send.
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
	After   int64  // events with a larger id
	Limit   int
}

// EventsSince returns events after f.After, oldest first.
func (s *Store) EventsSince(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 500
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id, COALESCE(post_id, ''), type, data, created_at FROM events
		WHERE owner_id = ? AND id > ? AND (? = '' OR post_id = ?) ORDER BY id LIMIT ?`,
		f.OwnerID, f.After, f.PostID, f.PostID, f.Limit)
	if err != nil {
		return nil, err
	}
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
