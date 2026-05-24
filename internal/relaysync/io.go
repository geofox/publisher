package relaysync

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gonostr "fiatjaf.com/nostr"
)

// LiveIO is the production relayIO: it talks to real relays over WebSocket.
type LiveIO struct {
	PageLimit    int           // events per REQ page (e.g. 500)
	PageTimeout  time.Duration // per-publish deadline (e.g. 8s)
	TotalTimeout time.Duration // whole-operation deadline (e.g. 60s)
	MaxEvents    int           // safety cap per relay (e.g. 50000)
}

// NewLiveIO returns a LiveIO with sane defaults.
func NewLiveIO() *LiveIO {
	return &LiveIO{PageLimit: 500, PageTimeout: 8 * time.Second, TotalTimeout: 60 * time.Second, MaxEvents: 50000}
}

var _ relayIO = (*LiveIO)(nil) // compile-time check LiveIO satisfies the engine's interface

func (io *LiveIO) Fetch(ctx context.Context, relayURL string, pubkey gonostr.PubKey) (map[gonostr.ID]gonostr.Event, error) {
	cctx, cancel := context.WithTimeout(ctx, io.TotalTimeout)
	defer cancel()
	relay, err := gonostr.RelayConnect(cctx, relayURL, gonostr.RelayOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRelayUnreachable, err)
	}
	defer relay.Close()

	out := map[gonostr.ID]gonostr.Event{}
	var until gonostr.Timestamp // 0 = no upper bound (relay treats as "now")
	for {
		filter := gonostr.Filter{Authors: []gonostr.PubKey{pubkey}, Limit: io.PageLimit}
		if until > 0 {
			filter.Until = until
		}
		// QueryEvents takes no context; the whole fetch is bounded by cctx
		// (TotalTimeout), which closes the relay connection on expiry.
		n := 0
		var oldest gonostr.Timestamp
		for evt := range relay.QueryEvents(filter) {
			out[evt.ID] = evt
			n++
			if oldest == 0 || evt.CreatedAt < oldest {
				oldest = evt.CreatedAt
			}
			if len(out) >= io.MaxEvents {
				break
			}
		}
		// Stop when a short page (relay exhausted), cap hit, or no progress.
		if n < io.PageLimit || len(out) >= io.MaxEvents || oldest == 0 {
			break
		}
		next := oldest - 1
		if until > 0 && next >= until {
			break
		}
		until = next
	}
	return out, nil
}

func (io *LiveIO) Publish(ctx context.Context, relayURL string, events []gonostr.Event) (int, int, string, error) {
	if len(events) == 0 {
		return 0, 0, "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, io.TotalTimeout)
	defer cancel()
	relay, err := gonostr.RelayConnect(cctx, relayURL, gonostr.RelayOptions{})
	if err != nil {
		return 0, 0, "", fmt.Errorf("%w: %v", ErrRelayUnreachable, err)
	}
	defer relay.Close()

	published, failed := 0, 0
	reasons := map[string]int{} // distinct relay rejection reason → count
	for _, ev := range events {
		pctx, pcancel := context.WithTimeout(cctx, io.PageTimeout)
		perr := relay.Publish(pctx, ev)
		pcancel()
		if perr != nil {
			if strings.Contains(strings.ToLower(perr.Error()), "auth-required") {
				// Whole relay needs auth — abort the batch with a classified error.
				return published, failed, "", fmt.Errorf("%w: %v", ErrRelayAuth, perr)
			}
			failed++
			reasons[cleanReason(perr.Error())]++
			continue
		}
		published++
	}
	return published, failed, summarizeReasons(reasons), nil
}

// cleanReason normalizes a relay's rejection message: strips the library's
// "msg: " prefix and caps the length so one verbose relay can't blow up the UI.
func cleanReason(msg string) string {
	msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg), "msg:"))
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "rejected"
	}
	if len(msg) > 140 {
		msg = msg[:140] + "…"
	}
	return msg
}

// summarizeReasons renders the distinct rejection reasons with counts, busiest
// first, capped to 3 (the rest collapse to "…").
func summarizeReasons(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}
	type rc struct {
		reason string
		n      int
	}
	arr := make([]rc, 0, len(reasons))
	for r, n := range reasons {
		arr = append(arr, rc{r, n})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].n != arr[j].n {
			return arr[i].n > arr[j].n
		}
		return arr[i].reason < arr[j].reason
	})
	parts := make([]string, 0, len(arr))
	for i, x := range arr {
		if i >= 3 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", x.reason, x.n))
	}
	return strings.Join(parts, "; ")
}
