package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// attentionExpr is true for a post (aliased p) that is settled and needs
// the owner: an item whose fate is unknown, or a target the engine gave up
// on with something unsent.
const attentionExpr = `(p.status NOT IN ('sending','scheduled','cancelling','pending') AND EXISTS (
	SELECT 1 FROM target_items ti JOIN post_targets pt ON pt.id = ti.target_id
	 WHERE pt.post_id = p.id AND (ti.status = 'unknown' OR (pt.gave_up_at IS NOT NULL AND ti.status = 'failed'))))`

// PostQuery selects a page of an owner's posts, newest first.
type PostQuery struct {
	OwnerID string
	// Statuses keeps posts with one of these store statuses; Attention
	// keeps those that need the owner. Both empty: any.
	Statuses  []string
	Attention bool
	AccountID string // posts with a target on this account
	Text      string // substring of the text
	Since     time.Time
	Until     time.Time
	Hidden    *bool // nil: only shown posts
	// AfterTime and AfterID are the last post of the previous page.
	AfterTime time.Time
	AfterID   string
	Limit     int
}

// ListPostsPage returns a page of posts with their targets (account,
// platform and status, no items), ordered by created_at then id, newest
// first: a keyset order, so new posts don't shift later pages.
func (s *Store) ListPostsPage(ctx context.Context, q PostQuery) ([]Post, error) {
	where := []string{"p.owner_id = ?"}
	args := []any{q.OwnerID}
	hidden := false
	if q.Hidden != nil {
		hidden = *q.Hidden
	}
	where = append(where, "p.hidden = ?")
	args = append(args, hidden)
	var status []string
	if len(q.Statuses) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(q.Statuses)), ",")
		status = append(status, "(p.status IN ("+marks+") AND NOT "+attentionExpr+")")
		for _, st := range q.Statuses {
			args = append(args, st)
		}
	}
	if q.Attention {
		status = append(status, attentionExpr)
	}
	if len(status) > 0 {
		where = append(where, "("+strings.Join(status, " OR ")+")")
	}
	if q.AccountID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM post_targets pt WHERE pt.post_id = p.id AND pt.account_id = ?)")
		args = append(args, q.AccountID)
	}
	if q.Text != "" {
		where = append(where, `p.master_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q.Text)+"%")
	}
	if !q.Since.IsZero() {
		where = append(where, "p.created_at >= ?")
		args = append(args, q.Since.UTC())
	}
	if !q.Until.IsZero() {
		where = append(where, "p.created_at < ?")
		args = append(args, q.Until.UTC())
	}
	if q.AfterID != "" {
		where = append(where, "(p.created_at < ? OR (p.created_at = ? AND p.id < ?))")
		args = append(args, q.AfterTime.UTC(), q.AfterTime.UTC(), q.AfterID)
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	args = append(args, q.Limit)
	rows, err := s.sql.QueryContext(ctx, `SELECT p.id, p.created_at, p.master_text, p.platforms, p.source, p.status,
		  p.scheduled_at, p.interaction_json, p.owner_id, p.hidden, `+attentionExpr+`
		FROM posts p WHERE `+strings.Join(where, " AND ")+`
		ORDER BY p.created_at DESC, p.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Post, 0)
	for rows.Next() {
		var p Post
		var platforms string
		var sa sql.NullTime
		var ix sql.NullString
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.Source, &p.Status, &sa, &ix,
			&p.OwnerID, &p.Hidden, &p.Attention); err != nil {
			rows.Close()
			return nil, err
		}
		p.CreatedAt = p.CreatedAt.UTC()
		_ = json.Unmarshal([]byte(platforms), &p.Platforms)
		if ix.String != "" {
			var in Interaction
			if json.Unmarshal([]byte(ix.String), &in) == nil {
				p.Interaction = &in
			}
		}
		if sa.Valid {
			t := sa.Time.UTC()
			p.ScheduledAt = &t
		}
		out = append(out, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// The page's targets in one query.
	idx := make(map[string]int, len(out))
	ids := make([]any, len(out))
	for i, p := range out {
		idx[p.ID], ids[i] = i, p.ID
	}
	trows, err := s.sql.QueryContext(ctx, `SELECT post_id, id, account_id, platform, status, COALESCE(error_class, ''), gave_up_at
		FROM post_targets WHERE post_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) ORDER BY id`, ids...)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var postID string
		var t Target
		var gu sql.NullTime
		if err := trows.Scan(&postID, &t.ID, &t.AccountID, &t.Platform, &t.Status, &t.ErrorClass, &gu); err != nil {
			return nil, err
		}
		if gu.Valid {
			g := gu.Time.UTC()
			t.GaveUpAt = &g
		}
		i := idx[postID]
		out[i].Targets = append(out[i].Targets, t)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
