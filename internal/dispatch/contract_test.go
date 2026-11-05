package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// stubAccount is a destination.Account for contract tests: its manifest and
// capabilities are set per test, it records every request, and publish
// decides each outcome.
type stubAccount struct {
	manifest destination.Manifest
	caps     *destination.Capabilities // nil = manifest defaults
	publish  func(n int, req destination.PublishRequest) (destination.PublishResult, error)

	mu   sync.Mutex
	reqs []destination.PublishRequest
}

func (s *stubAccount) Manifest() destination.Manifest { return s.manifest }

func (s *stubAccount) Capabilities(context.Context) destination.Capabilities {
	if s.caps != nil {
		return *s.caps
	}
	return s.manifest.Defaults
}

func (s *stubAccount) Publish(_ context.Context, req destination.PublishRequest) (destination.PublishResult, error) {
	s.mu.Lock()
	n := len(s.reqs)
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	if s.publish != nil {
		return s.publish(n, req)
	}
	return published(n), nil
}

func published(n int) destination.PublishResult {
	return destination.PublishResult{Status: destination.StatusPublished,
		Ref: destination.ItemRef{ID: "id" + itoa(n), URL: "https://x/" + itoa(n), Data: []byte(`{"n":` + itoa(n) + `}`)}}
}

var zero = 0

// stubManifest is a microblog with a 20-character limit, two images per post
// and the given idempotency mode.
func stubManifest(mode destination.IdempotencyMode) destination.Manifest {
	return destination.Manifest{
		Contract: destination.ContractVersion, Type: "stub", Name: "Stub",
		Kinds: []destination.Kind{destination.KindNote},
		Options: []destination.OptionSpec{
			{Key: "reply_gate", Type: destination.OptionString},
			{Key: "reply_control", Type: destination.OptionEnum, Enum: []destination.EnumValue{{Value: "everyone"}}},
			{Key: "pow", Type: destination.OptionInt, Min: &zero},
		},
		Defaults: destination.Capabilities{
			Text:      destination.TextCaps{MaxLength: 20, Counting: destination.Counting{Unit: destination.UnitGraphemes}},
			Threading: destination.ThreadingCaps{Supported: true},
			Media: destination.MediaCaps{Transport: destination.TransportBytes,
				Images: destination.Limit{Supported: true, MaxCount: 2}},
			Languages:  1,
			Idempotent: mode,
		},
	}
}

func stubDispatcher(t *testing.T, acct *stubAccount) *Dispatcher {
	t.Helper()
	return &Dispatcher{Accounts: map[string]destination.Account{"stub": acct}, Store: openDispatchStore(t)}
}

// The chain is split by the account's own capabilities, and each item
// threads under the previous one with the driver's opaque reference data.
func TestChainUsesAccountCapsAndRefData(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	rec := d.Post(context.Background(), PostSpec{MasterText: "one two three four five six seven", Platforms: []string{"stub"}})
	if rec.Status != "success" || len(acct.reqs) < 2 {
		t.Fatalf("want a success chain, got %s with %d items", rec.Status, len(acct.reqs))
	}
	for i, r := range acct.reqs {
		if n := len([]rune(r.Text)); n > 20 {
			t.Errorf("item %d is %d chars, over the account's 20", i, n)
		}
		if i == 0 {
			if r.InReplyTo != nil {
				t.Errorf("head must not thread: %+v", r.InReplyTo)
			}
			continue
		}
		tr := r.InReplyTo
		if tr == nil || tr.Root.ID != "id0" || tr.Parent.ID != "id"+itoa(i-1) || string(tr.Parent.Data) != `{"n":`+itoa(i-1)+`}` {
			t.Errorf("item %d thread ref = %+v", i, tr)
		}
	}
	segs := rec.Targets[0].Segments
	if string(segs[1].Ref) != `{"n":1}` {
		t.Errorf("segment ref data not kept: %q", segs[1].Ref)
	}
}

