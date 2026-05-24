package relaysync

import (
	"context"
	"strings"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
)

func mkEvent(sk gonostr.SecretKey, content string) gonostr.Event {
	ev := gonostr.Event{
		PubKey: gonostr.GetPublicKey(sk), CreatedAt: gonostr.Timestamp(time.Now().UnixNano()),
		Kind: 1, Content: content,
	}
	_ = ev.Sign(sk)
	return ev
}

func evmap(evs ...gonostr.Event) map[gonostr.ID]gonostr.Event {
	m := map[gonostr.ID]gonostr.Event{}
	for _, e := range evs {
		m[e.ID] = e
	}
	return m
}

func mkKind(sk gonostr.SecretKey, kind int, createdAt int64, dval string) gonostr.Event {
	ev := gonostr.Event{PubKey: gonostr.GetPublicKey(sk), CreatedAt: gonostr.Timestamp(createdAt), Kind: gonostr.Kind(kind)}
	if dval != "" {
		ev.Tags = gonostr.Tags{{"d", dval}}
	}
	_ = ev.Sign(sk)
	return ev
}

func TestDiffReplaceableAware(t *testing.T) {
	sk := gonostr.Generate()
	homeRelayList := mkKind(sk, 10002, 100, "")  // home's current relay list
	staleRelayList := mkKind(sk, 10002, 50, "")  // target's older copy (the nostr.land phantom)
	homeProfileOld := mkKind(sk, 0, 100, "")     // home's older profile
	targetProfileNew := mkKind(sk, 0, 200, "")   // target has a NEWER profile
	homeAppOld := mkKind(sk, 30078, 100, "cfg")  // addressable, d=cfg, home older
	targetAppNew := mkKind(sk, 30078, 200, "cfg") // addressable, d=cfg, target newer
	note := mkKind(sk, 1, 150, "")               // regular note only on target

	home := evmap(homeRelayList, homeProfileOld, homeAppOld)
	target := evmap(staleRelayList, targetProfileNew, targetAppNew, note)

	mh, _ := diff(home, target)
	got := map[gonostr.ID]bool{}
	for _, e := range mh {
		got[e.ID] = true
	}
	if got[staleRelayList.ID] {
		t.Error("stale older 10002 must NOT be a pull candidate (home has newer)")
	}
	if !got[targetProfileNew.ID] {
		t.Error("newer profile (kind 0) MUST be a pull candidate")
	}
	if !got[targetAppNew.ID] {
		t.Error("newer addressable (30078,d=cfg) MUST be a pull candidate")
	}
	if !got[note.ID] {
		t.Error("regular note MUST be a pull candidate")
	}
	if len(mh) != 3 {
		t.Errorf("missingAtHome = %d, want 3 (newer profile + newer app + note)", len(mh))
	}
}

// fakeIO: per-relay event sets + optional fetch errors; records publishes.
// pubFail>0 reports that many per-event failures with pubDetail as the summary.
type fakeIO struct {
	events    map[string]map[gonostr.ID]gonostr.Event
	fetchErr  map[string]error
	published map[string][]gonostr.ID
	pubFail   int
	pubDetail string
}

func (f *fakeIO) Fetch(ctx context.Context, relayURL string, _ gonostr.PubKey) (map[gonostr.ID]gonostr.Event, error) {
	if e := f.fetchErr[relayURL]; e != nil {
		return nil, e
	}
	return f.events[relayURL], nil
}
func (f *fakeIO) Publish(ctx context.Context, relayURL string, events []gonostr.Event) (int, int, string, error) {
	if f.pubFail > 0 {
		pub := len(events) - f.pubFail
		if pub < 0 {
			pub = 0
		}
		return pub, f.pubFail, f.pubDetail, nil
	}
	if f.published == nil {
		f.published = map[string][]gonostr.ID{}
	}
	for _, e := range events {
		f.published[relayURL] = append(f.published[relayURL], e.ID)
	}
	return len(events), 0, "", nil
}

func TestDiff(t *testing.T) {
	sk := gonostr.Generate()
	a, b, c := mkEvent(sk, "a"), mkEvent(sk, "b"), mkEvent(sk, "c")
	home := evmap(a, b)
	target := evmap(b, c)
	mh, mr := diff(home, target)
	if len(mh) != 1 || mh[0].ID != c.ID {
		t.Errorf("missingAtHome = %v, want [c]", mh)
	}
	if len(mr) != 1 || mr[0].ID != a.ID {
		t.Errorf("missingAtRelay = %v, want [a]", mr)
	}
}

func TestResolveTargets(t *testing.T) {
	got := ResolveTargets(
		[]string{"wss://a", "wss://home/", "wss://b"},
		[]string{"wss://b", "wss://c", "ws://x.onion"},
		"wss://home",
	)
	if len(got) != 3 {
		t.Fatalf("targets = %d (%v), want 3", len(got), got)
	}
	byURL := map[string]string{}
	for _, tg := range got {
		byURL[tg.URL] = tg.Group
	}
	if byURL["wss://a"] != "nip65" || byURL["wss://b"] != "nip65" || byURL["wss://c"] != "secondary" {
		t.Errorf("groups wrong: %v", byURL)
	}
	if _, ok := byURL["wss://home"]; ok {
		t.Errorf("home not excluded")
	}
}

