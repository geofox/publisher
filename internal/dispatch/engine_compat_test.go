package dispatch

import (
	"context"
	"encoding/json"
	"github.com/geofox/publisher/internal/thread"
	"os"
	"path/filepath"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/unfurl"
)

// The chain tests predate the engine: they called runChain (post a target
// now) and resumeSegments (continue a stored thread) directly. These
// helpers keep their assertions running against the engine: each one plans
// and saves a post, drives it, and reports the recorded target in the old
// outcome shape.

var testDBDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dispatch-test-*")
	if err != nil {
		panic(err)
	}
	testDBDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testStore gives a store-less test dispatcher one.
func (d *Dispatcher) testStore() *store.Store {
	if d.Store == nil {
		st, err := store.Open(filepath.Join(testDBDir, newID()+".db"))
		if err != nil {
			panic(err)
		}
		d.Store = st
	}
	return d.Store
}

// chainOutcome is a target as the old chain functions reported it.
type chainOutcome struct {
	Platform                    string
	Status                      string
	Error                       string
	ErrorClass                  string
	FinalText                   string
	LinkCard                    *unfurl.Card
	HeadRemoteID, HeadRemoteURL string
	Relays                      []store.RelayState
	SignedEventJSON             string
	RequestJSON, ResponseJSON   string
	Segments                    []store.Segment
}

func outcomeOf(tg store.Target) chainOutcome {
	o := chainOutcome{Platform: tg.Platform, Status: tg.Status, ErrorClass: tg.ErrorClass, FinalText: tg.FinalText,
		HeadRemoteID: tg.RemoteID, HeadRemoteURL: tg.RemoteURL, Relays: tg.Relays, SignedEventJSON: tg.SignedEventJSON,
		Segments: tg.Segments}
	var ov Overrides
	_ = json.Unmarshal([]byte(tg.FieldsJSON), &ov)
	o.LinkCard = ov.LinkCard
	if n := len(tg.Attempts); n > 0 {
		o.Error = tg.Attempts[n-1].Error
		o.RequestJSON, o.ResponseJSON = tg.Attempts[0].RequestJSON, tg.Attempts[0].ResponseJSON
	}
	return o
}

func mediaRecords(imgs []Img) []store.Media {
	out := make([]store.Media, len(imgs))
	for i, im := range imgs {
		out[i] = store.Media{Ordinal: i, BlossomURL: im.BlossomURL, SHA256: im.SHA256, Mime: im.Mime, Dim: im.Dim,
			Blurhash: im.Blurhash, SizeBytes: im.SizeBytes, Alt: im.Alt, DurationSecs: im.DurationSecs, PosterURL: im.PosterURL}
	}
	return out
}

// freshID is postID unless the store already has it.
func (d *Dispatcher) freshID(postID string) string {
	if postID == "" {
		return newID()
	}
	if _, err := d.testStore().GetPost(postID); err == nil {
		return postID + "-" + newID()
	}
	return postID
}

// runChain posts text to plat as a new post and returns the target.
func (d *Dispatcher) runChain(ctx context.Context, plat, text string, ov Overrides, imgs []Img, number bool, imgParts []int, head *headSpec, postID string) chainOutcome {
	st := d.testStore()
	id := d.freshID(postID)
	now := d.now()
	// Cases write breaks the old way, as "---" lines, and place media by part.
	text, breaks, parts := thread.FromMarkers(text)
	lay := thread.Layout{Breaks: breaks}
	for _, p := range imgParts {
		lay.MediaAt = append(lay.MediaAt, parts[min(max(p, 0), len(parts)-1)])
	}
	tg := d.planTarget(ctx, id, TargetSpec{AccountID: acct(plat), Type: plat, Text: text, Overrides: ov, Layout: lay},
		postMedia(len(imgs)), number, &now, "sending")
	rec := &store.Post{ID: id, CreatedAt: now, MasterText: text, Platforms: []string{plat}, Source: "test",
		Status: "sending", Media: mediaRecords(imgs), Targets: []store.Target{tg}}
	if head != nil {
		ix := &store.Interaction{SourcePlatform: plat}
		switch {
		case head.reply != nil:
			ix.Action, ix.Ref = actionReply, marshalRef(*head.reply)
		case head.quote != nil:
			ix.Action, ix.Ref = actionQuote, marshalRef(*head.quote)
		}
		rec.Interaction = ix
	}
	if err := d.drive(ctx, id, &memMedia{post: imgs}, func() error { return saveWithAccounts(st, rec) }); err != nil {
		panic(err)
	}
	post, err := st.GetPost(id)
	if err != nil {
		panic(err)
	}
	return outcomeOf(post.Targets[0])
}

// resumeSegments stores tg (a thread part-way through) on a new post with
// imgs as its media, retries it, and returns the target.
func (d *Dispatcher) resumeSegments(ctx context.Context, tg store.Target, ov Overrides, imgs []Img, postID string) chainOutcome {
	st := d.testStore()
	id := d.freshID(postID)
	tg.ID = 0
	if tg.Status == "" {
		tg.Status = "partial"
	}
	if tg.FieldsJSON == "" {
		fields, _ := json.Marshal(ov2fields(ov))
		tg.FieldsJSON = string(fields)
	}
	rec := &store.Post{ID: id, CreatedAt: d.now(), Platforms: []string{tg.Platform}, Source: "test", Status: "partial",
		Media: mediaRecords(imgs), Targets: []store.Target{tg}}
	if err := saveWithAccounts(st, rec); err != nil {
		panic(err)
	}
	saved, err := st.GetPost(id)
	if err != nil {
		panic(err)
	}
	d.drive(ctx, id, &memMedia{post: imgs}, func() error {
		_, err := st.MarkDue(ctx, id, []int64{saved.Targets[0].ID}, d.now())
		return err
	})
	post, err := st.GetPost(id)
	if err != nil {
		panic(err)
	}
	return outcomeOf(post.Targets[0])
}

// segRef is the reference a later segment replies to.
func segRef(sg store.Segment) destination.ItemRef {
	ref := destination.ItemRef{ID: sg.RemoteID, URL: sg.RemoteURL, Data: sg.Ref}
	if len(ref.Data) == 0 && sg.CID != "" {
		b, _ := json.Marshal(map[string]string{"cid": sg.CID})
		ref.Data = b
	}
	return ref
}

// marked is spec with its "---" lines turned into breaks for every platform.
func marked(spec typeSpec) typeSpec {
	text, breaks, _ := thread.FromMarkers(spec.MasterText)
	spec.MasterText = text
	spec.Layouts = map[string]thread.Layout{}
	for _, p := range spec.Platforms {
		spec.Layouts[p] = thread.Layout{Breaks: breaks}
	}
	return spec
}