// A segment written before Ref existed still threads: its CID becomes the
// Bluesky driver's ref data.
func TestSegRefLegacyCID(t *testing.T) {
	ref := segRef(store.Segment{RemoteID: "at://p", CID: "bafy"})
	if ref.ID != "at://p" || string(ref.Data) != `{"cid":"bafy"}` {
		t.Fatalf("legacy segment ref = %+v %s", ref, ref.Data)
	}
	ref = segRef(store.Segment{RemoteID: "x", Ref: []byte(`{"a":1}`), CID: "old"})
	if string(ref.Data) != `{"a":1}` {
		t.Fatalf("Ref must win over the legacy CID: %s", ref.Data)
	}
}

// Idempotency keys are sent only to destinations that honor them, stay the
// same across attempts at one item, and differ between items.
func TestIdempotencyKeys(t *testing.T) {
	calls := 0
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentKey),
		publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
			calls++
			if calls == 1 { // the first attempt at the head times out
				return destination.PublishResult{Status: destination.StatusFailed},
					&destination.Error{Class: destination.ClassUnknownOutcome, Err: context.DeadlineExceeded}
			}
			return published(n), nil
		}}
	d := stubDispatcher(t, acct)
	rec := d.Post(context.Background(), PostSpec{MasterText: "aaaa bbbb\n---\ncccc dddd", Platforms: []string{"stub"}})
	if rec.Targets[0].ErrorClass != string(destination.ClassUnknownOutcome) {
		t.Fatalf("error class = %q", rec.Targets[0].ErrorClass)
	}
	if _, err := d.Retry(context.Background(), rec.ID, nil); err != nil {
		t.Fatal(err)
	}
	if len(acct.reqs) != 3 {
		t.Fatalf("want head, head again, tail; got %d requests", len(acct.reqs))
	}
	k0, k1, k2 := acct.reqs[0].IdempotencyKey, acct.reqs[1].IdempotencyKey, acct.reqs[2].IdempotencyKey
	if k0 == "" || k0 != k1 {
		t.Errorf("a retry must repeat the item's key: %q vs %q", k0, k1)
	}
	if k2 == "" || k2 == k0 {
		t.Errorf("each item needs its own key: head %q, tail %q", k0, k2)
	}

	none := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	stubDispatcher(t, none).Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
	if none.reqs[0].IdempotencyKey != "" {
		t.Errorf("a destination without key support must get no key")
	}
}

func TestRequestCarriesOnlySupportedFields(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"},
		Overrides: map[string]Overrides{"stub": {
			Langs: []string{"fr", "en"}, Visibility: "unlisted", SpoilerText: "cw", Sensitive: true,
			BlueskyReply: "nobody", TopicTag: "go",
		}}})
	r := acct.reqs[0]
	if strings.Join(r.Languages, ",") != "fr" {
		t.Errorf("languages must be capped to the account's 1: %v", r.Languages)
	}
	if r.Visibility != "" || r.ContentWarning != "" || r.Sensitive {
		t.Errorf("unsupported fields must not be sent: %+v", r)
	}
	if len(r.Options) != 1 || r.Options["reply_gate"] != "nobody" {
		t.Errorf("only declared options are sent: %v", r.Options)
	}
}

// The composer's override fields fail open, as the old adapters did: a value
// the option doesn't accept is dropped and an int out of range is clamped,
// so a stale override never fails a post.
func TestLegacyOverridesFailOpen(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	pow := -3
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"},
		Overrides: map[string]Overrides{"stub": {ThreadsReplyControl: "bogus", POW: &pow}}})
	if rec.Status != "success" || len(acct.reqs) != 1 {
		t.Fatalf("post must go out; status %q, sent %d", rec.Status, len(acct.reqs))
	}
	opts := acct.reqs[0].Options
	if _, ok := opts["reply_control"]; ok {
		t.Errorf("an unaccepted enum value must be dropped: %v", opts)
	}
	if opts["pow"] != 0 {
		t.Errorf("a negative pow must be clamped to 0: %v", opts)
	}
}

