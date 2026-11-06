// Package accounts is the account registry: every account the server knows,
// its status, and the live object that publishes for it. The engine, the
// API and the background services find accounts here by id.
package accounts

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// Store is what the registry reads and writes in the database.
type Store interface {
	AllAccounts(ctx context.Context) ([]store.Account, error)
	SetAccountIdentity(ctx context.Context, id, handle, remoteID string) error
}

// StateStore keeps state an account saves (a refreshed token) and hands it
// back when the account opens.
type StateStore interface {
	// Restore returns the secrets and config the account opens with: those
	// given (from the environment) with its saved state applied.
	Restore(ctx context.Context, row store.Account, secrets map[string]string, config map[string]any) (map[string]string, map[string]any, error)
	// Save merges an update into the account's saved state.
	Save(ctx context.Context, row store.Account, u destination.StateUpdate) error
}

// Alerter sends an operator alert (notify.Webhook).
type Alerter interface {
	Alert(ctx context.Context, summary, body string) error
}

// EnvAccount is an account the environment configures, with its type.
type EnvAccount struct {
	Type string
	destination.EnvAccount
}

// Opener builds an account of a type (DriverOpener in production; tests
// give recording fakes).
type Opener func(ctx context.Context, typ string, req destination.OpenRequest) (destination.Account, error)

// DriverOpener opens an account with its type's registered driver.
func DriverOpener(ctx context.Context, typ string, req destination.OpenRequest) (destination.Account, error) {
	d, ok := destination.Lookup(typ)
	if !ok {
		return nil, destination.Invalidf("no driver for %s", typ)
	}
	return d.Open(ctx, req)
}

// Options are the registry's inputs.
type Options struct {
	Store Store
	// Env are the accounts the environment configures; each matches the
	// stored env row of its type and key.
	Env []EnvAccount
	// State keeps what accounts save (nil: saving is refused).
	State StateStore
	// Open builds accounts (nil: DriverOpener).
	Open    Opener
	Tuning  destination.Tuning
	Alerter Alerter
	Now     func() time.Time // nil: time.Now
	Logger  *slog.Logger     // nil: slog.Default()
}

// Entry is an account as the registry sees it.
type Entry struct {
	Row    store.Account
	Status Status
	// Reason says why the status isn't connected, in the operator's terms.
	Reason string
	// Order is the account's place in the registry order: types in
	// destination.TypesInOrder, then oldest first.
	Order int
	// Health is the last authenticated check (nil before one ran).
	Health    *destination.Health
	CheckedAt time.Time
}

// ChangeKind says what changed.
type ChangeKind string

const (
	// ChangeUpdated: the owner's settings (label, enabled, flags).
	ChangeUpdated ChangeKind = "updated"
	// ChangeReopened: the account was rebuilt (new credentials or
	// connection settings).
	ChangeReopened ChangeKind = "reopened"
	// ChangeStatus: a check changed its status.
	ChangeStatus ChangeKind = "status"
)

// Change is sent to OnChange subscribers.
type Change struct {
	ID    string
	Kind  ChangeKind
	Entry Entry
}

// Registry holds every account by id. Safe for concurrent use.
type Registry struct {
	opt Options
	log *slog.Logger

	mu      sync.RWMutex
	entries map[string]*entry
	order   []string

	reopenMu sync.Mutex // one Reopen at a time

	subMu   sync.Mutex
	subs    map[int]func(Change)
	nextSub int

	bg     context.Context
	stop   context.CancelFunc
	bgWait sync.WaitGroup
}

// entry is the registry's state for one account. Fields are guarded by
// Registry.mu.
type entry struct {
	row   store.Account
	order int
	live  *lease // nil when the account isn't open

	// base is Connected when the account is open, else why it isn't.
	base       Status
	baseReason string
	// refused: the last authenticated check said the network refused the
	// credentials.
	refused         bool
	refusedReason   string
	health          *destination.Health
	checkedAt       time.Time
	nextCheck       time.Time
	lastReport      time.Time
	lastHealthAlert time.Time

	refresh refreshState
	profile profileState
}

func (e *entry) status() (Status, string) {
	switch {
	case e.base != Connected:
		return e.base, e.baseReason
	case !e.row.Enabled:
		return Disabled, "turned off in the app"
	case e.refused:
		return NeedsReconnect, e.refusedReason
	default:
		return Connected, ""
	}
}

func (e *entry) snapshot() Entry {
	st, reason := e.status()
	out := Entry{Row: e.row, Status: st, Reason: reason, Order: e.order, CheckedAt: e.checkedAt}
	if e.health != nil {
		h := *e.health
		out.Health = &h
	}
	return out
}

// lease is one live account object, reference-counted so a replaced
// object is closed only when the last send using it ends.
type lease struct {
	acct    destination.Account
	mu      sync.Mutex
	refs    int
	retired bool
}

func (l *lease) acquire() func() {
	l.mu.Lock()
	l.refs++
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.release) }
}

