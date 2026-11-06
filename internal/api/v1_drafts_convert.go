package api

import (
	"context"
	"encoding/json"
	"log/slog"
	neturl "net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

// ConvertOldDrafts gives each draft saved before /v1 its /v1 body, once
// (at startup, before the server takes requests). The old spec named
// destination types and held overrides in the engine's field names; the
// new body names accounts and their options, and media by id: a draft's
// files in this server's media storage become media objects, others are
// dropped (and logged). Drafts are converted one by one: one that fails
// is left as it was, logged, and tried again on the next start.
func (a *API) ConvertOldDrafts(ctx context.Context) (converted, failed int) {
	if a.Store == nil {
		return 0, 0
	}
	olds, err := a.Store.OldDrafts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "drafts: list drafts to convert", "err", err)
		return 0, 1
	}
	for _, od := range olds {
		if err := a.convertOldDraft(ctx, od); err != nil {
			slog.ErrorContext(ctx, "drafts: convert", "draft_id", od.ID, "err", err)
			failed++
			continue
		}
		converted++
	}
	if converted+failed > 0 {
		slog.InfoContext(ctx, "drafts: converted to /v1", "converted", converted, "failed", failed)
	}
	return converted, failed
}

// oldDraftSpec is the old composer's draft spec, as far as the conversion
// reads it.
type oldDraftSpec struct {
	MasterText  string                  `json:"master_text"`
	Platforms   []string                `json:"platforms"`
	Overrides   map[string]oldOverrideF `json:"-"`
	RawOv       json.RawMessage         `json:"overrides"`
	Interaction *struct {
		Action      string `json:"action"`
		SourceURL   string `json:"sourceURL"`
		SourceInput string `json:"sourceInput"`
	} `json:"interaction"`
	Anchors     map[string]int  `json:"-"`
	RawAnchors  json.RawMessage `json:"anchors"`
	Number      *bool           `json:"number"`
	ScheduledAt string          `json:"scheduled_at"`
}

func (a *API) convertOldDraft(ctx context.Context, od store.OldDraft) error {
	var sp oldDraftSpec
	// A spec that doesn't parse keeps what the draft row has itself.
	_ = json.Unmarshal([]byte(od.Spec), &sp)
	// Each part on its own, so one malformed field loses only itself.
	if len(sp.RawOv) > 0 {
		raw := map[string]json.RawMessage{}
		_ = json.Unmarshal(sp.RawOv, &raw)
		sp.Overrides = map[string]oldOverrideF{}
		for k, v := range raw {
			var ov oldOverrideF
			if json.Unmarshal(v, &ov) == nil {
				sp.Overrides[k] = ov
			}
		}
	}
	if len(sp.RawAnchors) > 0 {
		_ = json.Unmarshal(sp.RawAnchors, &sp.Anchors)
	}
	text := od.MasterText
	if text == "" {
		text = sp.MasterText
	}
	req := v1CreatePostReq{Content: v1CreateContent{Text: &text}, Targets: []v1CreateTarget{}}

	// Media: the draft's files in this server's storage, in order.
	blossom := ""
	if a.media != nil {
		blossom = a.media.BlossomURL
	}
	var given []string
	for _, m := range od.Media {
		if !adoptable(m, blossom) {
			slog.WarnContext(ctx, "drafts: convert: attachment not in this server's media storage, dropped",
				"draft_id", od.ID, "ordinal", m.Ordinal)
			continue
		}
		m, ok := adoptedMeta(m, blossom)
		if !ok {
			slog.WarnContext(ctx, "drafts: convert: attachment of an unknown type, dropped",
				"draft_id", od.ID, "ordinal", m.Ordinal)
			continue
		}
		obj, err := a.Store.AdoptMedia(ctx, od.OwnerID, m, given)
		if err != nil {
			return err
		}
		given = append(given, obj.ID)
		alt := m.Alt
		cm := v1CreateMedia{ID: obj.ID, Alt: &alt}
		if p, ok := sp.Anchors[m.ClientID]; ok && m.ClientID != "" && p > 0 && p < maxThreadParts {
			cm.Part = &p
		}
		req.Content.Media = append(req.Content.Media, cm)
	}

	// Targets: the owner's enabled account of each type the draft named.
	accts, err := a.Store.Accounts(ctx, od.OwnerID)
	if err != nil {
		return err
	}
	for _, typ := range sp.Platforms {
		i := slices.IndexFunc(accts, func(ac store.Account) bool { return ac.Type == typ && ac.Enabled })
		if i < 0 || slices.ContainsFunc(req.Targets, func(t v1CreateTarget) bool { return t.Account == accts[i].ID }) {
			continue
		}
		t := v1CreateTarget{Account: accts[i].ID}
		if ov, ok := sp.Overrides[typ]; ok {
			t.Options, t.Content = oldOverrides(typ, ov)
		}
		req.Targets = append(req.Targets, t)
	}

	if sp.Number != nil {
		req.Threading = &v1CreateThreading{Numbering: *sp.Number}
	}
	if it := sp.Interaction; it != nil {
		u := strings.TrimSpace(it.SourceInput)
		if u == "" {
			u = strings.TrimSpace(it.SourceURL)
		}
		switch {
		case u == "" || len(u) > maxSourceURL:
		case it.Action == "quote":
			req.Quote = &v1SourceIn{URL: u}
		case it.Action == "reply":
			req.ReplyTo = &v1SourceIn{URL: u}
		}
		// A repost was never a draft: it posts at once.
	} else if sp.ScheduledAt != "" {
		at := sp.ScheduledAt
		req.Schedule = &v1CreateSchedule{At: &at}
	}
	// "---" lines and media parts become breaks and positions.
	body, _ := store.ConvertBreakMarkers(marshalPost(&req))
	text, _, _ = thread.FromMarkers(text)
	// The old routes never took a title: theirs was derived (cut at 80
	// bytes); derived again, it follows the text.
	return a.Store.ConvertDraft(ctx, od.ID, store.DeriveTitle(text), text, body)
}

