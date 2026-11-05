package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the store's driver, for seeding a row it can't write yet

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// engine holds the running dispatcher and can replace it, as a process
// restart would: the old one is abandoned mid-send (its held calls never
// return, like a killed process's), and a new one on the same database
// recovers what was left sending and carries on. The API talks to whichever
// is current through engine, which implements the dispatcher interfaces the
// API looks for.
type engine struct {
	ctx  context.Context // the harness's lifetime
	make func() *dispatch.Dispatcher

	restarting sync.Mutex // one restart at a time

	mu     sync.RWMutex
	d      *dispatch.Dispatcher
	cancel context.CancelFunc
}

func newEngine(ctx context.Context, mk func() *dispatch.Dispatcher) *engine {
	e := &engine{ctx: ctx, make: mk}
	e.start()
	return e
}

func (e *engine) cur() *dispatch.Dispatcher {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.d
}

// start runs a new dispatcher: recovery first, as cmd/publisher does, so a
// restart answers once what was left sending is settled.
func (e *engine) start() {
	d := e.make()
	ctx, cancel := context.WithCancel(e.ctx)
	d.Recover(ctx)
	e.mu.Lock()
	e.d, e.cancel = d, cancel
	e.mu.Unlock()
	go d.Run(ctx)
}

// restart stops the current dispatcher without waiting for its sends (a
// held one never finishes) and starts a new one.
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
	e.start()
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

func (e *engine) SendsAs(typ, id string) bool { return e.cur().SendsAs(typ, id) }

func (e *engine) Caps(ctx context.Context, name string) (destination.Capabilities, bool) {
	return e.cur().Caps(ctx, name)
}

// seedSecondAccount adds a second, enabled account of typ that no
// credentials back, as step 4's account settings will make possible. The
// store can only seed the env account of a type, so the row is written
// directly. It is created after the env accounts, so it lists after them.
func seedSecondAccount(dbPath, typ, label string) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now().UTC().Add(time.Second)
	_, err = db.Exec(`INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'env', 'second', 1, ?, ?)`,
		fmt.Sprintf("acc_e2esecond%s", typ), store.DefaultOwner, typ, label, now, now)
	return err
}