func (l *lease) release() {
	l.mu.Lock()
	l.refs--
	done := l.retired && l.refs == 0
	l.mu.Unlock()
	if done {
		closeAccount(l.acct)
	}
}

// retire marks the object replaced; it closes now if nothing holds it.
func (l *lease) retire() {
	l.mu.Lock()
	l.retired = true
	done := l.refs == 0
	l.mu.Unlock()
	if done {
		closeAccount(l.acct)
	}
}

func closeAccount(a destination.Account) {
	if c, ok := a.(io.Closer); ok {
		_ = c.Close()
	}
}

// New returns an empty registry; Load fills it.
func New(opt Options) *Registry {
	if opt.Open == nil {
		opt.Open = DriverOpener
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	bg, stop := context.WithCancel(context.Background())
	return &Registry{opt: opt, log: log, entries: map[string]*entry{}, subs: map[int]func(Change){}, bg: bg, stop: stop}
}

func (r *Registry) now() time.Time { return r.opt.Now() }

// Load reads every account from the store and opens those it can. An
// account that can't open gets a status saying why; Load fails only when
// the store can't be read. Opening does no network I/O.
func (r *Registry) Load(ctx context.Context) error {
	rows, err := r.opt.Store.AllAccounts(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	entries := make(map[string]*entry, len(rows))
	for i, row := range rows {
		e := &entry{row: row}
		e.live, e.base, e.baseReason = r.build(ctx, row)
		// The first authenticated check runs soon after start, staggered,
		// then daily: a restart forgets needs_reconnect, the check finds it.
		e.nextCheck = now.Add(time.Minute + time.Duration(i)*10*time.Second)
		entries[row.ID] = e
	}
	r.mu.Lock()
	old := r.entries
	r.entries = entries
	r.reorderLocked()
	r.mu.Unlock()
	for _, e := range old {
		if e.live != nil {
			e.live.retire()
		}
	}
	for _, id := range r.ids() {
		if en, _ := r.Get(id); en.Status != Connected {
			r.log.Warn("account not available", "account", id, "type", en.Row.Type, "status", en.Status, "reason", en.Reason)
		}
	}
	return nil
}

// build opens one account, or says why it can't be opened.
func (r *Registry) build(ctx context.Context, row store.Account) (*lease, Status, string) {
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return nil, NotConfigured, "this version of Publisher has no driver for " + row.Type
	}
	if row.Source != "env" {
		return nil, Locked, "added in the app; this version of Publisher can't open it yet"
	}
	env := r.envFor(row)
	if env == nil {
		return nil, NotConfigured, notConfiguredReason(d)
	}
	secrets, config := env.Secrets, env.Config
	if r.opt.State != nil {
		var err error
		if secrets, config, err = r.opt.State.Restore(ctx, row, secrets, config); err != nil {
			r.log.Error("account state could not be restored", "account", row.ID, "type", row.Type, "err", err)
			return nil, Locked, "its saved state could not be read; it is retried at the next start"
		}
	}
	acct, err := r.opt.Open(ctx, row.Type, destination.OpenRequest{
		AccountID: row.ID,
		Method:    env.Method,
		Config:    config,
		Secrets:   secrets,
		Tuning:    r.opt.Tuning,
		Host:      &host{r: r, id: row.ID, row: row},
	})
	if err != nil {
		return nil, NotConfigured, openProblem(err)
	}
	return &lease{acct: acct}, Connected, ""
}

// envFor is the environment's account for an env row.
func (r *Registry) envFor(row store.Account) *EnvAccount {
	for i := range r.opt.Env {
		if e := &r.opt.Env[i]; e.Type == row.Type && e.Key == row.EnvKey {
			return e
		}
	}
	return nil
}

// reorderLocked sorts the ids into registry order and numbers the
// entries. r.mu must be held for writing.
func (r *Registry) reorderLocked() {
	rank := map[string]int{}
	for i, t := range destination.TypesInOrder() {
		rank[t] = i + 1
	}
	r.order = r.order[:0]
	for id := range r.entries {
		r.order = append(r.order, id)
	}
	slices.SortFunc(r.order, func(a, b string) int {
		ea, eb := r.entries[a].row, r.entries[b].row
		ra, rb := rank[ea.Type], rank[eb.Type]
		if ra == 0 {
			ra = len(rank) + 1 // unknown types last
		}
		if rb == 0 {
			rb = len(rank) + 1
		}
		switch {
		case ra != rb:
			return ra - rb
		case ea.Type != eb.Type:
			return strings.Compare(ea.Type, eb.Type)
		case !ea.CreatedAt.Equal(eb.CreatedAt):
			return ea.CreatedAt.Compare(eb.CreatedAt)
		default:
			return strings.Compare(a, b)
		}
	})
	for i, id := range r.order {
		r.entries[id].order = i
	}
}

func (r *Registry) ids() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.order)
}

// Get returns one account.
func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return Entry{}, false
	}
	return e.snapshot(), true
}

