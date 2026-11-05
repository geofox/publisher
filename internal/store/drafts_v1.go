package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// V1Draft is a draft as /v1 keeps it: a POST /v1/posts body (Body, JSON)
// with a title and tags. Text is the body's text, kept in its own column
// for search; Version counts writes and is the draft's ETag.
type V1Draft struct {
	ID      string
	OwnerID string
	Title   string
	Tags    []string
	Text    string
	Body    string
	// FirstMediaID is the post's first media id, read by ListV1Drafts
	// (which reads neither Body nor all of Text).
	FirstMediaID string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ErrDraftConflict is returned by a write whose expected version is not
// the draft's (it changed since the caller read it).
var ErrDraftConflict = errors.New("draft changed since it was read")

// CreateV1Draft stores d, minting its id and times; Version becomes 1.
func (s *Store) CreateV1Draft(ctx context.Context, d *V1Draft) error {
	if d.OwnerID == "" {
		d.OwnerID = DefaultOwner
	}
	if d.ID == "" {
		d.ID = newID()
	}
	d.Tags = NormalizeTags(d.Tags)
	tags, _ := json.Marshal(d.Tags)
	now := time.Now().UTC()
	d.CreatedAt, d.UpdatedAt, d.Version = now, now, 1
	_, err := s.sql.ExecContext(ctx, `INSERT INTO drafts (id, owner_id, created_at, updated_at, title, master_text,
		  tags_json, spec_json, body_json, version) VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, 1)`,
		d.ID, d.OwnerID, now, now, d.Title, d.Text, string(tags), d.Body)
	return err
}

// RestoreV1Draft puts back a draft deleted a moment ago (a post that used
// it up failed to save), as it was.
func (s *Store) RestoreV1Draft(ctx context.Context, d *V1Draft) error {
	tags, _ := json.Marshal(NormalizeTags(d.Tags))
	_, err := s.sql.ExecContext(ctx, `INSERT INTO drafts (id, owner_id, created_at, updated_at, title, master_text,
		  tags_json, spec_json, body_json, version) VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
		d.ID, d.OwnerID, d.CreatedAt.UTC(), d.UpdatedAt.UTC(), d.Title, d.Text, string(tags), d.Body, d.Version)
	return err
}

const v1DraftCols = `id, owner_id, title, tags_json, master_text, body_json, version, created_at, updated_at`

func scanV1Draft(sc interface{ Scan(...any) error }) (*V1Draft, error) {
	d := &V1Draft{}
	var tags string
	if err := sc.Scan(&d.ID, &d.OwnerID, &d.Title, &tags, &d.Text, &d.Body, &d.Version, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tags), &d.Tags)
	if d.Tags == nil {
		d.Tags = []string{}
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

// GetV1Draft returns the owner's draft (ErrDraftNotFound otherwise, also
// for a draft not yet converted).
func (s *Store) GetV1Draft(ctx context.Context, owner, id string) (*V1Draft, error) {
	d, err := scanV1Draft(s.sql.QueryRowContext(ctx, `SELECT `+v1DraftCols+` FROM drafts
		WHERE id = ? AND owner_id = ? AND body_json IS NOT NULL`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	return d, err
}

// UpdateV1Draft writes d's title, tags, text and body when the draft is
// still at version want (0: whatever it is), and bumps its version.
func (s *Store) UpdateV1Draft(ctx context.Context, d *V1Draft, want int64) error {
	d.Tags = NormalizeTags(d.Tags)
	tags, _ := json.Marshal(d.Tags)
	now := time.Now().UTC()
	q := `UPDATE drafts SET title = ?, tags_json = ?, master_text = ?, body_json = ?, version = version + 1, updated_at = ?
		WHERE id = ? AND owner_id = ? AND body_json IS NOT NULL`
	args := []any{d.Title, string(tags), d.Text, d.Body, now, d.ID, d.OwnerID}
	if want > 0 {
		q += ` AND version = ?`
		args = append(args, want)
	}
	res, err := s.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.draftMissOrConflict(ctx, d.OwnerID, d.ID)
	}
	cur, err := s.GetV1Draft(ctx, d.OwnerID, d.ID)
	if err != nil {
		return err
	}
	*d = *cur
	return nil
}

// DeleteV1Draft deletes the owner's draft when it is at version want (0:
// any).
func (s *Store) DeleteV1Draft(ctx context.Context, owner, id string, want int64) error {
	q := `DELETE FROM drafts WHERE id = ? AND owner_id = ? AND body_json IS NOT NULL`
	args := []any{id, owner}
	if want > 0 {
		q += ` AND version = ?`
		args = append(args, want)
	}
	res, err := s.sql.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.draftMissOrConflict(ctx, owner, id)
	}
	return nil
}

func (s *Store) draftMissOrConflict(ctx context.Context, owner, id string) error {
	if _, err := s.GetV1Draft(ctx, owner, id); err != nil {
		return err
	}
	return ErrDraftConflict
}

// V1DraftQuery selects drafts for a list page: newest change first.
type V1DraftQuery struct {
	OwnerID string
	Text    string   // in the text or title
	Tags    []string // every one of them
	Limit   int
	// Before is the position after the previous page's last draft.
	BeforeTime *time.Time
	BeforeID   string
}

// ListV1Drafts returns a page of the owner's drafts: without their bodies
// (FirstMediaID instead) and with the text's first 500 characters only.
func (s *Store) ListV1Drafts(ctx context.Context, q V1DraftQuery) ([]V1Draft, error) {
	where := []string{"owner_id = ?", "body_json IS NOT NULL"}
	args := []any{q.OwnerID}
	if t := strings.TrimSpace(q.Text); t != "" {
		where = append(where, `(master_text LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(t) + "%"
		args = append(args, like, like)
	}
	for _, tag := range NormalizeTags(q.Tags) {
		b, _ := json.Marshal(tag)
		where = append(where, `tags_json LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(string(b))+"%")
	}
	if q.BeforeTime != nil {
		where = append(where, "(updated_at < ? OR (updated_at = ? AND id < ?))")
		args = append(args, q.BeforeTime.UTC(), q.BeforeTime.UTC(), q.BeforeID)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	args = append(args, q.Limit)
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id, title, tags_json, substr(master_text, 1, 500),
		COALESCE(json_extract(body_json, '$.content.media[0].id'), ''), version, created_at, updated_at
		FROM drafts WHERE `+strings.Join(where, " AND ")+
		` ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("ListV1Drafts: %w", err)
	}
	defer rows.Close()
	out := []V1Draft{}
	for rows.Next() {
		d, err := scanV1Draft(rows) // FirstMediaID in Body's place
		if err != nil {
			return nil, err
		}
		d.FirstMediaID, d.Body = d.Body, ""
		out = append(out, *d)
	}
	return out, rows.Err()
}

// OldDraft is a draft from before /v1, still to convert.
type OldDraft struct {
	Draft
	OwnerID string
}

// OldDrafts returns the drafts not yet converted to the /v1 shape, with
// their media.
func (s *Store) OldDrafts(ctx context.Context) ([]OldDraft, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, owner_id FROM drafts WHERE body_json IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	type key struct{ id, owner string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.id, &k.owner); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]OldDraft, 0, len(keys))
	for _, k := range keys {
		d, err := s.GetDraft(k.id)
		if err != nil {
			return nil, err
		}
		out = append(out, OldDraft{Draft: *d, OwnerID: k.owner})
	}
	return out, nil
}

// ConvertDraft gives an old draft its /v1 body (once: a converted draft is
// left alone). Its old media rows stay, unused.
func (s *Store) ConvertDraft(ctx context.Context, id, title, text, body string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE drafts SET title = ?, master_text = ?, body_json = ?
		WHERE id = ? AND body_json IS NULL`, title, text, body, id)
	return err
}
