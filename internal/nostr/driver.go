package nostr

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	gonostr "fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/media"
)

// Type is the destination type identifier.
const Type = "nostr"

// OptPOW is the proof-of-work difficulty option (NIP-13).
const OptPOW = "pow"

// Driver is the Nostr destination type.
type Driver struct{}

var _ destination.Driver = Driver{}

// Manifest describes Nostr: no length limit, media by URL (Blossom + NIP-92
// imeta), fan-out to relays.
func (Driver) Manifest() destination.Manifest {
	zero := 0
	return destination.Manifest{
		Contract: destination.ContractVersion,
		Type:     Type,
		Name:     "Nostr",
		Kinds:    []destination.Kind{destination.KindNote},
		Options: []destination.OptionSpec{
			{Key: OptPOW, Type: destination.OptionInt, Label: "Proof of work", Min: &zero,
				Help: "NIP-13 difficulty; clamped to the server's maximum"},
		},
		Defaults: destination.Capabilities{
			Text:      destination.TextCaps{MaxLength: 0, Counting: destination.Counting{Unit: destination.UnitGraphemes}},
			Threading: destination.ThreadingCaps{Supported: true},
			Media: destination.MediaCaps{
				Transport: destination.TransportURL,
				Images:    destination.Limit{Supported: true},
				Video:     destination.Limit{Supported: true, MaxCount: 1},
				AltText:   true,
			},
			LinkPreview:    destination.LinkPreviewNone,
			ContentWarning: true,
			Interactions:   destination.InteractionCaps{Reply: true, Quote: true, Repost: true},
			// Each attempt signs a new event today, so a repeat is a second
			// note. Persisting the signed event before sending (step 2, PR 2)
			// makes this "natural".
			Idempotent: destination.IdempotentNone,
		},
	}
}

// Account is the Nostr identity whose key the Publisher holds.
type Account struct {
	P *Publisher
}

var (
	_ destination.Account     = (*Account)(nil)
	_ destination.Reposter    = (*Account)(nil)
	_ destination.Redeliverer = (*Account)(nil)
	_ destination.Identifier  = (*Account)(nil)
)

// Manifest implements destination.Account.
func (a *Account) Manifest() destination.Manifest { return Driver{}.Manifest() }

// Capabilities implements destination.Account.
func (a *Account) Capabilities(context.Context) destination.Capabilities {
	return Driver{}.Manifest().Defaults
}

// Publish implements destination.Account. ItemRef.Data is the signed event,
// which Redeliver re-sends unchanged.
func (a *Account) Publish(ctx context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	in := noteInput(req)
	trace, _ := json.Marshal(map[string]any{"text": in.Text, "pow": in.POW, "imeta": in.Imetas, "tags": in.Tags})
	out, err := a.publish(ctx, in)
	out.Trace.Request = string(trace)
	return out, err
}

// noteInput maps a request to a kind-1 note: imeta tags for the media, a
// NIP-36 content warning, NIP-10 reply tags, or a NIP-18 quote (a "q" tag,
// the commentary followed by a nostr:nevent mention of the quoted event).
func noteInput(req destination.PublishRequest) PublishInput {
	in := PublishInput{Kind: 1, Text: req.Text, Imetas: Imetas(req.Media), Tags: contentWarning(req.ContentWarning)}
	if n, ok := destination.OptInt(req.Options, OptPOW); ok {
		in.POW = &n
	}
	switch {
	case req.Quote != nil:
		src := *req.Quote
		hint := firstHint(src.RelayHints)
		content := strings.TrimSpace(req.Text)
		if mention := neventMention(src.EventID, src.Author, hint); mention != "" {
			if content == "" {
				content = mention
			} else {
				content = content + "\n" + mention
			}
		}
		in.Text = content
		in.Tags = append([]gonostr.Tag{{"q", src.EventID, hint, src.Author}}, in.Tags...)
	case req.InReplyTo != nil:
		// RelayHint is left empty: NIP-10 hints are optional.
		in.ReplyTo = &NostrReply{RootID: req.InReplyTo.Root.ID, ParentID: req.InReplyTo.Parent.ID}
	case req.ReplyTo != nil:
		in.ReplyTo = &NostrReply{RootID: req.ReplyTo.EventID, ParentID: req.ReplyTo.EventID, AuthorPubkey: req.ReplyTo.Author}
	}
	return in
}

// Imetas builds one NIP-92 imeta tag per media item that has a URL, with the
// item's alt text (NIP-92 "alt", newlines folded: entries are single-line).
func Imetas(items []destination.MediaItem) []gonostr.Tag {
	var out []gonostr.Tag
	for _, m := range items {
		if m.URL == "" {
			continue
		}
		dim := ""
		if m.Width > 0 && m.Height > 0 {
			dim = strconv.Itoa(m.Width) + "x" + strconv.Itoa(m.Height)
		}
		tag := media.ImetaTag(m.URL, m.Mime, m.SHA256, dim, m.Blurhash, m.PosterURL)
		if alt := strings.Join(strings.Fields(m.Alt), " "); alt != "" {
			tag = append(tag, "alt "+alt)
		}
		out = append(out, tag)
	}
	return out
}