// List returns the owner's accounts in registry order.
func (r *Registry) List(owner string) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Entry
	for _, id := range r.order {
		if e := r.entries[id]; e.row.OwnerID == owner {
			out = append(out, e.snapshot())
		}
	}
	return out
}

// OfType returns the owner's accounts of one type in registry order; the
// first is "the first account of the type".
func (r *Registry) OfType(owner, typ string) []Entry {
	var out []Entry
	for _, e := range r.List(owner) {
		if e.Row.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// Acquire leases a connected account's live object. release must be
// called when the work using it ends (a send, a request); until then a
// Reopen leaves the object open. An account that can't be used gives an
// *Unavailable error.
func (r *Registry) Acquire(id string) (destination.Account, func(), error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return nil, nil, ErrNoAccount
	}
	if st, reason := e.status(); st != Connected {
		return nil, nil, &Unavailable{ID: id, Status: st, Reason: reason}
	}
	return e.live.acct, e.live.acquire(), nil
}

// leaseAny leases the live object whatever the status (checks, identity,
// capabilities); nil when the account isn't open.
func (r *Registry) leaseAny(id string) (*lease, func()) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.live == nil {
		return nil, nil
	}
	return e.live, e.live.acquire()
}

// Caps returns the account's capabilities when it is open, else the
// type's manifest defaults, so recovery decides the same way for an
// account that is locked or not open.
func (r *Registry) Caps(ctx context.Context, id, typ string) destination.Capabilities {
	if l, release := r.leaseAny(id); l != nil {
		defer release()
		return l.acct.Capabilities(ctx)
	}
	if d, ok := destination.Lookup(typ); ok {
		return d.Manifest().Defaults
	}
	return destination.Capabilities{}
}

// Reopen rebuilds an account (its credentials or connection settings
// changed) and swaps the new object in. The old one is closed when its
// last lease is released. A confirmed refusal is forgotten: the new
// credentials get their own check.
func (r *Registry) Reopen(ctx context.Context, id string) error {
	r.reopenMu.Lock()
	defer r.reopenMu.Unlock()
	r.mu.RLock()
	e, ok := r.entries[id]
	var row store.Account
	if ok {
		row = e.row
	}
	r.mu.RUnlock()
	if !ok {
		return ErrNoAccount
	}
	live, base, reason := r.build(ctx, row)
	r.mu.Lock()
	old := e.live
	e.live, e.base, e.baseReason = live, base, reason
	e.refused, e.refusedReason = false, ""
	e.refresh = refreshState{}
	e.nextCheck = r.now().Add(time.Minute)
	snap := e.snapshot()
	r.mu.Unlock()
	if old != nil {
		old.retire()
	}
	r.emit(Change{ID: id, Kind: ChangeReopened, Entry: snap})
	return nil
}

// Update takes the owner's settings (label, enabled, flags) from a row the
// store just saved. The live object is kept, so its caches survive. The
// network's identity (handle, remote id) is the registry's own and is
// kept, and a row older than the one held is ignored: two changes saved
// one after the other may reach here in the other order.
func (r *Registry) Update(id string, row store.Account) error {
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return ErrNoAccount
	}
	if row.UpdatedAt.Before(e.row.UpdatedAt) {
		r.mu.Unlock()
		return nil
	}
	e.row.Label, e.row.Enabled, e.row.DefaultSelected, e.row.PublicFeed = row.Label, row.Enabled, row.DefaultSelected, row.PublicFeed
	e.row.UpdatedAt = row.UpdatedAt
	snap := e.snapshot()
	r.mu.Unlock()
	r.emit(Change{ID: id, Kind: ChangeUpdated, Entry: snap})
	return nil
}

// OnChange calls fn after every change, outside the registry's locks.
func (r *Registry) OnChange(fn func(Change)) (unsubscribe func()) {
	r.subMu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = fn
	r.subMu.Unlock()
	return func() {
		r.subMu.Lock()
		delete(r.subs, id)
		r.subMu.Unlock()
	}
}

func (r *Registry) emit(c Change) {
	r.subMu.Lock()
	fns := make([]func(Change), 0, len(r.subs))
	for _, fn := range r.subs {
		fns = append(fns, fn)
	}
	r.subMu.Unlock()
	for _, fn := range fns {
		fn(c)
	}
}

// background runs fn on a goroutine Close waits for.
func (r *Registry) background(fn func(ctx context.Context)) {
	r.bgWait.Add(1)
	go func() {
		defer r.bgWait.Done()
		fn(r.bg)
	}()
}

// Close stops background work and closes every account once its leases
// are released.
func (r *Registry) Close() {
	r.stop()
	r.bgWait.Wait()
	r.mu.Lock()
	entries := r.entries
	r.entries = map[string]*entry{}
	r.order = nil
	r.mu.Unlock()
	for _, e := range entries {
		if e.live != nil {
			e.live.retire()
		}
	}
}
