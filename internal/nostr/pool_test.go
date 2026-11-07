package nostr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/gorilla/websocket"

	"github.com/geofox/publisher/internal/destination"
)

// testRelay is a minimal NIP-01 relay: it keeps the events it is sent and
// answers a REQ with the matching ones and EOSE. It counts connections.
type testRelay struct {
	srv   *httptest.Server
	conns atomic.Int32
	mu    sync.Mutex
	evs   []gonostr.Event
	live  []*websocket.Conn
}

func newTestRelay(t *testing.T) *testRelay {
	tr := &testRelay{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.conns.Add(1)
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		tr.mu.Lock()
		tr.live = append(tr.live, c)
		tr.mu.Unlock()
		var wmu sync.Mutex
		send := func(v ...any) {
			b, _ := json.Marshal(v)
			wmu.Lock()
			_ = c.WriteMessage(websocket.TextMessage, b)
			wmu.Unlock()
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var arr []json.RawMessage
			if json.Unmarshal(msg, &arr) != nil || len(arr) < 2 {
				continue
			}
			var typ string
			_ = json.Unmarshal(arr[0], &typ)
			switch typ {
			case "EVENT":
				var ev gonostr.Event
				if json.Unmarshal(arr[1], &ev) != nil {
					continue
				}
				tr.mu.Lock()
				tr.evs = append(tr.evs, ev)
				tr.mu.Unlock()
				send("OK", ev.ID.Hex(), true, "")
			case "REQ":
				var sub string
				_ = json.Unmarshal(arr[1], &sub)
				var f gonostr.Filter
				if len(arr) > 2 {
					_ = json.Unmarshal(arr[2], &f)
				}
				tr.mu.Lock()
				evs := append([]gonostr.Event(nil), tr.evs...)
				tr.mu.Unlock()
				for _, ev := range evs {
					if f.Matches(ev) {
						send("EVENT", sub, ev)
					}
				}
				send("EOSE", sub)
			}
		}
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *testRelay) url() string { return "ws" + strings.TrimPrefix(tr.srv.URL, "http") }

// drop closes every connection the relay has open.
func (tr *testRelay) drop() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, c := range tr.live {
		_ = c.Close()
	}
	tr.live = nil
}

func signedNote(t *testing.T, sk gonostr.SecretKey, content string) gonostr.Event {
	t.Helper()
	ev := gonostr.Event{Kind: 1, CreatedAt: gonostr.Now(), Content: content, Tags: gonostr.Tags{}}
	if err := ev.Sign(sk); err != nil {
		t.Fatal(err)
	}
	return ev
}