func TestUnsupportedInteractionsAreInvalid(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	d := stubDispatcher(t, acct)
	for _, action := range []string{actionReply, actionQuote, actionRepost} {
		rec := d.Interact(context.Background(), InteractSpec{Action: action, SourcePlatform: "stub", Text: "hi",
			Ref: InteractRef{URI: "at://x"}})
		if tg := rec.Targets[0]; tg.Status != "failed" || tg.ErrorClass != string(destination.ClassInvalid) {
			t.Errorf("%s on a destination without it: %+v", action, tg)
		}
	}
	if len(acct.reqs) != 0 {
		t.Fatalf("nothing may be sent, got %d requests", len(acct.reqs))
	}
}

func TestNormalize(t *testing.T) {
	acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone)}
	live := destination.PublishResult{Status: destination.StatusPublished, Ref: destination.ItemRef{ID: "x"}}
	cases := []struct {
		res    destination.PublishResult
		err    error
		status string
		class  destination.ErrorClass
	}{
		{live, nil, "success", ""},
		{live, errors.New("gate write failed"), "partial", destination.ClassTransient},
		{destination.PublishResult{Status: destination.StatusPartial, Ref: destination.ItemRef{ID: "x"}}, nil, "partial", ""},
		{destination.PublishResult{}, nil, "failed", destination.ClassTransient}, // a driver bug never counts as success
		{destination.PublishResult{}, &destination.Error{Class: destination.ClassAuth, Err: errors.New("401")}, "failed", destination.ClassAuth},
	}
	for i, c := range cases {
		r := normalize("stub", acct, c.res, c.err)
		if r.Status != c.status || r.ErrorClass != string(c.class) || (r.Status != "success" && c.status != "partial" && r.Error == "") {
			t.Errorf("case %d: %+v", i, r)
		}
	}
	if r := normalize("stub", nil, destination.PublishResult{}, errors.New("x")); r.Status != "failed" {
		t.Errorf("no account: %+v", r)
	}
}

func TestUnconfiguredAccountIsAuth(t *testing.T) {
	d := &Dispatcher{Store: openDispatchStore(t)}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
	if tg := rec.Targets[0]; tg.ErrorClass != string(destination.ClassAuth) || !strings.Contains(tg.Attempts[0].Error, "not configured") {
		t.Fatalf("target = %+v", tg)
	}
}

// The retrier stops on failures a retry can't fix, alerts once, and keeps
// retrying transient ones.
func TestRetrierHonorsErrorClass(t *testing.T) {
	cases := []struct {
		class   destination.ErrorClass
		mode    destination.IdempotencyMode
		retried bool
	}{
		{destination.ClassTransient, destination.IdempotentNone, true},
		{destination.ClassRateLimited, destination.IdempotentNone, true},
		{destination.ClassAuth, destination.IdempotentNone, false},
		{destination.ClassInvalid, destination.IdempotentKey, false},
		{destination.ClassRejected, destination.IdempotentKey, false},
		{destination.ClassUnknownOutcome, destination.IdempotentNone, false},
		{destination.ClassUnknownOutcome, destination.IdempotentKey, true},
	}
	for _, c := range cases {
		t.Run(string(c.class)+"/"+string(c.mode), func(t *testing.T) {
			first := true
			acct := &stubAccount{manifest: stubManifest(c.mode),
				publish: func(n int, _ destination.PublishRequest) (destination.PublishResult, error) {
					if first {
						first = false
						return destination.PublishResult{}, &destination.Error{Class: c.class, Err: errors.New("boom")}
					}
					return published(n), nil
				}}
			d := stubDispatcher(t, acct)
			rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
			al := &fakeAlerter{}
			now := time.Now().UTC().Add(time.Hour)
			r := &Retrier{disp: d, notifier: al, enabled: true, maxAttempts: 6,
				base: time.Minute, max: time.Hour, now: func() time.Time { return now }}
			r.runDue(context.Background())

			got, _ := d.Store.GetPost(rec.ID)
			tg := got.Targets[0]
			if retried := len(acct.reqs) == 2; retried != c.retried {
				t.Fatalf("retried = %v, want %v (target %+v)", retried, c.retried, tg)
			}
			if !c.retried {
				if tg.GaveUpAt == nil || len(al.calls) != 1 || !strings.Contains(al.calls[0], "needs attention") {
					t.Fatalf("a hopeless failure must give up with one alert: gave_up=%v alerts=%v", tg.GaveUpAt, al.calls)
				}
				r.runDue(context.Background())
				if len(al.calls) != 1 {
					t.Fatalf("alert must fire once, got %v", al.calls)
				}
			} else if tg.Status != "success" {
				t.Fatalf("retry should have succeeded: %+v", tg)
			}
		})
	}
}

