package nostr

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
)

// capturePool accepts every event and remembers each one's id.
type capturePool struct {
	mu  sync.Mutex
	ids []string
}

func (c *capturePool) PublishMany(_ context.Context, urls []string, ev gonostr.Event) chan gonostr.PublishResult {
	c.mu.Lock()
	c.ids = append(c.ids, ev.ID.Hex())
	c.mu.Unlock()
	ch := make(chan gonostr.PublishResult, len(urls))
	for _, u := range urls {
		ch <- gonostr.PublishResult{RelayURL: u}
	}
	close(ch)
	return ch
}

func (c *capturePool) QuerySingle(context.Context, []string, gonostr.Filter, gonostr.SubscriptionOptions) *gonostr.RelayEvent {
	return nil
}

func testAccount(pool publishPool) *Account {
	sk := gonostr.Generate()
	return &Account{P: &Publisher{
		cfg: Config{NSEC: sk, OwnerPubkey: gonostr.GetPublicKey(sk), NIP65BootstrapRelay: "wss://a",
			FallbackRelays: []string{"wss://a"}, PublishTimeout: 5 * time.Second, POWTimeout: time.Second},
		pool: pool, cache: map[gonostr.PubKey]relayCacheEntry{},
	}}
}

// Every attempt at a prepared note broadcasts the same signed event, so
// relays dedupe a repeat.
func TestPreparedEventIsSentUnchanged(t *testing.T) {
	pool := &capturePool{}
	a := testAccount(pool)
	req := destination.PublishRequest{Kind: destination.KindNote, Text: "hello"}
	prep, err := a.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var ev gonostr.Event
	if err := json.Unmarshal(prep, &ev); err != nil {
		t.Fatal(err)
	}
	req.Prepared = prep
	for i := 0; i < 2; i++ {
		res, err := a.Publish(context.Background(), req)
		if err != nil || res.Status != destination.StatusPublished || res.Ref.ID != ev.ID.Hex() {
			t.Fatalf("attempt %d: %+v, %v", i, res, err)
		}
	}
	if len(pool.ids) != 2 || pool.ids[0] != ev.ID.Hex() || pool.ids[1] != ev.ID.Hex() {
		t.Fatalf("broadcast ids %v, want the prepared %s twice", pool.ids, ev.ID.Hex())
	}
}

// A stored event that isn't the owner's, or was altered, is never sent.
func TestPreparedEventMustBeTheOwners(t *testing.T) {
	pool := &capturePool{}
	a := testAccount(pool)
	other := testAccount(pool)
	foreign, err := other.Prepare(context.Background(), destination.PublishRequest{Kind: destination.KindNote, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	own, _ := a.Prepare(context.Background(), destination.PublishRequest{Kind: destination.KindNote, Text: "x"})
	var ev gonostr.Event
	_ = json.Unmarshal(own, &ev)
	ev.Content = "altered"
	altered, _ := json.Marshal(ev)
	for name, raw := range map[string]json.RawMessage{"foreign": foreign, "altered": altered, "garbage": json.RawMessage(`{`)} {
		_, err := a.Publish(context.Background(), destination.PublishRequest{Kind: destination.KindNote, Text: "x", Prepared: raw})
		if destination.ClassOf(err) != destination.ClassInvalid {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	if len(pool.ids) != 0 {
		t.Errorf("sent %d events", len(pool.ids))
	}
}