// oldOverrideF is a type's overrides in an old draft: the engine's fields
// then, its own text among them.
type oldOverrideF struct {
	dispatch.Overrides
	Text string `json:"text"`
}

// oldOverrides turns a type's old override fields into a target's options
// and content, keeping only the options its destination declares.
func oldOverrides(typ string, ov oldOverrideF) (map[string]any, *v1TargetContent) {
	drv, ok := destination.Lookup(typ)
	if !ok {
		return nil, nil
	}
	m := drv.Manifest()
	opts := map[string]any{}
	set := func(k string, v any) {
		if _, ok := m.Option(k); ok {
			opts[k] = v
		}
	}
	if ov.BlueskyReply != "" {
		set("reply_gate", ov.BlueskyReply)
	}
	if ov.BlueskyDisableQuotes {
		set("disable_quotes", true)
	}
	if ov.TopicTag != "" {
		set("topic_tag", ov.TopicTag)
	}
	if ov.ThreadsReplyControl != "" {
		set("reply_control", ov.ThreadsReplyControl)
	}
	if ov.POW != nil {
		set("pow", *ov.POW)
	}
	if ov.Visibility != "" && slices.Contains(m.Defaults.Visibility, ov.Visibility) {
		opts["visibility"] = ov.Visibility
	}
	c := &v1TargetContent{}
	if strings.TrimSpace(ov.Text) != "" {
		t := ov.Text
		c.Text = &t
	}
	cw := ov.SpoilerText
	if cw == "" {
		cw = ov.ContentWarning
	}
	if cw != "" {
		c.ContentWarning = &cw
	}
	if ov.Sensitive {
		s := true
		c.Sensitive = &s
	}
	var langs []string
	for _, l := range append(slices.Clone(ov.Langs), ov.Language) {
		if l = strings.TrimSpace(l); l != "" && langTag.MatchString(l) && !slices.Contains(langs, l) {
			langs = append(langs, l)
		}
	}
	if len(langs) > maxPostLanguages {
		langs = langs[:maxPostLanguages]
	}
	c.Languages = langs
	if len(opts) == 0 {
		opts = nil
	}
	if c.Text == nil && c.ContentWarning == nil && c.Sensitive == nil && len(c.Languages) == 0 {
		c = nil
	}
	return opts, c
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// adoptable is whether a draft attachment is a file in this server's media
// storage: on the configured Blossom server's origin, at /<sha256> (with an
// optional extension), so the file is the one its hash names. Old drafts'
// references came through the client: anything else (another host, an
// internal address) never becomes media a post would fetch or publish.
func adoptable(m store.Media, blossom string) bool {
	base, err := neturl.Parse(blossom)
	if err != nil || base.Host == "" {
		return false
	}
	u, err := neturl.Parse(m.BlossomURL)
	if err != nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || !sha256Hex.MatchString(m.SHA256) {
		return false
	}
	rest, ok := strings.CutPrefix(u.Path, strings.TrimRight(base.Path, "/")+"/"+m.SHA256)
	return ok && (rest == "" || blossomExt.MatchString(rest))
}

var blossomExt = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

// adoptedMeta checks the rest of an old attachment's description, which
// the old routes took from the client as it came: a poster only from this
// server's media storage (a post publishes it), a known media type, sane
// dimensions and sizes. ok=false: a type no post could carry.
func adoptedMeta(m store.Media, blossom string) (store.Media, bool) {
	if !adoptableMime[m.Mime] {
		return m, false
	}
	if m.PosterURL != "" && !onBlossom(m.PosterURL, blossom) {
		m.PosterURL = ""
	}
	if !dimPattern.MatchString(m.Dim) {
		m.Dim = ""
	}
	if !blurhashPattern.MatchString(m.Blurhash) {
		m.Blurhash = ""
	}
	m.SizeBytes = max(m.SizeBytes, 0)
	m.DurationSecs = min(max(m.DurationSecs, 0), 24*3600)
	return m, true
}

var (
	adoptableMime = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
		"image/heic": true, "image/heif": true, "image/avif": true, "video/mp4": true, "video/quicktime": true, "video/webm": true}
	dimPattern      = regexp.MustCompile(`^[1-9][0-9]{0,4}x[1-9][0-9]{0,4}$`)
	blurhashPattern = regexp.MustCompile(`^[0-9A-Za-z#$%*+,\-.:;=?@\[\]^_{|}~]{6,100}$`)
)

// onBlossom is whether u is a file in this server's media storage: on the
// Blossom origin at /<sha256>[.ext] (whichever hash).
func onBlossom(u, blossom string) bool {
	pu, err := neturl.Parse(u)
	if err != nil {
		return false
	}
	sha := strings.TrimPrefix(pu.Path, strings.TrimRight(blossomPath(blossom), "/")+"/")
	if i := strings.IndexByte(sha, '.'); i >= 0 {
		sha = sha[:i]
	}
	return sha256Hex.MatchString(sha) && adoptable(store.Media{BlossomURL: u, SHA256: sha}, blossom)
}

func blossomPath(blossom string) string {
	if b, err := neturl.Parse(blossom); err == nil {
		return b.Path
	}
	return ""
}