func TestScanCountsAndStatus(t *testing.T) {
	sk := gonostr.Generate()
	a, b, c := mkEvent(sk, "a"), mkEvent(sk, "b"), mkEvent(sk, "c")
	io := &fakeIO{
		events: map[string]map[gonostr.ID]gonostr.Event{
			"wss://home": evmap(a, b),
			"wss://t1":   evmap(b, c),
		},
		fetchErr: map[string]error{"wss://t2": ErrRelayAuth},
	}
	s := New(io, "wss://home", gonostr.GetPublicKey(sk))
	out := s.Scan(context.Background(), []Target{{URL: "wss://t1", Group: "nip65"}, {URL: "wss://t2", Group: "secondary"}})
	if len(out) != 2 {
		t.Fatalf("scan len = %d", len(out))
	}
	if out[0].MissingAtHome != 1 || out[0].MissingAtRelay != 1 || out[0].Status != "ok" {
		t.Errorf("t1 diff wrong: %+v", out[0])
	}
	if out[1].Status != "auth" {
		t.Errorf("t2 status = %q, want auth", out[1].Status)
	}
}

func TestApplyPushAndPull(t *testing.T) {
	sk := gonostr.Generate()
	a, b, c := mkEvent(sk, "a"), mkEvent(sk, "b"), mkEvent(sk, "c")
	mk := func() *fakeIO {
		return &fakeIO{events: map[string]map[gonostr.ID]gonostr.Event{
			"wss://home": evmap(a, b),
			"wss://t1":   evmap(b, c),
		}}
	}
	owner := gonostr.GetPublicKey(sk)
	io := mk()
	New(io, "wss://home", owner).Apply(context.Background(), []Target{{URL: "wss://t1"}}, "pull")
	if got := io.published["wss://home"]; len(got) != 1 || got[0] != c.ID {
		t.Errorf("pull published to home = %v, want [c]", got)
	}
	io = mk()
	New(io, "wss://home", owner).Apply(context.Background(), []Target{{URL: "wss://t1"}}, "push")
	if got := io.published["wss://t1"]; len(got) != 1 || got[0] != a.ID {
		t.Errorf("push published to t1 = %v, want [a]", got)
	}
}

func TestApplyInSyncSkipsPublish(t *testing.T) {
	sk := gonostr.Generate()
	a, b := mkEvent(sk, "a"), mkEvent(sk, "b")
	io := &fakeIO{events: map[string]map[gonostr.ID]gonostr.Event{
		"wss://home": evmap(a, b),
		"wss://t1":   evmap(a, b), // identical → nothing to publish
	}}
	out := New(io, "wss://home", gonostr.GetPublicKey(sk)).Apply(context.Background(), []Target{{URL: "wss://t1"}}, "push")
	if len(io.published) != 0 {
		t.Errorf("in-sync relay should skip publish, got %v", io.published)
	}
	if out[0].Status != "ok" || out[0].Published != 0 {
		t.Errorf("in-sync result = %+v, want ok/0", out[0])
	}
}

func TestApplyHomeFetchFails(t *testing.T) {
	sk := gonostr.Generate()
	io := &fakeIO{
		events:   map[string]map[gonostr.ID]gonostr.Event{"wss://t1": evmap(mkEvent(sk, "x"))},
		fetchErr: map[string]error{"wss://home": ErrRelayUnreachable},
	}
	out := New(io, "wss://home", gonostr.GetPublicKey(sk)).Apply(context.Background(), []Target{{URL: "wss://t1"}}, "pull")
	if out[0].Status != "error" || !strings.Contains(out[0].Message, "home fetch failed") {
		t.Errorf("home-fetch-fail result = %+v, want error/home-fetch-failed", out[0])
	}
}

func TestApplySurfacesFailureDetail(t *testing.T) {
	sk := gonostr.Generate()
	a, b := mkEvent(sk, "a"), mkEvent(sk, "b")
	io := &fakeIO{
		events: map[string]map[gonostr.ID]gonostr.Event{
			"wss://home": evmap(a, b),
			"wss://t1":   evmap(),
		},
		pubFail: 1, pubDetail: "restricted: sign up ×1",
	}
	out := New(io, "wss://home", gonostr.GetPublicKey(sk)).Apply(context.Background(), []Target{{URL: "wss://t1"}}, "push")
	if out[0].Status != "partial" || out[0].Message != "restricted: sign up ×1" {
		t.Errorf("result = %+v, want partial + detail message", out[0])
	}
	if out[0].Published != 1 || out[0].Failed != 1 {
		t.Errorf("counts = pub %d fail %d, want 1/1", out[0].Published, out[0].Failed)
	}
}

func TestSummarizeReasons(t *testing.T) {
	if got := summarizeReasons(map[string]int{"rate-limited": 3, "blocked": 1}); got != "rate-limited ×3; blocked ×1" {
		t.Errorf("summarize = %q", got)
	}
	if got := summarizeReasons(nil); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
}

func TestCleanReason(t *testing.T) {
	if got := cleanReason("msg: restricted: sign up"); got != "restricted: sign up" {
		t.Errorf("cleanReason = %q", got)
	}
	if got := cleanReason("   "); got != "rejected" {
		t.Errorf("blank reason = %q, want rejected", got)
	}
}

func TestDiffSkipsEphemeral(t *testing.T) {
	sk := gonostr.Generate()
	homeEph := mkKind(sk, 22456, 100, "")   // ephemeral on home (the rejected one)
	targetEph := mkKind(sk, 21000, 100, "") // ephemeral on target
	note := mkKind(sk, 1, 100, "")          // regular note on home, not target
	home := evmap(homeEph, note)
	target := evmap(targetEph)

	mh, mr := diff(home, target)
	for _, e := range mh {
		if e.ID == targetEph.ID {
			t.Error("ephemeral must NOT be a pull candidate")
		}
	}
	for _, e := range mr {
		if e.ID == homeEph.ID {
			t.Error("ephemeral must NOT be a push candidate")
		}
	}
	if len(mh) != 0 {
		t.Errorf("missingAtHome = %d, want 0", len(mh))
	}
	if len(mr) != 1 || mr[0].ID != note.ID {
		t.Errorf("missingAtRelay = %v, want [note]", mr)
	}
}