// contentWarning is the NIP-36 tag that marks a note's content as sensitive,
// with the reason clients show before revealing it. Newlines are folded:
// clients render the reason on one line.
func contentWarning(reason string) []gonostr.Tag {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return nil
	}
	return []gonostr.Tag{{"content-warning", reason}}
}

// Repost implements destination.Reposter: NIP-18 kind 6 for a kind-1 note,
// kind 16 (generic repost with a "k" tag) for any other kind.
func (a *Account) Repost(ctx context.Context, src destination.SourceRef) (destination.PublishResult, error) {
	hint := firstHint(src.RelayHints)
	k := 6
	tags := []gonostr.Tag{{"e", src.EventID, hint}, {"p", src.Author}}
	if src.EventKind != 1 {
		k = 16
		tags = append(tags, gonostr.Tag{"k", strconv.Itoa(src.EventKind)})
	}
	return a.publish(ctx, PublishInput{Kind: k, Text: "", Tags: tags})
}

// publish runs Publish and maps relay outcomes to deliveries and a status:
// published when every dialed relay accepted, partial when some did, failed
// when none did. Skipped (overlay) relays don't count.
func (a *Account) publish(ctx context.Context, in PublishInput) (destination.PublishResult, error) {
	out := destination.PublishResult{Status: destination.StatusFailed}
	res, err := a.P.Publish(ctx, in)
	if err != nil {
		return out, destination.Classify(err)
	}
	b, _ := json.Marshal(res)
	out.Trace.Response = string(b)
	out.Ref = destination.ItemRef{ID: res.EventID, Data: json.RawMessage(res.SignedEvent)}
	if res.Nevent != "" {
		out.Ref.URL = "https://njump.me/" + res.Nevent
	}
	out.Pending = res.PendingRelays
	for _, rr := range res.Relays {
		st := "ok"
		switch {
		case rr.Skipped:
			st = "skipped"
		case !rr.OK:
			st = "failed"
		}
		out.Deliveries = append(out.Deliveries, destination.Delivery{Endpoint: rr.URL, Status: st, Message: rr.Message})
	}
	out.Status = StatusFromDeliveries(out.Deliveries)
	if out.Status == destination.StatusFailed {
		return out, &destination.Error{Class: destination.ClassTransient, Err: errNoRelay}
	}
	return out, nil
}

// StatusFromDeliveries derives an item status from relay outcomes, ignoring
// skipped relays.
func StatusFromDeliveries(ds []destination.Delivery) destination.ItemStatus {
	attempted, ok := 0, 0
	for _, d := range ds {
		if d.Status == "skipped" {
			continue
		}
		attempted++
		if d.Status == "ok" {
			ok++
		}
	}
	switch {
	case ok == 0:
		return destination.StatusFailed
	case ok < attempted:
		return destination.StatusPartial
	default:
		return destination.StatusPublished
	}
}

type relayErr string

func (e relayErr) Error() string { return string(e) }

const errNoRelay = relayErr("no relay accepted the event")

// Redeliver implements destination.Redeliverer: the same signed event (same
// id, no re-mining) to one relay.
func (a *Account) Redeliver(ctx context.Context, ref destination.ItemRef, endpoint string) destination.Delivery {
	rr := a.P.RebroadcastToRelay(ctx, string(ref.Data), endpoint)
	d := destination.Delivery{Endpoint: endpoint, Status: "ok", Message: rr.Message}
	if !rr.OK {
		d.Status = "failed"
	}
	return d
}

// Identity implements destination.Identifier.
func (a *Account) Identity(ctx context.Context) (destination.Identity, error) {
	npub, name, picture, err := a.P.OwnerProfile(ctx)
	if err != nil {
		return destination.Identity{}, err
	}
	return destination.Identity{Handle: npub, DisplayName: name, AvatarURL: picture,
		ProfileURL: "https://njump.me/" + npub}, nil
}

func firstHint(hints []string) string {
	if len(hints) > 0 {
		return hints[0]
	}
	return ""
}

// neventMention builds a "nostr:nevent…" mention for the quoted event, or ""
// when the id is malformed (nip19.EncodeNevent has no error return).
func neventMention(eventID, author, relayHint string) string {
	id, err := gonostr.IDFromHex(eventID)
	if err != nil {
		return ""
	}
	var relays []string
	if relayHint != "" {
		relays = []string{relayHint}
	}
	pk, _ := gonostr.PubKeyFromHex(author) // a zero key is acceptable to EncodeNevent
	nevent := nip19.EncodeNevent(id, relays, pk)
	if nevent == "" {
		return ""
	}
	return "nostr:" + nevent
}
