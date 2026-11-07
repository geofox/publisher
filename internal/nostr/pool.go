package nostr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	gonostr "fiatjaf.com/nostr"
)

// errPoolClosed is the error after Close.
var errPoolClosed = errors.New("the account's relay connections are closed")

// relayPool keeps one connection per relay for an account, each dialed
// with the account's HTTP client, so the host's network policy applies to
// every relay the account publishes to or reads from. (gonostr.Pool can't
// take a client: it always connects with the default one.) It implements
// publishPool.
type relayPool struct {
	ctx    context.Context // the connections' lifetime; Close cancels it
	cancel context.CancelCauseFunc
	client *http.Client

	mu      sync.Mutex
	relays  map[string]*gonostr.Relay
	dialing map[string]*sync.Mutex // one connect at a time per relay
}

func newRelayPool(client *http.Client) *relayPool {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &relayPool{ctx: ctx, cancel: cancel, client: client,
		relays: map[string]*gonostr.Relay{}, dialing: map[string]*sync.Mutex{}}
}

// relay returns a connection to url, connecting (or reconnecting) it with
// the pool's client. ctx bounds the connect (7 s when it has no deadline);
// the connection lives until the pool closes or the relay drops it.
func (p *relayPool) relay(ctx context.Context, url string) (*gonostr.Relay, error) {
	nm := gonostr.NormalizeURL(url)
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	lock := p.dialing[nm]
	if lock == nil {
		lock = &sync.Mutex{}
		p.dialing[nm] = lock
	}
	p.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	r := p.relays[nm]
	p.mu.Unlock()
	if r != nil && r.IsConnected() {
		return r, nil
	}
	r = gonostr.NewRelay(p.ctx, nm, gonostr.RelayOptions{})
	if err := r.ConnectWithClient(ctx, p.client); err != nil {
		_ = r.Close()
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil { // closed while connecting
		_ = r.Close()
		return nil, errPoolClosed
	}
	p.relays[nm] = r
	return r, nil
}

// PublishMany sends evt to each relay (once per normalized URL) and
// reports each result on the channel, which is closed when all are in.
func (p *relayPool) PublishMany(ctx context.Context, urls []string, evt gonostr.Event) chan gonostr.PublishResult {
	ch := make(chan gonostr.PublishResult, len(urls))
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, url := range urls {
		nm := gonostr.NormalizeURL(url)
		if seen[nm] {
			continue
		}
		seen[nm] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.relay(ctx, url)
			if err != nil {
				ch <- gonostr.PublishResult{Error: fmt.Errorf("failed to connect: %w", err), RelayURL: url}
				return
			}
			ch <- gonostr.PublishResult{Error: r.Publish(ctx, evt), RelayURL: url, Relay: r}
		}()
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch
}

// QuerySingle asks every relay for filter and returns the first event any
// of them sends, or nil when all have finished (stored events sent, or
// the subscription closed) without one. The other queries are cancelled.
func (p *relayPool) QuerySingle(ctx context.Context, urls []string, filter gonostr.Filter, opts gonostr.SubscriptionOptions) *gonostr.RelayEvent {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan gonostr.RelayEvent, 1)
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, url := range urls {
		nm := gonostr.NormalizeURL(url)
		if seen[nm] {
			continue
		}
		seen[nm] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.relay(ctx, url)
			if err != nil {
				return
			}
			sub, err := r.Subscribe(ctx, filter, opts)
			if err != nil {
				return
			}
			defer sub.Unsub()
			select {
			case ev, ok := <-sub.Events:
				if ok {
					select {
					case found <- gonostr.RelayEvent{Event: ev, Relay: r}:
					default: // another relay answered first
					}
				}
			case <-sub.EndOfStoredEvents:
			case <-sub.ClosedReason:
			case <-ctx.Done():
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case ev := <-found:
		return &ev
	case <-done:
		select { // an event sent just before the last query ended
		case ev := <-found:
			return &ev
		default:
			return nil
		}
	case <-ctx.Done():
		return nil
	}
}

// Close closes every connection; the pool can't be used after it.
func (p *relayPool) Close(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel(errors.New(reason))
}
