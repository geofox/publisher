package dispatch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// flakyMasto fails the first call of any kind, then succeeds, recording what
// each call was so a test can see how a retry re-ran the target.
type flakyMasto struct {
	calls []string // "post", "reblog:<id>", "quote:<id>"
	reply []*ReplyRef
}

func (m *flakyMasto) result() legacyResult {
	if len(m.calls) == 1 {
		return legacyResult{Platform: "mastodon", Status: "failed", Error: "timeout"}
	}
	return legacyResult{Platform: "mastodon", Status: "success", RemoteID: "s2", RemoteURL: "https://m/2"}
}

func (m *flakyMasto) PostText(_ context.Context, _ string, _ Overrides, _ []Img, r *ReplyRef) (legacyResult, error) {
	m.calls = append(m.calls, "post")
	m.reply = append(m.reply, r)
	return m.result(), nil
}
func (m *flakyMasto) Reblog(_ context.Context, id string) (legacyResult, error) {
	m.calls = append(m.calls, "reblog:"+id)
	return m.result(), nil
}
func (m *flakyMasto) QuoteStatus(_ context.Context, _ string, id string, _ []Img) (legacyResult, error) {
	m.calls = append(m.calls, "quote:"+id)
	return m.result(), nil
}

func newInteractRetryDispatcher(t *testing.T, m *flakyMasto) *Dispatcher {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Dispatcher{Registry: legacyAccounts(legacy{Mastodon: m}), Store: db, Fetcher: fakeFetcher{}}
}

// A failed interaction must be retried as the same interaction. Before the fix
// a retried reply went out as a top-level post, a quote as a plain post, and a
// repost as an empty status.
func TestRetryRepeatsInteraction(t *testing.T) {
	cases := []struct {
		action string
		want   string
	}{
		{actionReply, "post"},
		{actionQuote, "quote:109"},
		{actionRepost, "reblog:109"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			m := &flakyMasto{}
			d := newInteractRetryDispatcher(t, m)
			p := d.Interact(context.Background(), typeInteract{
				Action: tc.action, SourcePlatform: "mastodon",
				Ref: InteractRef{LocalID: "109"}, Text: "commentary",
			})
			if p.Status != "failed" {
				t.Fatalf("first attempt status = %q, want failed", p.Status)
			}
			got, err := d.Retry(context.Background(), p.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.calls) != 2 || m.calls[1] != tc.want {
				t.Fatalf("calls = %v, want retry %q", m.calls, tc.want)
			}
			if tc.action == actionReply {
				if r := m.reply[1]; r == nil || r.ParentID != "109" {
					t.Fatalf("retried reply lost its parent: %+v", r)
				}
			}
			if got.Status != "success" {
				t.Fatalf("post status after retry = %q, want success", got.Status)
			}
		})
	}
}

// A post recorded before Interaction.Ref existed can't repeat its action: the
// retry records a failed attempt (so the Retrier's cap still applies) and
// never calls the platform.
func TestRetryInteractionWithoutRefFailsClosed(t *testing.T) {
	m := &flakyMasto{}
	d := newInteractRetryDispatcher(t, m)
	now := time.Now().UTC()
	if err := saveWithAccounts(d.Store, &store.Post{
		ID: "old", CreatedAt: now, Platforms: []string{"mastodon"}, Source: "web", Status: "failed",
		Interaction: &store.Interaction{Action: actionReply, SourcePlatform: "mastodon", SourceURL: "https://m/@a/109"},
		Targets: []store.Target{{Platform: "mastodon", Status: "failed", FinalText: "my reply",
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed", Error: "timeout", AttemptedAt: now}}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Retry(context.Background(), "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("platform called %v; a ref-less interaction must not be re-posted", m.calls)
	}
	tg := got.Targets[0]
	if tg.Status != "failed" || tg.AttemptCount != 2 {
		t.Fatalf("target = %+v, want failed with 2 attempts", tg)
	}
	if last := tg.Attempts[len(tg.Attempts)-1]; !strings.Contains(last.Error, "source reference") {
		t.Fatalf("last attempt error = %q", last.Error)
	}
}

// A threaded reply whose head failed is resumed with the head still replying
// to the source, and the tail threading under the new head.
func TestRetryResumesThreadedReplyHead(t *testing.T) {
	m := &flakyMasto{}
	d := newInteractRetryDispatcher(t, m)
	long := strings.Repeat("word ", 150) // 750 chars: two Mastodon segments
	p := d.Interact(context.Background(), typeInteract{
		Action: actionReply, SourcePlatform: "mastodon", Ref: InteractRef{LocalID: "109"}, Text: long,
	})
	if p.Status != "failed" || len(p.Targets[0].Segments) < 2 {
		t.Fatalf("setup: status %q segments %d", p.Status, len(p.Targets[0].Segments))
	}
	if _, err := d.Retry(context.Background(), p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.reply) < 3 {
		t.Fatalf("calls = %v, want head retry plus tail", m.calls)
	}
	if r := m.reply[1]; r == nil || r.ParentID != "109" {
		t.Fatalf("resumed head lost its parent: %+v", r)
	}
	if r := m.reply[2]; r == nil || r.ParentID != "s2" {
		t.Fatalf("tail does not thread under the new head: %+v", r)
	}
}