func results(ch chan gonostr.PublishResult) []gonostr.PublishResult {
	var out []gonostr.PublishResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func refuseLoopback(_ string, ip net.IP) error {
	if ip.IsLoopback() {
		return errors.New("loopback")
	}
	return nil
}

// The pool dials every relay with its client: a client that refuses the
// address never reaches the relay, for a publish or a query.
func TestRelayPoolDialsThroughItsClient(t *testing.T) {
	tr := newTestRelay(t)
	p := newRelayPool(destination.GuardedNet(refuseLoopback).Client(0))
	defer p.Close("test")
	ev := signedNote(t, gonostr.Generate(), "hi")
	byName := strings.Replace(tr.url(), "127.0.0.1", "localhost", 1)
	rs := results(p.PublishMany(context.Background(), []string{byName}, ev))
	if len(rs) != 1 || !errors.Is(rs[0].Error, destination.ErrBlockedAddress) {
		t.Fatalf("results = %+v, want one ErrBlockedAddress", rs)
	}
	if strings.Contains(rs[0].Error.Error(), "127.0.0.1") || strings.Contains(rs[0].Error.Error(), "::1") {
		t.Errorf("the error says what the name resolved to: %v", rs[0].Error)
	}
	if re := p.QuerySingle(context.Background(), []string{tr.url(), byName}, gonostr.Filter{Limit: 1}, gonostr.SubscriptionOptions{}); re != nil {
		t.Errorf("query got %v", re.Event)
	}
	if n := tr.conns.Load(); n != 0 {
		t.Errorf("the relay was connected to %d times", n)
	}
}

// With a client that may reach it, the pool publishes once per relay
// (duplicates of a URL are one relay), keeps the connection for the next
// publish, queries, and reconnects after the relay drops it.
func TestRelayPoolPublishesAndQueries(t *testing.T) {
	tr := newTestRelay(t)
	p := newRelayPool(destination.TrustedNet().Client(0))
	defer p.Close("test")
	sk := gonostr.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev := signedNote(t, sk, "one")
	rs := results(p.PublishMany(ctx, []string{tr.url(), tr.url() + "/"}, ev))
	if len(rs) != 1 || rs[0].Error != nil || rs[0].Relay == nil {
		t.Fatalf("results = %+v, want one success", rs)
	}
	ev2 := signedNote(t, sk, "two")
	if rs := results(p.PublishMany(ctx, []string{tr.url()}, ev2)); len(rs) != 1 || rs[0].Error != nil {
		t.Fatalf("second publish = %+v", rs)
	}
	if n := tr.conns.Load(); n != 1 {
		t.Errorf("%d connections for two publishes, want 1", n)
	}
	re := p.QuerySingle(ctx, []string{tr.url()}, gonostr.Filter{IDs: []gonostr.ID{ev2.ID}}, gonostr.SubscriptionOptions{})
	if re == nil || re.Event.ID != ev2.ID {
		t.Fatalf("query = %v, want the second event", re)
	}
	if re := p.QuerySingle(ctx, []string{tr.url()}, gonostr.Filter{Kinds: []gonostr.Kind{7}}, gonostr.SubscriptionOptions{}); re != nil {
		t.Errorf("query for nothing = %v", re.Event)
	}
	tr.drop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		r := p.relays[gonostr.NormalizeURL(tr.url())]
		p.mu.Unlock()
		if r == nil || !r.IsConnected() || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rs := results(p.PublishMany(ctx, []string{tr.url()}, signedNote(t, sk, "three"))); len(rs) != 1 || rs[0].Error != nil {
		t.Fatalf("publish after a drop = %+v", rs)
	}
	if n := tr.conns.Load(); n != 2 {
		t.Errorf("%d connections after a drop, want 2", n)
	}
	p.Close("done")
	if rs := results(p.PublishMany(ctx, []string{tr.url()}, ev)); len(rs) != 1 || !errors.Is(rs[0].Error, errPoolClosed) {
		t.Errorf("publish after Close = %+v", rs)
	}
}

// An opened account dials its own relays (publishing, the NIP-65 lookup)
// through the host's NetAccount network: a host that refuses means the
// relay is never reached.
func TestAccountRelaysUseHostAccountNet(t *testing.T) {
	for _, allow := range []bool{false, true} {
		tr := newTestRelay(t)
		host := &guardHost{allow: allow}
		acct, err := Driver{}.Open(context.Background(), destination.OpenRequest{
			Config:  map[string]any{ConfigBootstrapRelay: tr.url()},
			Secrets: map[string]string{SecretKey: strings.Repeat("01", 32)},
			Tuning:  destination.Tuning{PublishTimeout: 5 * time.Second, RelayCacheTTL: time.Minute},
			Host:    host,
		})
		if err != nil {
			t.Fatal(err)
		}
		a := acct.(*Account)
		_, lookupErr := a.ResolveWriteRelays(context.Background())
		res := a.P.RebroadcastToRelay(context.Background(), mustJSON(t, signedNote(t, gonostr.Generate(), "x")), tr.url())
		_ = acct.(io.Closer).Close()
		if allow {
			if !res.OK || tr.conns.Load() == 0 {
				t.Errorf("allowed: rebroadcast = %+v, %d connections", res, tr.conns.Load())
			}
			continue
		}
		if lookupErr == nil || res.OK || tr.conns.Load() != 0 {
			t.Errorf("refused: lookup err %v, rebroadcast %+v, %d connections", lookupErr, res, tr.conns.Load())
		}
		if !slices.Contains(host.policies, destination.NetAccount) {
			t.Errorf("policies = %v, want NetAccount among them", host.policies)
		}
	}
}

// guardHost is an account host whose networks refuse loopback addresses
// unless allow is set.
type guardHost struct {
	allow    bool
	policies []destination.NetPolicy
}

func (h *guardHost) Save(context.Context, destination.StateUpdate) error { return nil }
func (h *guardHost) Logger() *slog.Logger                                { return slog.New(slog.DiscardHandler) }
func (h *guardHost) Net(p destination.NetPolicy) destination.Net {
	h.policies = append(h.policies, p)
	if h.allow {
		return destination.TrustedNet()
	}
	return destination.GuardedNet(refuseLoopback)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
