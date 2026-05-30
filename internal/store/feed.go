package store

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// firstSuccessExpr is the SQL fragment for a post's first-live time: the
// earliest target_attempts.attempted_at across the post's targets whose attempt
// went live — status 'success', or 'partial' (a Nostr note that reached at
// least one relay). This must match feed.targetLink's notion of "live" so the
// publish date covers partial-but-live Nostr-only posts (e.g. a scheduled post
// that fires partial, where created_at would otherwise misdate it). Used for
// both ORDER BY and the FirstSuccessAt projection.
const firstSuccessExpr = `(SELECT MIN(ta.attempted_at) FROM target_attempts ta
	JOIN post_targets pt ON ta.target_id = pt.id
	WHERE pt.post_id = p.id AND ta.status IN ('success','partial'))`

// PublicFeed returns posts for the public homepage feed, newest-first by
// first-success time. It includes only non-hidden posts in a published state
// (success/partial — the only statuses that can hold a successful target) and
// hydrates each target with platform/status/remote_url/fields_json plus media,
// but NOT attempt/relay history. Reply exclusion and per-platform visibility
// filtering are applied later by feed.Build/feed.Eligible, so this over-fetches
// a bounded window to keep a full page after those drops.
func (s *Store) PublicFeed(limit int) ([]Post, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	fetch := limit * 4
	if fetch < 50 {
		fetch = 50
	}
	if fetch > 200 {
		fetch = 200
	}

	q := `SELECT p.id, p.created_at, p.master_text, p.platforms, p.source, p.status, p.interaction_json,
	             ` + firstSuccessExpr + ` AS first_success_at
	        FROM posts p
	       WHERE p.hidden = 0 AND p.status IN ('success','partial')
	       ORDER BY COALESCE(` + firstSuccessExpr + `, p.created_at) DESC
	       LIMIT ?`

	rows, err := s.sql.Query(q, fetch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Post, 0)
	for rows.Next() {
		var p Post
		var platforms string
		var interactionJSON sql.NullString
		var fsa sql.NullString // MIN() loses TIMESTAMP affinity → returned as text
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.MasterText, &platforms, &p.Source, &p.Status, &interactionJSON, &fsa); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(platforms), &p.Platforms)
		if interactionJSON.String != "" {
			var ix Interaction
			if json.Unmarshal([]byte(interactionJSON.String), &ix) == nil {
				p.Interaction = &ix
			}
		}
		if fsa.Valid && fsa.String != "" {
			if t, perr := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", fsa.String); perr == nil {
				u := t.UTC()
				p.FirstSuccessAt = &u
			} else {
				slog.Warn("PublicFeed: unexpected first_success_at format", "post_id", p.ID, "raw", fsa.String, "err", perr)
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		trows, err := s.sql.Query(`SELECT platform, status, remote_url, fields_json FROM post_targets WHERE post_id=? ORDER BY id`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for trows.Next() {
			var tg Target
			var rurl, fields sql.NullString
			if err := trows.Scan(&tg.Platform, &tg.Status, &rurl, &fields); err != nil {
				trows.Close()
				return nil, err
			}
			tg.RemoteURL, tg.FieldsJSON = rurl.String, fields.String
			out[i].Targets = append(out[i].Targets, tg)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return nil, err
		}
		trows.Close()

		mrows, err := s.sql.Query(`SELECT ordinal, blossom_url, sha256, mime, dim, blurhash, size_bytes, alt FROM media WHERE post_id=? ORDER BY ordinal`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m Media
			if err := mrows.Scan(&m.Ordinal, &m.BlossomURL, &m.SHA256, &m.Mime, &m.Dim, &m.Blurhash, &m.SizeBytes, &m.Alt); err != nil {
				mrows.Close()
				return nil, err
			}
			out[i].Media = append(out[i].Media, m)
		}
		if err := mrows.Err(); err != nil {
			mrows.Close()
			return nil, err
		}
		mrows.Close()
	}
	return out, nil
}
