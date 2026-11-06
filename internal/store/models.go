package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Interaction records that a Post is a reply/repost/quote of an external source.
type Interaction struct {
	Action         string `json:"action"` // reply|repost|quote
	SourcePlatform string `json:"source_platform"`
	SourceURL      string `json:"source_url"`
	SourceAuthor   string `json:"source_author"`
	// Ref is the source post's platform-native reference (dispatch.InteractRef
	// as JSON), kept so a retry can repeat the action. Empty on rows recorded
	// before it existed.
	Ref json.RawMessage `json:"ref,omitempty"`
}

type Post struct {
	ID           string     `json:"id"`
	CreatedAt    time.Time  `json:"created_at"`
	MasterText   string     `json:"master_text"`
	Platforms    []string   `json:"platforms"`
	DelaySeconds int        `json:"delay_seconds"`
	Source       string     `json:"source"`
	Status       string     `json:"status"`
	ScheduledAt  *time.Time `json:"scheduled_at,omitempty"`
	FiredAt      *time.Time `json:"fired_at,omitempty"` // list view: latest target attempt time (actual publish/retry)
	// FirstSuccessAt is the earliest time the post went live on ANY platform
	// (MIN over successful attempts). Set only by PublicFeed, never serialized.
	// Retries append later attempt rows, so this never moves once set.
	FirstSuccessAt *time.Time `json:"-"`
	OwnerID        string     `json:"-"`
	Hidden         bool       `json:"-"`
	// Attention: settled with an item in an unknown state or a target the
	// engine gave up on (set by GetPost and ListPostsPage).
	Attention   bool         `json:"-"`
	Targets     []Target     `json:"targets,omitempty"`
	Media       []Media      `json:"media,omitempty"`
	Interaction *Interaction `json:"interaction,omitempty"`
}

// Target is one account a post publishes to. Its items are the posts it
// makes (one, or one per thread part); RemoteID, RemoteURL, Relays,
// Segments and SignedEventJSON are the /api view of those items (see
// targetView).
type Target struct {
	ID              int64        `json:"id"`
	AccountID       string       `json:"account_id,omitempty"`
	Platform        string       `json:"platform"`
	FinalText       string       `json:"final_text"`
	FieldsJSON      string       `json:"fields_json"`
	Status          string       `json:"status"`
	ErrorClass      string       `json:"error_class,omitempty"` // of the latest attempt
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
	Items           []Item       `json:"-"`
}

type Attempt struct {
	AttemptNo    int       `json:"attempt_no"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	ErrorClass   string    `json:"error_class,omitempty"`
	LatencyMS    int       `json:"latency_ms"`
	RemoteID     string    `json:"remote_id"`
	RequestJSON  string    `json:"request_json"`
	ResponseJSON string    `json:"response_json"`
	AttemptedAt  time.Time `json:"attempted_at"`
}

type Media struct {
	Ordinal      int    `json:"ordinal"`
	BlossomURL   string `json:"blossom_url"`
	SHA256       string `json:"sha256"`
	Mime         string `json:"mime"`
	Dim          string `json:"dim"`
	Blurhash     string `json:"blurhash"`
	SizeBytes    int64  `json:"size_bytes"`
	Alt          string `json:"alt"`
	DurationSecs int64  `json:"duration_secs,omitempty"` // video only; 0 for images
	PosterURL    string `json:"poster_url,omitempty"`    // video poster JPEG; "" for images/legacy rows
	ClientID     string `json:"client_id,omitempty"`     // stable per-attachment id; drafts only (anchor key)
}

type RelayState struct {
	URL         string     `json:"url"`
	Status      string     `json:"status"` // ok | failed | skipped
	Message     string     `json:"message,omitempty"`
	GaveUpAt    *time.Time `json:"gave_up_at,omitempty"`
	RetryCount  int        `json:"retry_count,omitempty"`
	AttemptedAt time.Time  `json:"attempted_at,omitempty"`
}

// Segment is one post in a platform's reply-chain. A non-threaded target has no
// segments; a threaded one has an ordered slice (ordinal 0 = the chain head).
type Segment struct {
	Ordinal   int    `json:"ordinal"`
	Text      string `json:"text"`
	RemoteID  string `json:"remote_id,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	// Ref is the destination's private reference data for the segment
	// (destination.ItemRef.Data), needed to reply to it. It is stored with
	// the item but never sent to API clients (for Nostr it is the signed
	// event, kept off the wire like Target.SignedEventJSON).
	Ref json.RawMessage `json:"-"`
	// CID is the Bluesky record CID of segments written before Ref existed.
	// Only the v4 migration reads it (into the item's ref).
	CID    string `json:"cid,omitempty"`
	Status string `json:"status"` // success | failed | pending
	Error  string `json:"error,omitempty"`
	Images []int  `json:"images,omitempty"` // attachment indices this post carries (placement plan)
}

