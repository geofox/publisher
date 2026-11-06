package relaysync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gonostr "fiatjaf.com/nostr"
	"github.com/gorilla/websocket"
)

// A relay page cut inside a group of same-second events loses the rest of
// that second: Fetch continues with Until = oldest-1 (io.go), and NIP-01's
// until is inclusive.
func TestFetchLosesSameSecondEventsAtPageBoundary(t *testing.T) {
	qaFetch(t, 2, 1000)
}

// A relay whose max limit is below PageLimit returns short pages; Fetch takes
// the first short page as "relay exhausted" (n < io.PageLimit) and stops.
func TestFetchStopsAtRelayLimitCap(t *testing.T) {
	qaFetch(t, 500, 2)
}

var qaRelayCap = 1000

func qaFetch(t *testing.T, pageLimit, relayCap int) {
	qaRelayCap = relayCap
	sk := gonostr.Generate()
	pk := gonostr.GetPublicKey(sk)
	var evs []gonostr.Event
	for i, ts := range []int64{200, 100, 100, 50} {
		ev := gonostr.Event{Kind: 1, CreatedAt: gonostr.Timestamp(ts), Content: strings.Repeat("x", i+1), Tags: gonostr.Tags{}}
		if err := ev.Sign(sk); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	query := func(f gonostr.Filter) []gonostr.Event {
		s := slices.Clone(evs)
		slices.SortFunc(s, func(a, b gonostr.Event) int { return int(b.CreatedAt) - int(a.CreatedAt) })
		var out []gonostr.Event
		for _, e := range s {
			if !f.Matches(e) {
				continue
			}
			if lim := min(f.Limit, qaRelayCap); lim > 0 && len(out) >= lim {
				break
			}
			out = append(out, e)
		}
		return out
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	rl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var arr []json.RawMessage
			if json.Unmarshal(msg, &arr) != nil || len(arr) < 3 {
				continue
			}
			var typ, sub string
			_ = json.Unmarshal(arr[0], &typ)
			_ = json.Unmarshal(arr[1], &sub)
			if typ != "REQ" {
				continue
			}
			var f gonostr.Filter
			if err := json.Unmarshal(arr[2], &f); err != nil {
				t.Errorf("filter: %v", err)
			}
			for _, e := range query(f) {
				b, _ := json.Marshal([]any{"EVENT", sub, e})
				_ = c.WriteMessage(websocket.TextMessage, b)
			}
			b, _ := json.Marshal([]any{"EOSE", sub})
			_ = c.WriteMessage(websocket.TextMessage, b)
		}
	})
	srv := httptest.NewServer(rl)
	defer srv.Close()
	io := &LiveIO{PageLimit: pageLimit, PageTimeout: 5 * time.Second, TotalTimeout: 10 * time.Second, MaxEvents: 100}
	got, err := io.Fetch(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), pk)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(evs) {
		t.Fatalf("fetched %d of %d events; events lost", len(got), len(evs))
	}
}
