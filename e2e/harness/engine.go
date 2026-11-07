package main

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// engine holds the running account registry and dispatcher and can
// replace them, as a process restart would: the old dispatcher is
// abandoned mid-send (its held calls never return, like a killed
// process's), and a new registry and dispatcher on the same database
// recover what was left sending and carry on. The API talks to whichever
// is current through engine, which implements the interfaces the API looks
// for.
type engine struct {
	ctx  context.Context // the harness's lifetime
	make func(ctx context.Context) (*accounts.Registry, *dispatch.Dispatcher, error)

	restarting sync.Mutex // one restart at a time

	mu     sync.RWMutex
	reg    *accounts.Registry
	d      *dispatch.Dispatcher
	cancel context.CancelFunc
}

func newEngine(ctx context.Context, mk func(ctx context.Context) (*accounts.Registry, *dispatch.Dispatcher, error)) (*engine, error) {
	e := &engine{ctx: ctx, make: mk}
	return e, e.start()
}

func (e *engine) cur() *dispatch.Dispatcher {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.d
}

func (e *engine) registry() *accounts.Registry {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.reg
}

// start loads a new registry and runs a new dispatcher: recovery first, as
// cmd/publisher does, so a restart answers once what was left sending is
// settled.
func (e *engine) start() error {
	ctx, cancel := context.WithCancel(e.ctx)
	reg, d, err := e.make(ctx)
	if err != nil {
		cancel()
		return err
	}
	d.Recover(ctx)
	e.mu.Lock()
	e.reg, e.d, e.cancel = reg, d, cancel
	e.mu.Unlock()
	go d.Run(ctx)
	return nil
}

// restart stops the current dispatcher without waiting for its sends (a
// held one never finishes) and starts a new registry and dispatcher.
func (e *engine) restart() {
	e.restarting.Lock()
	defer e.restarting.Unlock()
	e.mu.RLock()
	old, cancel := e.d, e.cancel
	e.mu.RUnlock()
	cancel()
	sctx, done := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_ = old.Shutdown(sctx) // times out while a send is held: the "killed" part
	done()
	if err := e.start(); err != nil {
		slog.Error("e2e harness: restart failed", "err", err)
	}
}

func (e *engine) shutdown(ctx context.Context) error {
	e.mu.RLock()
	d, cancel := e.d, e.cancel
	e.mu.RUnlock()
	cancel()
	return d.Shutdown(ctx)
}

func (e *engine) Enqueue(ctx context.Context, spec dispatch.PostSpec, at *time.Time) (*store.Post, error) {
	return e.cur().Enqueue(ctx, spec, at)
}

func (e *engine) EnqueueInteraction(ctx context.Context, spec dispatch.InteractSpec) (*store.Post, error) {
	return e.cur().EnqueueInteraction(ctx, spec)
}

func (e *engine) RetryTargetRelay(ctx context.Context, postID string, targetID int64, relay string) (*store.Post, error) {
	return e.cur().RetryTargetRelay(ctx, postID, targetID, relay)
}

func (e *engine) Caps(ctx context.Context, id, typ string) destination.Capabilities {
	return e.cur().Caps(ctx, id, typ)
}

// The api.AccountRegistry methods reach the current registry (a restart
// builds a new one).

func (e *engine) Get(id string) (accounts.Entry, bool)       { return e.registry().Get(id) }
func (e *engine) List(owner string) []accounts.Entry         { return e.registry().List(owner) }
func (e *engine) Profile(id string) (accounts.Profile, bool) { return e.registry().Profile(id) }
func (e *engine) Acquire(id string) (destination.Account, func(), error) {
	return e.registry().Acquire(id)
}
func (e *engine) Update(id string, row store.Account) error { return e.registry().Update(id, row) }

// The api.AccountWriter methods (adding, changing and deleting accounts
// in the app) reach the current registry too.

func (e *engine) Create(ctx context.Context, n accounts.NewAccount) (accounts.Entry, error) {
	return e.registry().Create(ctx, n)
}
func (e *engine) ChangeCredentials(ctx context.Context, u accounts.CredentialUpdate) (accounts.Entry, error) {
	return e.registry().ChangeCredentials(ctx, u)
}
func (e *engine) Adopt(ctx context.Context, owner, id, actor, tokenID string) (accounts.Entry, error) {
	return e.registry().Adopt(ctx, owner, id, actor, tokenID)
}
func (e *engine) Delete(ctx context.Context, owner, id, actor, tokenID string) error {
	return e.registry().Delete(ctx, owner, id, actor, tokenID)
}
func (e *engine) Reveal(ctx context.Context, owner, id string) (map[string]string, error) {
	return e.registry().Reveal(ctx, owner, id)
}
func (e *engine) Details(ctx context.Context, row store.Account) (accounts.Details, error) {
	return e.registry().Details(ctx, row)
}
func (e *engine) CheckNow(ctx context.Context, id string) (destination.Health, bool) {
	return e.registry().CheckNow(ctx, id)
}

// The api.AccountConnector methods (connecting by signing in at the
// network) too.

func (e *engine) BeginConnect(ctx context.Context, q accounts.ConnectStart) (accounts.StartedFlow, error) {
	return e.registry().BeginConnect(ctx, q)
}
func (e *engine) CompleteConnect(ctx context.Context, typ string, callback url.Values, sessionUser string) (accounts.FlowView, error) {
	return e.registry().CompleteConnect(ctx, typ, callback, sessionUser)
}
func (e *engine) Flow(ctx context.Context, owner, user, id string) (accounts.FlowView, error) {
	return e.registry().Flow(ctx, owner, user, id)
}
func (e *engine) Choose(ctx context.Context, owner, user, id string, n int) (accounts.FlowView, error) {
	return e.registry().Choose(ctx, owner, user, id, n)
}
func (e *engine) ConnectUnavailable(typ string, a destination.AuthMethod) string {
	return e.registry().ConnectUnavailable(typ, a)
}