// storedSegment is a Segment as pre-v4 segments_json held it: with its Ref.
// Only the v4 migration reads it.
type storedSegment struct {
	Segment
	Ref json.RawMessage `json:"ref,omitempty"`
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
			`INSERT INTO media(post_id,ordinal,blossom_url,sha256,mime,dim,blurhash,size_bytes,alt,duration_secs,poster_url)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, m.Ordinal, m.BlossomURL, m.SHA256, m.Mime, m.Dim, m.Blurhash, m.SizeBytes, m.Alt, m.DurationSecs, m.PosterURL,
		); err != nil {
			return err
		}
	}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tg := range p.Targets {
		accountID := tg.AccountID
		if accountID == "" {
			if accountID, err = envAccountFor(ctx, tx, DefaultOwner, tg.Platform); err != nil {
				return err
			}
		}
		var lastAttempt any // a target planned and not yet sent has none
		if len(tg.Attempts) > 0 {
			lastAttempt = now
		}
		res, err := tx.Exec(
			`INSERT INTO post_targets(post_id,account_id,platform,final_text,fields_json,status,latency_ms,attempt_count,last_attempt_at,error_class)
			 VALUES(?,?,?,?,?,?,?,?,?,?)`,
			p.ID, accountID, tg.Platform, tg.FinalText, tg.FieldsJSON, tg.Status,
			tg.LatencyMS, len(tg.Attempts), lastAttempt, nullStr(tg.ErrorClass),
		)
		if err != nil {
			return err
		}
		tid, _ := res.LastInsertId()
		items, derived := tg.Items, tg.Items == nil
		if derived {
			items = itemsOf(tg, len(p.Media))
		}
		var headID int64
		for i, it := range items {
			if it.IdemKey == "" {
				it.IdemKey = LegacyItemKey(p.ID, tg.Platform, it.Ordinal)
			}
			// Planned items the caller passed carry their own due time (nil:
			// not until a retry). Derived ones are due at the post's time, or now.
			if derived && it.NextAttemptAt == nil && it.Status == ItemPlanned && p.Status != "missed" {
				due := now
				if p.ScheduledAt != nil {
					due = p.ScheduledAt.UTC()
				}
				it.NextAttemptAt = &due
			}
			if it.PublishedAt == nil && (it.Status == ItemPublished || it.Status == ItemPartial) {
				it.PublishedAt = &now
			}
			id, err := insertItemID(ctx, tx, tid, it)
			if err != nil {
				return err
			}
			if err := upsertDeliveries(ctx, tx, id, it.Deliveries, now); err != nil {
				return err
			}
			if i == 0 {
				headID = id
			}
		}
		var attemptItem any // a single post's attempts are its item's; a thread's span items
		if len(items) == 1 {
			attemptItem = headID
		}
		for _, at := range tg.Attempts {
			if _, err = tx.Exec(
				`INSERT INTO target_attempts(target_id,attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at,error_class,item_id)
				 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				tid, at.AttemptNo, at.Status, Scrub(at.Error), at.LatencyMS, at.RemoteID, Scrub(at.RequestJSON), Scrub(at.ResponseJSON), at.AttemptedAt, nullStr(at.ErrorClass), attemptItem,
			); err != nil {
				return err
			}
		}
	}
	if err := emit(ctx, tx, p.ID, EventPostCreated, EventData{Status: p.Status, ScheduledAt: p.ScheduledAt}, now); err != nil {
		return err
	}
	if st, err := readPostState(ctx, tx, p.ID); err != nil {
		return err
	} else if st.Settled {
		// Recorded already finished (an interaction answered inline).
		if err := emit(ctx, tx, p.ID, EventPostCompleted, EventData{Status: p.Status}, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
	return nil
}

func (s *Store) GetPost(id string) (*Post, error) {
	p := &Post{}
	var platforms string
	var schedAt sql.NullTime
	var interactionJSON sql.NullString // NULL on rows that predate the column migration
	err := s.sql.QueryRow(
		`SELECT id,created_at,master_text,platforms,delay_seconds,source,status,scheduled_at,interaction_json,owner_id,hidden,`+attentionExpr+`
		   FROM posts p WHERE id=?`, id,
	).Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.DelaySeconds, &p.Source, &p.Status, &schedAt, &interactionJSON,
		&p.OwnerID, &p.Hidden, &p.Attention)
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

	mrows, err := s.sql.Query(`SELECT ordinal,blossom_url,sha256,mime,dim,blurhash,size_bytes,alt,COALESCE(duration_secs,0),COALESCE(poster_url,'') FROM media WHERE post_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m Media
		if err := mrows.Scan(&m.Ordinal, &m.BlossomURL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes, &m.Alt, &m.DurationSecs, &m.PosterURL); err != nil {
			return nil, err
		}
		p.Media = append(p.Media, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	trows, err := s.sql.Query(`SELECT id,account_id,platform,final_text,fields_json,status,latency_ms,attempt_count,last_attempt_at,gave_up_at,COALESCE(error_class,'') FROM post_targets WHERE post_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var tg Target
		var fields, ftext sql.NullString
		var la, gu sql.NullTime
		var lat sql.NullInt64
		if err := trows.Scan(&tg.ID, &tg.AccountID, &tg.Platform, &ftext, &fields, &tg.Status, &lat, &tg.AttemptCount, &la, &gu, &tg.ErrorClass); err != nil {
			return nil, err
		}
		tg.FieldsJSON, tg.FinalText, tg.LatencyMS = fields.String, ftext.String, int(lat.Int64)
		if la.Valid {
			tg.LastAttempt = la.Time.UTC()
		}
		if gu.Valid {
			t := gu.Time.UTC()
			tg.GaveUpAt = &t
		}
		p.Targets = append(p.Targets, tg)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	trows.Close()
	for i := range p.Targets {
		items, err := s.loadItems(p.Targets[i].ID)
		if err != nil {
			return nil, err
		}
		targetView(&p.Targets[i], items)
	}
	for i := range p.Targets {
		arows, err := s.sql.Query(`SELECT attempt_no,status,error,latency_ms,remote_id,request_json,response_json,attempted_at,COALESCE(error_class,'') FROM target_attempts WHERE target_id=? ORDER BY attempt_no`, p.Targets[i].ID)
		if err != nil {
			return nil, err
		}
		for arows.Next() {
			var a Attempt
			var e, rid, req, resp sql.NullString
			if err := arows.Scan(&a.AttemptNo, &a.Status, &e, &a.LatencyMS, &rid, &req, &resp, &a.AttemptedAt, &a.ErrorClass); err != nil {
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
	return p, nil
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx.
type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

func recomputeStatus(q rowQuerier, postID string) error {
	rows, err := q.Query(`SELECT status FROM post_targets WHERE post_id=?`, postID)
	if err != nil {
		return err
	}
	defer rows.Close()
	total, succ, fail, open, canc, missed := 0, 0, 0, 0, 0, 0
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
		case "sending", "scheduled", "pending":
			open++ // still to be sent
		case "canceled":
			canc++
		case "missed":
			missed++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	status := "partial"
	switch {
	case open > 0:
		status = "sending"
	case total > 0 && canc == total:
		status = "canceled"
	case total > 0 && missed == total:
		status = "missed"
	case total == 0 || (succ == 0 && fail > 0 && fail+canc+missed == total):
		status = "failed"
	case succ == total:
		status = "success"
	}
	_, err = q.Exec(`UPDATE posts SET status=? WHERE id=?`, status, postID)
	return err
}

// recomputeTargetStatus derives a single-item target's status from its
// item's endpoint rows (skipped rows ignored): success = all attempted ok,
// failed = none ok, partial = some ok. The item follows. No-op for targets
// without endpoint rows (non-Nostr).
func recomputeTargetStatus(q rowQuerier, targetID int64) error {
	rows, err := q.Query(`SELECT d.status FROM item_deliveries d JOIN target_items ti ON ti.id = d.item_id
		WHERE ti.target_id=? AND ti.ordinal=0`, targetID)
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
	// A thread with a part still unpublished keeps its status: the head's
	// relays alone can't make it a success.
	var unsent int
	if err := q.QueryRow(`SELECT COUNT(*) FROM target_items WHERE target_id=? AND ordinal>0 AND status<>?`,
		targetID, ItemPublished).Scan(&unsent); err != nil {
		return err
	}
	if unsent > 0 {
		return nil
	}
	// All-skipped (e.g. only an unreachable .onion) → nothing was delivered → failed.
	status := "partial"
	switch {
	case ok == 0:
		status = "failed"
	case ok == attempted:
		status = "success"
	}
	if _, err = q.Exec(`UPDATE post_targets SET status=? WHERE id=?`, status, targetID); err != nil {
		return err
	}
	// A thread's head stays as it was: its other items decide the target.
	_, err = q.Exec(`UPDATE target_items SET status=? WHERE target_id=? AND ordinal=0
		AND (SELECT COUNT(*) FROM target_items x WHERE x.target_id=?) = 1`, itemStatusOf(status), targetID, targetID)
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
		`UPDATE item_deliveries
		    SET status=?, message=?, attempted_at=?, retry_count = retry_count + 1,
		        gave_up_at = CASE WHEN ?='ok' THEN NULL ELSE gave_up_at END
		  WHERE endpoint=? AND item_id=(SELECT id FROM target_items WHERE target_id=? AND ordinal=0)`,
		status, Scrub(message), time.Now().UTC(), status, relayURL, targetID,
	); err != nil {
		return err
	}
	if err := recomputeTargetStatus(tx, targetID); err != nil {
		return err
	}
	ctx, now := context.Background(), time.Now()
	var postID, account, platform string
	if err := tx.QueryRow(`SELECT pt.post_id, pt.account_id, pt.platform FROM post_targets pt WHERE pt.id=?`, targetID).
		Scan(&postID, &account, &platform); err != nil {
		return err
	}
	before, err := readPostState(ctx, tx, postID)
	if err != nil {
		return err
	}
	if err := recomputeStatus(tx, postID); err != nil {
		return err
	}
	zero := 0
	if err := emit(ctx, tx, postID, EventDelivery, EventData{TargetID: targetID, AccountID: account, Platform: platform,
		Ordinal: &zero, Status: status, Endpoint: relayURL, Error: Scrub(message)}, now); err != nil {
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

// nullStr stores "" as NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt stores 0 as NULL.
func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// ErrNotPending is returned by cancel/reschedule when a post is not 'scheduled'.
var ErrNotPending = errors.New("post is not pending")

// ErrNotHideable is returned by HidePost when the post is missing or still
// pending (scheduled/sending) — pending posts must be cancelled, not hidden.
var ErrNotHideable = errors.New("post cannot be hidden")

// HidePost soft-deletes a terminal post: it stays in the archive (data, retry
// history, backups) but is excluded from the history list. Pending posts
// (scheduled/sending) can't be hidden.
func (s *Store) HidePost(id string) error { return s.SetPostHidden(id, true) }

// SetPostHidden hides or shows a terminal post again. Hiding a pending post
// is ErrNotHideable; showing one that isn't hidden is a no-op.
func (s *Store) SetPostHidden(id string, hidden bool) error {
	ctx := context.Background()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := `UPDATE posts SET hidden=1 WHERE id=? AND hidden=0 AND status NOT IN ('scheduled','sending','cancelling','pending')`
	if !hidden {
		q = `UPDATE posts SET hidden=0 WHERE id=? AND hidden=1`
	}
	res, err := tx.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var h bool
		var status string
		err := tx.QueryRowContext(ctx, `SELECT hidden, status FROM posts WHERE id=?`, id).Scan(&h, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotHideable
		case err != nil:
			return err
		case h == hidden:
			return nil // already so
		}
		return ErrNotHideable
	}
	if err := emit(ctx, tx, id, EventPostUpdated, EventData{Hidden: &hidden}, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
	return nil
}

// RescheduleScheduled changes a pending post's time, and its planned items'
// with it; ErrNotPending otherwise.
func (s *Store) RescheduleScheduled(id string, at time.Time) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE posts SET scheduled_at=? WHERE id=? AND status='scheduled'`, at.UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	if _, err := tx.Exec(`UPDATE target_items SET next_attempt_at=?
		WHERE status='planned' AND target_id IN (SELECT id FROM post_targets WHERE post_id=?)`, at.UTC(), id); err != nil {
		return err
	}
	u := at.UTC()
	if err := emit(context.Background(), tx, id, EventPostUpdated, EventData{Status: "scheduled", ScheduledAt: &u}, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.ev.fire()
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
	items := `(SELECT id FROM target_items WHERE target_id IN ` + sub + `)`
	if _, err := tx.Exec(`DELETE FROM item_deliveries WHERE item_id IN `+items, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM target_attempts WHERE target_id IN `+sub, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nostr_fanout SET item_id=NULL WHERE item_id IN `+items, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM target_items WHERE target_id IN `+sub, id); err != nil {
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
		// Only targets with a failed endpoint (relay) the retrier may still
		// retry: one whose failed relays all gave up has nothing left, and
		// reloading it on every tick would grow with history.
		`SELECT DISTINCT pt.post_id FROM post_targets pt
		  WHERE pt.status IN ('failed','partial') AND pt.gave_up_at IS NULL
		    AND EXISTS (SELECT 1 FROM target_items ti JOIN item_deliveries d ON d.item_id = ti.id
		                 WHERE ti.target_id = pt.id AND d.status = 'failed' AND d.gave_up_at IS NULL)`)
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
		`UPDATE item_deliveries SET gave_up_at=?
		  WHERE endpoint=? AND gave_up_at IS NULL AND item_id=(SELECT id FROM target_items WHERE target_id=? AND ordinal=0)`,
		at.UTC(), relayURL, targetID)
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
