// Package accounts is the account registry: every account the server knows,
// its status, and the live object that publishes for it. The engine, the
// API and the background services find accounts here by id.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// Store is what the registry reads and writes in the database.
type Store interface {
	AllAccounts(ctx context.Context) ([]store.Account, error)
	SetAccountIdentity(ctx context.Context, id, handle, remoteID string) error
}

// StateStore keeps state an account from the environment saves (a
// refreshed token) and hands it back when the account opens.
type StateStore interface {
	// Restore returns the secrets and config the account opens with: those
	// given (from the environment) with its saved state applied.
	Restore(ctx context.Context, row store.Account, secrets map[string]string, config map[string]any) (map[string]string, map[string]any, error)
	// Save merges an update into the account's saved state. audience is
	// where the account's credentials are sent (destination.AudienceOf).
	Save(ctx context.Context, row store.Account, audience string, u destination.StateUpdate) error
}

// SecretStore keeps the sealed credentials of accounts (store.Store).
type SecretStore interface {
	Secrets(ctx context.Context, owner, scope, ref string) ([]store.SealedSecret, error)
	SaveAccountState(ctx context.Context, owner, id string, patch map[string]any, put []store.SealedSecret, del []string) error
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
	// Guard is where accounts added in the app may connect
	// (PRIVATE_DESTINATIONS and the server's own addresses).
	Guard netguard.Policy
	// Secrets holds the sealed credentials of accounts added in the app
	// (nil: they stay locked).
	Secrets SecretStore
	// Keys opens and seals credentials; KeyState is what PrepareKeys
	// found (the zero value: no key).
	Keys     secrets.Keyring
	KeyState KeyState
	// Writes stores the accounts added, changed, adopted and deleted in
	// the app (nil: they can't be).
	Writes AccountWriter
	// Flows keeps the connect flows and the OAuth clients (nil: accounts
	// can't be connected by signing in at their network).
	Flows FlowStore
	// Instance answers a type's InstanceConfig values and secrets (a
	// Threads app); nil: none.
	Instance func(typ string) (map[string]any, map[string]string)
	// Seed gives an account just added in the app its starting state
	// beside its row (a Nostr account's relay-sync list); nil: none.
	Seed func(ctx context.Context, row store.Account) error
	// Getenv reads the operator's defaults for a new account (nil:
	// os.Getenv).
	Getenv func(string) string
	Now    func() time.Time // nil: time.Now
	Logger *slog.Logger     // nil: slog.Default()
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
	// ChangeAdded: an account was added in the app.
	ChangeAdded ChangeKind = "added"
	// ChangeRemoved: an account was deleted; Entry is its last state.
	ChangeRemoved ChangeKind = "removed"
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
	closed  bool

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
	lastGrantAlert  time.Time

	refresh refreshState
	profile profileState
	// duplicateOf is the account this one is the same network account as
	// (warned about once).
	duplicateOf string
}

func (e *entry) status() (Status, string) {
	switch {
	case e.base != Connected:
		return e.base, e.baseReason
	case !e.row.Enabled:
		return Disabled, "you turned it off; turn it on again to use it"
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
	return nil // the caller logs each account's status (cmd/publisher logStart)
}

// build opens one account, or says why it can't be opened.
func (r *Registry) build(ctx context.Context, row store.Account) (*lease, Status, string) {
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return nil, NotConfigured, "this version of Publisher has no driver for " + row.Type
	}
	switch row.Source {
	case store.SourceEnv:
	case store.SourceDB:
		return r.buildDB(ctx, d, row)
	default:
		return nil, Locked, "this version of Publisher can't open an account from " + row.Source
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
			if errors.Is(err, errStateLocked) {
				// Its renewed token is the one that works now: not the
				// environment's older one.
				return nil, Locked, "its saved state (a renewed token) was saved with another master key: set that key in PUBLISHER_MASTER_KEY (or rotate to a new one); its posts wait until then"
			}
			return nil, Locked, "its saved state could not be read; it is retried at the next start"
		}
	}
	acct, err := r.opt.Open(ctx, row.Type, destination.OpenRequest{
		AccountID: row.ID,
		Method:    env.Method,
		Config:    config,
		Secrets:   secrets,
		Tuning:    r.opt.Tuning,
		Host:      &host{r: r, id: row.ID, row: row, audience: destination.AudienceOf(d.Manifest(), config)},
	})
	if err != nil {
		return nil, NotConfigured, openProblem(err)
	}
	return &lease{acct: acct}, Connected, ""
}

// buildDB opens an account added in the app: its configuration from its
// row, its credentials opened with the master key for the server its
// configuration names (so a credential copied to another account, or an
// address edited in the database, doesn't open).
func (r *Registry) buildDB(ctx context.Context, d destination.Driver, row store.Account) (*lease, Status, string) {
	config := map[string]any{}
	if row.Config != "" {
		if err := json.Unmarshal([]byte(row.Config), &config); err != nil {
			r.log.Error("account configuration could not be read", "account", row.ID, "type", row.Type, "err", err)
			return nil, Locked, "its stored configuration can't be read"
		}
	}
	if r.opt.Secrets == nil {
		return nil, Locked, "this server can't open accounts added in the app"
	}
	sealed, err := r.opt.Secrets.Secrets(ctx, row.OwnerID, store.SecretScopeAccount, row.ID)
	if err != nil {
		r.log.Error("account credentials could not be read", "account", row.ID, "type", row.Type, "err", err)
		return nil, Locked, "its credentials can't be read now; it is tried again at the next start"
	}
	audience := destination.AudienceOf(d.Manifest(), config)
	plain := make(map[string]string, len(sealed))
	for _, s := range sealed {
		b := Binding(s)
		b.Audience = audience
		v, err := r.opt.Keys.Open(b, s.Sealed)
		if err != nil {
			r.log.Error("account credential could not be opened", "account", row.ID, "type", row.Type, "secret", s.Name, "err", err)
			return nil, Locked, lockedReason(err)
		}
		plain[s.Name] = v
	}
	acct, err := r.opt.Open(ctx, row.Type, destination.OpenRequest{
		AccountID: row.ID,
		Method:    row.AuthMethod,
		Config:    config,
		Secrets:   plain,
		Tuning:    r.opt.Tuning,
		Host:      &host{r: r, id: row.ID, row: row, audience: audience},
	})
	if err != nil {
		return nil, NotConfigured, openProblem(err)
	}
	return &lease{acct: acct}, Connected, ""
}

