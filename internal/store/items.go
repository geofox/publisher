package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Item statuses. An item is one post a target publishes: a single post is a
// one-item target, a thread has one item per part.
const (
	ItemPlanned   = "planned"   // not sent yet
	ItemSending   = "sending"   // a publish is in flight
	ItemPublished = "published" // live
	ItemPartial   = "partial"   // live, but some endpoints or a follow-up write failed
	ItemFailed    = "failed"    // the last attempt failed
	ItemUnknown   = "unknown"   // a publish was in flight when the process stopped
	ItemCanceled  = "canceled"
)

// Item is one target_items row.
type Item struct {
	ID            int64
	Ordinal       int
	Text          string
	Media         []int // indices into the post's media
	Status        string
	IdemKey       string
	RemoteID      string
	RemoteURL     string
	Ref           json.RawMessage // destination.ItemRef.Data
	ErrorClass    string
	Error         string
	AttemptCount  int
	NextAttemptAt *time.Time
	PublishedAt   *time.Time
	Deliveries    []RelayState
}

// ItemKey is the idempotency key of item ordinal of a post's target of type
// typ. It is derived, not random, so the key a destination saw before an
// upgrade is the key the item keeps after it.
func ItemKey(postID, typ string, ordinal int) string {
	sum := sha256.Sum256([]byte("publisher\x00" + postID + "\x00" + typ + "\x00" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(sum[:16])
}

// itemContent is target_items.content_json.
type itemContent struct {
	Text  string `json:"text"`
	Media []int  `json:"media,omitempty"`
}

// itemStatusOf maps the target and segment vocabulary (success, pending,
// scheduled, missed) onto item statuses. Item statuses pass through.
func itemStatusOf(s string) string {
	switch s {
	case "success":
		return ItemPublished
	case "pending", "scheduled", "missed", "":
		return ItemPlanned
	}
	return s
}

// segmentStatusOf maps an item status back onto the segment vocabulary the
// /api routes return.
func segmentStatusOf(s string) string {
	switch s {
	case ItemPublished:
		return "success"
	case ItemPlanned, ItemSending:
		return "pending"
	case ItemUnknown:
		return "failed"
	}
	return s
}

// itemExec is what item writes need: a *sql.Tx or the migration connection.
type itemExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertItem writes one item, minting an idempotency key when it has none.
func insertItem(ctx context.Context, x itemExec, targetID int64, it Item) error {
	_, err := insertItemID(ctx, x, targetID, it)
	return err
}

func insertItemID(ctx context.Context, x itemExec, targetID int64, it Item) (int64, error) {
	content, _ := json.Marshal(itemContent{Text: it.Text, Media: it.Media})
	if it.IdemKey == "" {
		it.IdemKey = newID()
	}
	res, err := x.ExecContext(ctx, `INSERT INTO target_items (target_id, ordinal, content_json, status, idem_key,
		remote_id, remote_url, ref_json, error_class, error, attempt_count, next_attempt_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		targetID, it.Ordinal, string(content), it.Status, it.IdemKey,
		nullStr(it.RemoteID), nullStr(it.RemoteURL), nullStr(string(it.Ref)), nullStr(it.ErrorClass), nullStr(it.Error),
		it.AttemptCount, it.NextAttemptAt, it.PublishedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// loadItems reads a target's items in ordinal order, with their deliveries.
func (s *Store) loadItems(targetID int64) ([]Item, error) {
	rows, err := s.sql.Query(`SELECT id, ordinal, content_json, status, idem_key, COALESCE(remote_id, ''), COALESCE(remote_url, ''),
		COALESCE(ref_json, ''), COALESCE(error_class, ''), COALESCE(error, ''), attempt_count, next_attempt_at, published_at
		FROM target_items WHERE target_id = ? ORDER BY ordinal`, targetID)
	if err != nil {
		return nil, err
	}
	var items []Item
	for rows.Next() {
		var it Item
		var content, ref string
		var next, pub sql.NullTime
		if err := rows.Scan(&it.ID, &it.Ordinal, &content, &it.Status, &it.IdemKey, &it.RemoteID, &it.RemoteURL,
			&ref, &it.ErrorClass, &it.Error, &it.AttemptCount, &next, &pub); err != nil {
			rows.Close()
			return nil, err
		}
		var c itemContent
		_ = json.Unmarshal([]byte(content), &c)
		it.Text, it.Media = c.Text, c.Media
		if ref != "" {
			it.Ref = json.RawMessage(ref)
		}
		if next.Valid {
			t := next.Time.UTC()
			it.NextAttemptAt = &t
		}
		if pub.Valid {
			t := pub.Time.UTC()
			it.PublishedAt = &t
		}
		items = append(items, it)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range items {
		d, err := s.loadDeliveries(items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].Deliveries = d
	}
	return items, nil
}

func (s *Store) loadDeliveries(itemID int64) ([]RelayState, error) {
	rows, err := s.sql.Query(`SELECT endpoint, status, message, gave_up_at, retry_count, attempted_at
		FROM item_deliveries WHERE item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelayState
	for rows.Next() {
		var rs RelayState
		var msg sql.NullString
		var gu, at sql.NullTime
		if err := rows.Scan(&rs.URL, &rs.Status, &msg, &gu, &rs.RetryCount, &at); err != nil {
			return nil, err
		}
		rs.Message = msg.String
		if gu.Valid {
			t := gu.Time.UTC()
			rs.GaveUpAt = &t
		}
		if at.Valid {
			rs.AttemptedAt = at.Time.UTC()
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// upsertDeliveries records endpoint outcomes for an item.
func upsertDeliveries(ctx context.Context, x itemExec, itemID int64, relays []RelayState, at time.Time) error {
	for _, rl := range relays {
		if _, err := x.ExecContext(ctx, `INSERT INTO item_deliveries (item_id, endpoint, status, message, attempted_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (item_id, endpoint) DO UPDATE SET status = excluded.status, message = excluded.message,
			  attempted_at = excluded.attempted_at`,
			itemID, rl.URL, rl.Status, rl.Message, at); err != nil {
			return err
		}
	}
	return nil
}

// targetView fills the /api shape of a target from its items: the first
// item's remote id, URL and endpoint rows; Segments when it is a thread; and
// SignedEventJSON (the first item's ref) for targets that deliver to
// endpoints.
func targetView(tg *Target, items []Item) {
	tg.Items = items
	if len(items) == 0 {
		return
	}
	head := items[0]
	tg.RemoteID, tg.RemoteURL, tg.Relays = head.RemoteID, head.RemoteURL, head.Deliveries
	if len(head.Deliveries) > 0 {
		tg.SignedEventJSON = string(head.Ref)
	}
	if len(items) > 1 {
		tg.Segments = make([]Segment, len(items))
		for i, it := range items {
			tg.Segments[i] = Segment{Ordinal: it.Ordinal, Text: it.Text, RemoteID: it.RemoteID, RemoteURL: it.RemoteURL,
				Ref: it.Ref, Status: segmentStatusOf(it.Status), Error: it.Error, Images: it.Media}
		}
	}
}

// itemsOf derives a target's items from the /api shape: one per segment for
// a thread, else one carrying the target's own text, result and ref.
func itemsOf(tg Target, mediaCount int) []Item {
	if len(tg.Segments) > 0 {
		items := make([]Item, len(tg.Segments))
		for i, sg := range tg.Segments {
			items[i] = Item{Ordinal: i, Text: sg.Text, Media: sg.Images, Status: itemStatusOf(sg.Status),
				RemoteID: sg.RemoteID, RemoteURL: sg.RemoteURL, Ref: segmentRef(sg), Error: sg.Error}
		}
		if len(items[0].Ref) == 0 && tg.SignedEventJSON != "" {
			items[0].Ref = json.RawMessage(tg.SignedEventJSON)
		}
		items[0].Deliveries = tg.Relays
		return items
	}
	it := Item{Ordinal: 0, Text: tg.FinalText, Media: allMedia(mediaCount), Status: itemStatusOf(tg.Status),
		RemoteID: tg.RemoteID, RemoteURL: tg.RemoteURL, ErrorClass: tg.ErrorClass, Deliveries: tg.Relays}
	if tg.SignedEventJSON != "" {
		it.Ref = json.RawMessage(tg.SignedEventJSON)
	}
	if len(tg.Attempts) > 0 {
		it.AttemptCount = len(tg.Attempts)
		it.Error = tg.Attempts[len(tg.Attempts)-1].Error
	}
	return []Item{it}
}

// segmentRef is a segment's ref, with a pre-Ref segment's Bluesky CID turned
// into the Bluesky driver's ref data ({"cid": ...}).
func segmentRef(sg Segment) json.RawMessage {
	if len(sg.Ref) == 0 && sg.CID != "" {
		b, _ := json.Marshal(map[string]string{"cid": sg.CID})
		return b
	}
	return sg.Ref
}