// The immediate failure alert must not promise an auto-retry the Retrier
// won't make.
func TestFailureAlertMatchesRetryClass(t *testing.T) {
	cases := []struct {
		name  string
		class destination.ErrorClass
		want  string
	}{
		{"transient", destination.ClassTransient, "auto-retry will attempt recovery"},
		{"invalid", destination.ClassInvalid, "will not be retried automatically"},
		{"unknown outcome, no idempotency", destination.ClassUnknownOutcome, "will not be retried automatically"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct := &stubAccount{manifest: stubManifest(destination.IdempotentNone),
				publish: func(int, destination.PublishRequest) (destination.PublishResult, error) {
					return destination.PublishResult{Status: destination.StatusFailed},
						&destination.Error{Class: tc.class, Err: errors.New("boom")}
				}}
			d := stubDispatcher(t, acct)
			al := &fakeAlerter{}
			d.Alerter = al
			d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
			if len(al.bodies) != 1 || !strings.Contains(al.bodies[0], tc.want) {
				t.Fatalf("alert bodies = %q, want one containing %q", al.bodies, tc.want)
			}
		})
	}
}

// A destination that drops repeats by key remembers the key for a limited
// time (Mastodon: an hour). Once an attempt that may have published is older
// than that, the Retrier stops, even if a later failure was transient.
func TestRetrierStopsWhenIdempotencyKeyExpires(t *testing.T) {
	cases := []struct {
		name    string
		classes []destination.ErrorClass // classes of the failed attempts, in order
		after   time.Duration            // how long after the first attempt the Retrier runs
		retried bool
	}{
		{"within the window", []destination.ErrorClass{destination.ClassUnknownOutcome}, 30 * time.Minute, true},
		{"past the window", []destination.ErrorClass{destination.ClassUnknownOutcome}, 2 * time.Hour, false},
		{"transient after an old unknown outcome",
			[]destination.ErrorClass{destination.ClassUnknownOutcome, destination.ClassTransient}, 2 * time.Hour, false},
		{"only transient failures", []destination.ErrorClass{destination.ClassTransient}, 2 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := stubManifest(destination.IdempotentKey)
			m.Defaults.IdempotencyKeyTTLSecs = 3600
			n := 0
			acct := &stubAccount{manifest: m,
				publish: func(i int, _ destination.PublishRequest) (destination.PublishResult, error) {
					if n < len(tc.classes) {
						c := tc.classes[n]
						n++
						return destination.PublishResult{}, &destination.Error{Class: c, Err: errors.New("boom")}
					}
					return published(i), nil
				}}
			d := stubDispatcher(t, acct)
			rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"stub"}})
			for range tc.classes[1:] { // the later failed attempts
				if _, err := d.Retry(context.Background(), rec.ID, nil); err != nil {
					t.Fatal(err)
				}
			}
			sent := len(acct.reqs)
			now := time.Now().UTC().Add(tc.after)
			r := &Retrier{disp: d, notifier: &fakeAlerter{}, enabled: true, maxAttempts: 6,
				base: time.Minute, max: time.Hour, now: func() time.Time { return now }}
			r.runDue(context.Background())
			if retried := len(acct.reqs) > sent; retried != tc.retried {
				got, _ := d.Store.GetPost(rec.ID)
				t.Fatalf("retried = %v, want %v (target %+v)", retried, tc.retried, got.Targets[0])
			}
		})
	}
}