// lockedReason says why an account's credentials didn't open, and what to
// do; its posts wait meanwhile.
func lockedReason(err error) string {
	switch {
	case errors.Is(err, secrets.ErrNoKey):
		return "its credentials need the master key: set PUBLISHER_MASTER_KEY; its posts wait until then"
	case errors.Is(err, secrets.ErrUnknownKey):
		return "its credentials were saved with another master key: set that key (or rotate to a new one); its posts wait until then"
	default:
		return "its credentials can't be opened (changed or moved in the database, or its server address was edited there): connect it again"
	}
}

// KeyState is the master key's status.
func (r *Registry) KeyState() KeyState { return r.opt.KeyState }

// Keys is the master key ring, for the account API's writes.
func (r *Registry) Keys() secrets.Keyring { return r.opt.Keys }

// Add opens an account just added in the app and lists it.
func (r *Registry) Add(ctx context.Context, row store.Account) (Entry, error) {
	r.reopenMu.Lock()
	defer r.reopenMu.Unlock()
	r.mu.RLock()
	_, exists := r.entries[row.ID]
	closed := r.closed
	r.mu.RUnlock()
	switch {
	case closed:
		return Entry{}, ErrClosed
	case exists:
		return Entry{}, fmt.Errorf("account %s is already listed", row.ID)
	}
	e := &entry{row: row, nextCheck: r.now().Add(time.Minute)}
	e.live, e.base, e.baseReason = r.build(ctx, row)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		if e.live != nil {
			e.live.retire()
		}
		return Entry{}, ErrClosed
	}
	r.entries[row.ID] = e
	r.reorderLocked()
	snap := e.snapshot()
	r.mu.Unlock()
	r.emit(Change{ID: row.ID, Kind: ChangeAdded, Entry: snap})
	return snap, nil
}

// Remove drops a deleted account. Its live object is closed when its last
// lease is released; new leases are refused at once.
func (r *Registry) Remove(id string) error {
	r.reopenMu.Lock()
	defer r.reopenMu.Unlock()
	r.mu.Lock()
	e, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return ErrNoAccount
	}
	delete(r.entries, id)
	r.reorderLocked()
	e.base, e.baseReason = Deleted, "you deleted it"
	snap := e.snapshot()
	r.mu.Unlock()
	if e.live != nil {
		e.live.retire()
	}
	r.emit(Change{ID: id, Kind: ChangeRemoved, Entry: snap})
	return nil
}

// EnvConfigured reports whether the environment configures an env row's
// account now (its variables are set).
func (r *Registry) EnvConfigured(row store.Account) bool {
	return row.Source == store.SourceEnv && r.envFor(row) != nil
}

// envFor is the environment's account for an env row. The environment's
// accounts are the operator's: another owner's row never opens with them.
func (r *Registry) envFor(row store.Account) *EnvAccount {
	if row.OwnerID != store.DefaultOwner || row.Source != store.SourceEnv || !row.DeletedAt.IsZero() {
		return nil
	}
	key := row.EnvKey
	if key == "" {
		key = destination.EnvKeyDefault
	}
	for i := range r.opt.Env {
		e := &r.opt.Env[i]
		ek := e.Key
		if ek == "" {
			ek = destination.EnvKeyDefault
		}
		if e.Type == row.Type && ek == key {
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
	if r.closed {
		return nil, nil, ErrClosed
	}
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
	_, err := r.reopen(ctx, id, nil)
	return err
}

// ReopenWith reopens an account from a row the store just saved with new
// configuration or credentials (or adopted into the app): the row's
// source, method, configuration and identity replace the held ones. The
// owner's settings (label, flags) of a newer row already held are kept,
// as Update would.
func (r *Registry) ReopenWith(ctx context.Context, row store.Account) (Entry, error) {
	return r.reopen(ctx, row.ID, &row)
}

func (r *Registry) reopen(ctx context.Context, id string, next *store.Account) (Entry, error) {
	r.reopenMu.Lock()
	defer r.reopenMu.Unlock()
	r.mu.Lock()
	e, ok := r.entries[id]
	var row store.Account
	if ok {
		if next != nil {
			n := *next
			if n.UpdatedAt.Before(e.row.UpdatedAt) {
				n.Label, n.Enabled, n.DefaultSelected, n.PublicFeed = e.row.Label, e.row.Enabled, e.row.DefaultSelected, e.row.PublicFeed
				n.UpdatedAt = e.row.UpdatedAt
			}
			e.row = n
			e.duplicateOf = ""
		}
		row = e.row
	}
	r.mu.Unlock()
	if !ok {
		return Entry{}, ErrNoAccount
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
	return snap, nil
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
	r.closed = true
	r.mu.Unlock()
	for _, e := range entries {
		if e.live != nil {
			e.live.retire()
		}
	}
}
