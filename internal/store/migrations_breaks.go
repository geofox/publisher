package store

import (
	"context"
	"encoding/json"

	"github.com/geofox/publisher/internal/thread"
)

// migrateDraftBreaks (v9) moves drafts off "---" lines: a post body's
// thread breaks become positions (content.breaks, targets[].thread.breaks)
// and media placed by part get a position (media[].at, thread.media_at).
// From here on "---" in a text is posted as written.
func migrateDraftBreaks(ctx context.Context, tx execer) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, body_json FROM drafts WHERE body_json IS NOT NULL`)
	if err != nil {
		return err
	}
	type change struct{ id, body, text string }
	var changes []change
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return err
		}
		if out, ok := ConvertBreakMarkers(body); ok {
			changes = append(changes, change{id, out, bodyText(out)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE drafts SET body_json = ?, master_text = ?, version = version + 1 WHERE id = ?`,
			c.body, c.text, c.id); err != nil {
			return err
		}
	}
	return nil
}

// bodyText is a post body's shared text.
func bodyText(body string) string {
	var v struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal([]byte(body), &v)
	return v.Content.Text
}

// ConvertBreakMarkers rewrites a POST /v1/posts body that breaks its
// thread the old way (lines of "---", media[].part) into positions: the
// shared text's breaks become content.breaks and media[].at; an account's
// own text's become its thread.breaks, and its thread.media_at when a
// media had a part (counted in its own parts, as the old planner did). ok
// is false when there was nothing to convert (or the body isn't a JSON
// object); the body is then returned as it was.
func ConvertBreakMarkers(body string) (out string, ok bool) {
	var p map[string]any
	if json.Unmarshal([]byte(body), &p) != nil || p == nil {
		return body, false
	}
	changed := false
	content, _ := p["content"].(map[string]any)
	parts := []int{0}
	if content != nil {
		if t, isStr := content["text"].(string); isStr {
			text, breaks, ps := thread.FromMarkers(t)
			parts = ps
			if len(breaks) > 0 {
				content["text"], content["breaks"] = text, breaks
				changed = true
			}
		}
	}
	var media []any
	if content != nil {
		media, _ = content["media"].([]any)
	}
	partOf := make([]int, len(media))
	anyPart := false
	for i, m := range media {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if f, has := mm["part"].(float64); has {
			delete(mm, "part")
			changed = true
			if k := int(f); k > 0 {
				partOf[i], anyPart = k, true
				mm["at"] = parts[min(k, len(parts)-1)]
			}
		}
	}
	targets, _ := p["targets"].([]any)
	for _, t := range targets {
		tm, _ := t.(map[string]any)
		c, _ := tm["content"].(map[string]any)
		if c == nil {
			continue
		}
		s, isStr := c["text"].(string)
		if !isStr {
			continue
		}
		text, breaks, own := thread.FromMarkers(s)
		if len(breaks) == 0 && !anyPart {
			continue
		}
		th := map[string]any{}
		if len(breaks) > 0 {
			c["text"], th["breaks"] = text, breaks
		}
		if anyPart {
			at := make([]int, len(media))
			for j := range media {
				at[j] = own[min(partOf[j], len(own)-1)]
			}
			th["media_at"] = at
		}
		tm["thread"] = th
		changed = true
	}
	if !changed {
		return body, false
	}
	b, err := json.Marshal(p)
	if err != nil {
		return body, false
	}
	return string(b), true
}
