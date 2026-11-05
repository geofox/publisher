package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// Media object statuses.
const (
	MediaProcessing = "processing" // a video being normalized
	MediaReady      = "ready"
	MediaFailed     = "failed"
)

// MediaObject is media uploaded through /v1 for later posts: a file in
// media storage (Blossom) with what posting needs to know about it.
type MediaObject struct {
	ID           string
	OwnerID      string
	Status       string
	URL          string
	SHA256       string
	Mime         string
	Dim          string
	Blurhash     string
	SizeBytes    int64
	DurationSecs int64
	PosterURL    string
	Alt          string
	Error        string
	JobID        string // the video job processing it
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ErrNoMedia is returned for a media id that doesn't exist.
var ErrNoMedia = errors.New("no such media")

// CreateMediaObject stores m, minting its id (med_…) and times.
func (s *Store) CreateMediaObject(ctx context.Context, m *MediaObject) error {
	now := time.Now().UTC()
	if m.ID == "" {
		m.ID = "med_" + newID()
	}
	if m.OwnerID == "" {
		m.OwnerID = DefaultOwner
	}
	m.CreatedAt, m.UpdatedAt = now, now
	_, err := s.sql.ExecContext(ctx, `INSERT INTO media_objects (id, owner_id, status, url, sha256, mime, dim, blurhash,
		  size_bytes, duration_secs, poster_url, alt, error, job_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OwnerID, m.Status, m.URL, m.SHA256, m.Mime, m.Dim, m.Blurhash, m.SizeBytes, m.DurationSecs,
		m.PosterURL, m.Alt, Scrub(m.Error), m.JobID, now, now)
	return err
}

// MediaObject returns the owner's media object id (ErrNoMedia otherwise).
func (s *Store) MediaObject(ctx context.Context, owner, id string) (*MediaObject, error) {
	m := &MediaObject{}
	err := s.sql.QueryRowContext(ctx, `SELECT id, owner_id, status, url, sha256, mime, dim, blurhash, size_bytes,
		  duration_secs, poster_url, alt, error, job_id, created_at, updated_at
		FROM media_objects WHERE id = ? AND owner_id = ?`, id, owner).
		Scan(&m.ID, &m.OwnerID, &m.Status, &m.URL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes,
			&m.DurationSecs, &m.PosterURL, &m.Alt, &m.Error, &m.JobID, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoMedia
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// FinishMediaObject records how a processing object ended: ready with its
// file, or failed with why. Only a processing object changes; false when
// it had already finished.
func (s *Store) FinishMediaObject(ctx context.Context, m *MediaObject) (bool, error) {
	now := time.Now().UTC()
	res, err := s.sql.ExecContext(ctx, `UPDATE media_objects SET status = ?, url = ?, sha256 = ?, mime = ?, dim = ?,
		  blurhash = ?, size_bytes = ?, duration_secs = ?, poster_url = ?, error = ?, updated_at = ?
		WHERE id = ? AND owner_id = ? AND status = ?`,
		m.Status, m.URL, m.SHA256, m.Mime, m.Dim, m.Blurhash, m.SizeBytes, m.DurationSecs, m.PosterURL,
		Scrub(m.Error), now, m.ID, m.OwnerID, MediaProcessing)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		m.UpdatedAt = now
	}
	return n == 1, nil
}

// SetMediaAlt sets a media object's alt text.
func (s *Store) SetMediaAlt(ctx context.Context, owner, id, alt string) error {
	res, err := s.sql.ExecContext(ctx, `UPDATE media_objects SET alt = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
		alt, time.Now().UTC(), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoMedia
	}
	return nil
}

// AdoptMedia returns a ready media object for a file already in media
// storage (a draft's attachment), creating one when the owner has none for
// that file. skip lists objects already handed out (the same file attached
// twice gets two objects, as a post names each media once); the oldest
// other one is taken, so reading a draft again gives the same ids. The web
// composer posts a draft's media by id this way; drafts on /v1 hold media
// ids themselves.
func (s *Store) AdoptMedia(ctx context.Context, owner string, m Media, skip []string) (*MediaObject, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id FROM media_objects
		WHERE owner_id = ? AND status = ? AND url = ? AND sha256 = ? ORDER BY created_at, id`,
		owner, MediaReady, m.BlossomURL, m.SHA256)
	if err != nil {
		return nil, err
	}
	id := ""
	for rows.Next() {
		var cand string
		if err := rows.Scan(&cand); err != nil {
			rows.Close()
			return nil, err
		}
		if !slices.Contains(skip, cand) {
			id = cand
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if id != "" {
		return s.MediaObject(ctx, owner, id)
	}
	mo := &MediaObject{OwnerID: owner, Status: MediaReady, URL: m.BlossomURL, SHA256: m.SHA256, Mime: m.Mime,
		Dim: m.Dim, Blurhash: m.Blurhash, SizeBytes: m.SizeBytes, DurationSecs: m.DurationSecs,
		PosterURL: m.PosterURL, Alt: m.Alt}
	if err := s.CreateMediaObject(ctx, mo); err != nil {
		return nil, err
	}
	return mo, nil
}
