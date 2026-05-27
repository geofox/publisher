package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Draft is a saved work-in-progress Compose state. See
// docs/superpowers/specs/2026-05-27-drafts-design.md for the full shape.
type Draft struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Title      string    `json:"title"`
	MasterText string    `json:"master_text"`
	Tags       []string  `json:"tags"`
	Spec       string    `json:"spec"` // raw spec_json — opaque to the store
	Media      []Media   `json:"media,omitempty"`
}

// DraftListItem is the lightweight projection used by ListDraftsFiltered for
// the sidebar — full spec is deliberately omitted.
type DraftListItem struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Preview       string    `json:"preview"`
	Tags          []string  `json:"tags"`
	UpdatedAt     time.Time `json:"updated_at"`
	FirstMediaURL string    `json:"first_media_url,omitempty"`
}

// NormalizeTags applies the server-side tag rules: lowercase, trim, strip a
// single leading '#', cap at 32 chars (truncate), drop empties, dedup,
// preserving first-occurrence order. Always returns a non-nil slice.
func NormalizeTags(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		t = strings.ToLower(t)
		if strings.HasPrefix(t, "#") {
			t = t[1:]
		}
		if len(t) > 32 {
			t = t[:32]
		}
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// DeriveTitle returns the first non-empty line of master_text, trimmed and
// capped at 80 chars. Used when the caller didn't supply an explicit title.
func DeriveTitle(masterText string) string {
	for _, line := range strings.Split(masterText, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if len(t) > 80 {
			t = t[:80]
		}
		return t
	}
	return ""
}

// CreateDraft inserts a new draft (with media) in a single transaction.
// CreatedAt / UpdatedAt must be set by the caller (callers usually use
// time.Now().UTC()). Tags are normalized via NormalizeTags before persistence.
func (s *Store) CreateDraft(d *Draft) error {
	if d.ID == "" {
		return errors.New("CreateDraft: empty id")
	}
	d.Tags = NormalizeTags(d.Tags)
	tagsJSON, err := json.Marshal(d.Tags)
	if err != nil {
		return fmt.Errorf("CreateDraft: marshal tags: %w", err)
	}
	if d.Title == "" {
		d.Title = DeriveTitle(d.MasterText)
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO drafts(id, created_at, updated_at, title, master_text, tags_json, spec_json)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.CreatedAt.UTC(), d.UpdatedAt.UTC(), d.Title, d.MasterText, string(tagsJSON), d.Spec,
	); err != nil {
		return fmt.Errorf("CreateDraft: insert draft: %w", err)
	}
	if err := insertDraftMedia(tx, d.ID, d.Media); err != nil {
		return err
	}
	return tx.Commit()
}

func insertDraftMedia(tx *sql.Tx, draftID string, media []Media) error {
	for _, m := range media {
		if _, err := tx.Exec(
			`INSERT INTO draft_media(draft_id, ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			draftID, m.Ordinal, m.BlossomURL, m.SHA256, m.Mime, m.Dim, m.Blurhash, m.SizeBytes, m.Alt,
		); err != nil {
			return fmt.Errorf("insertDraftMedia: %w", err)
		}
	}
	return nil
}

// ErrDraftNotFound is returned by GetDraft / UpdateDraft / DeleteDraft when no
// row matches the requested id.
var ErrDraftNotFound = errors.New("draft not found")

// GetDraft returns a draft hydrated with its media. Returns ErrDraftNotFound
// (wrapped) if no row matches.
func (s *Store) GetDraft(id string) (*Draft, error) {
	d := &Draft{ID: id}
	var tagsJSON string
	err := s.sql.QueryRow(
		`SELECT created_at, updated_at, title, master_text, tags_json, spec_json
		 FROM drafts WHERE id=?`, id,
	).Scan(&d.CreatedAt, &d.UpdatedAt, &d.Title, &d.MasterText, &tagsJSON, &d.Spec)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("GetDraft %q: %w", id, ErrDraftNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("GetDraft %q: %w", id, err)
	}
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &d.Tags)
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	media, err := s.getDraftMedia(id)
	if err != nil {
		return nil, err
	}
	d.Media = media
	return d, nil
}

func (s *Store) getDraftMedia(draftID string) ([]Media, error) {
	rows, err := s.sql.Query(
		`SELECT ordinal, blossom_url, sha256, COALESCE(mime,''), COALESCE(dim,''),
		        COALESCE(blurhash,''), COALESCE(size_bytes,0), COALESCE(alt,'')
		 FROM draft_media WHERE draft_id=? ORDER BY ordinal`, draftID,
	)
	if err != nil {
		return nil, fmt.Errorf("getDraftMedia: %w", err)
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		var m Media
		if err := rows.Scan(&m.Ordinal, &m.BlossomURL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes, &m.Alt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateDraft replaces the row's mutable fields and fully replaces the
// draft_media set in a single transaction. UpdatedAt should be set by the
// caller. Returns ErrDraftNotFound (wrapped) if the row doesn't exist.
func (s *Store) UpdateDraft(d *Draft) error {
	if d.ID == "" {
		return errors.New("UpdateDraft: empty id")
	}
	d.Tags = NormalizeTags(d.Tags)
	tagsJSON, err := json.Marshal(d.Tags)
	if err != nil {
		return fmt.Errorf("UpdateDraft: marshal tags: %w", err)
	}
	if d.Title == "" {
		d.Title = DeriveTitle(d.MasterText)
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE drafts SET updated_at=?, title=?, master_text=?, tags_json=?, spec_json=? WHERE id=?`,
		d.UpdatedAt.UTC(), d.Title, d.MasterText, string(tagsJSON), d.Spec, d.ID,
	)
	if err != nil {
		return fmt.Errorf("UpdateDraft: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("UpdateDraft %q: %w", d.ID, ErrDraftNotFound)
	}
	if _, err := tx.Exec(`DELETE FROM draft_media WHERE draft_id=?`, d.ID); err != nil {
		return fmt.Errorf("UpdateDraft: clear media: %w", err)
	}
	if err := insertDraftMedia(tx, d.ID, d.Media); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDraft removes a draft and (via ON DELETE CASCADE) its media rows.
// Returns ErrDraftNotFound (wrapped) if no row matches.
func (s *Store) DeleteDraft(id string) error {
	res, err := s.sql.Exec(`DELETE FROM drafts WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("DeleteDraft %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("DeleteDraft %q: %w", id, ErrDraftNotFound)
	}
	return nil
}

// DraftFilter selects drafts for ListDraftsFiltered. Tags are AND-combined
// (a draft must contain every requested tag). Tags are normalized by the caller.
type DraftFilter struct {
	Query  string
	Tags   []string
	Limit  int
	Offset int
}

// ListDraftsFiltered returns lightweight DraftListItem rows ordered by
// updated_at DESC. The full spec is not loaded; only the first media row's
// URL is included for thumbnail rendering.
func (s *Store) ListDraftsFiltered(f DraftFilter) ([]DraftListItem, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	where := []string{"1=1"}
	args := []any{}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "master_text LIKE ?")
		args = append(args, "%"+escapeLike(q)+"%")
	}
	for _, tag := range NormalizeTags(f.Tags) {
		where = append(where, "tags_json LIKE ?")
		args = append(args, `%"`+tag+`"%`)
	}
	args = append(args, f.Limit, f.Offset)
	sqlStr := `SELECT id, title, master_text, tags_json, updated_at
	           FROM drafts WHERE ` + strings.Join(where, " AND ") + `
	           ORDER BY updated_at DESC
	           LIMIT ? OFFSET ?`
	rows, err := s.sql.Query(sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("ListDraftsFiltered: %w", err)
	}
	defer rows.Close()
	out := make([]DraftListItem, 0)
	for rows.Next() {
		var it DraftListItem
		var master, tagsJSON string
		if err := rows.Scan(&it.ID, &it.Title, &master, &tagsJSON, &it.UpdatedAt); err != nil {
			return nil, err
		}
		if tagsJSON != "" {
			_ = json.Unmarshal([]byte(tagsJSON), &it.Tags)
		}
		if it.Tags == nil {
			it.Tags = []string{}
		}
		if it.Title == "" {
			it.Title = DeriveTitle(master)
		}
		// short preview (first 120 chars, single-line)
		oneLine := strings.ReplaceAll(strings.ReplaceAll(master, "\n", " "), "\r", " ")
		oneLine = strings.TrimSpace(oneLine)
		if len(oneLine) > 120 {
			oneLine = oneLine[:120]
		}
		it.Preview = oneLine
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Augment with first media URL (one extra query per row — fine at single-user scale)
	for i := range out {
		var url string
		err := s.sql.QueryRow(
			`SELECT blossom_url FROM draft_media WHERE draft_id=? ORDER BY ordinal LIMIT 1`,
			out[i].ID,
		).Scan(&url)
		if err == nil {
			out[i].FirstMediaURL = url
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return out, nil
}
