package relaysync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gonostr "fiatjaf.com/nostr"
)

// clientIO records the client each relay was dialed with.
type clientIO struct {
	fakeIO
	fetched, published map[string]*http.Client
}

func (c *clientIO) Fetch(ctx context.Context, client *http.Client, url string, pk gonostr.PubKey) (map[gonostr.ID]gonostr.Event, error) {
	c.fetched[url] = client
	return c.fakeIO.Fetch(ctx, client, url, pk)
}

func (c *clientIO) Publish(ctx context.Context, client *http.Client, url string, evs []gonostr.Event) (int, int, string, error) {
	c.published[url] = client
	return c.fakeIO.Publish(ctx, client, url, evs)
}

// The home relay is dialed with the account's client; the targets (owner
// set) with the target client, in both directions.
func TestSyncDialsEachRelayWithItsClient(t *testing.T) {
	sk := gonostr.Generate()
	ev := gonostr.Event{Kind: 1, CreatedAt: 1, Tags: gonostr.Tags{}}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	home, target := &http.Client{}, &http.Client{}
	who := Owner{Home: "wss://home", Pubkey: gonostr.GetPublicKey(sk), HomeClient: home, TargetClient: target}
	for _, dir := range []string{"pull", "push"} {
		io := &clientIO{fetched: map[string]*http.Client{}, published: map[string]*http.Client{}}
		io.events = map[string]map[gonostr.ID]gonostr.Event{"wss://home": {}, "wss://t1": {}}
		if dir == "pull" {
			io.events["wss://t1"][ev.ID] = ev
		} else {
			io.events["wss://home"][ev.ID] = ev
		}
		New(io).Apply(context.Background(), who, []Target{{URL: "wss://t1"}}, dir)
		if io.fetched["wss://home"] != home || io.fetched["wss://t1"] != target {
			t.Errorf("%s: fetch clients wrong", dir)
		}
		dest, want := "wss://t1", target
		if dir == "pull" {
			dest, want = "wss://home", home
		}
		if got, ok := io.published[dest]; !ok || got != want {
			t.Errorf("%s: published to %s with the wrong client (published %v)", dir, dest, io.published)
		}
	}
	io := &clientIO{fetched: map[string]*http.Client{}, published: map[string]*http.Client{}}
	New(io).Scan(context.Background(), who, []Target{{URL: "wss://t1"}})
	if io.fetched["wss://home"] != home || io.fetched["wss://t1"] != target {
		t.Error("scan: fetch clients wrong")
	}
}

// LiveIO dials with the client it is given, and refuses without one.
func TestLiveIODialsWithItsClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	refused := errors.New("refused by policy")
	deny := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, refused })}
	io := NewLiveIO()
	pk := gonostr.GetPublicKey(gonostr.Generate())
	ev := gonostr.Event{Kind: 1, Tags: gonostr.Tags{}}
	for name, c := range map[string]*http.Client{"nil": nil, "denying": deny} {
		if _, err := io.Fetch(context.Background(), c, url, pk); !errors.Is(err, ErrRelayUnreachable) {
			t.Errorf("%s fetch: %v", name, err)
		}
		if _, _, _, err := io.Publish(context.Background(), c, url, []gonostr.Event{ev}); !errors.Is(err, ErrRelayUnreachable) {
			t.Errorf("%s publish: %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the relay was reached %d times", hits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
