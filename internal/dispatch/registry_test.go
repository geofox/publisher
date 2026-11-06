package dispatch

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/logging"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/thread"
)

// Test accounts and specs. Most tests send one account per type: the
// account of type t has the id acct(t), and typeSpec/typeInteract name
// targets by type the way specs did before accounts. Tests about several
// accounts of a type build PostSpec and InteractSpec themselves.

// acct is the id of a type's test account.
func acct(typ string) string { return "acc_" + typ }

// testRegistry is an AccountSource over fixed accounts, each connected
// unless set otherwise. It counts leases, so a test can see none leak.
type testRegistry struct {
	mu       sync.Mutex
	accts    map[string]destination.Account
	rows     map[string]store.Account
	status   map[string]accounts.Status
	reasons  map[string]string
	leases   int
	reported []string
}

// registryOf makes a registry with one account per type, id acct(type).
func registryOf(byType map[string]destination.Account) *testRegistry {
	r := &testRegistry{accts: map[string]destination.Account{}, rows: map[string]store.Account{},
		status: map[string]accounts.Status{}, reasons: map[string]string{}}
	for typ, a := range byType {
		r.add(acct(typ), typ, a)
	}
	return r
}

// add adds an account (nil: one that isn't open).
func (r *testRegistry) add(id, typ string, a destination.Account) *testRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accts[id] = a
	label := typ
	if a != nil {
		label = a.Manifest().Name
	}
	r.rows[id] = store.Account{ID: id, OwnerID: store.DefaultOwner, Type: typ, Label: label, Source: "env", Enabled: true}
	return r
}

// set gives an account a status other than connected.
func (r *testRegistry) set(id string, st accounts.Status, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[id], r.reasons[id] = st, reason
}

func (r *testRegistry) statusOf(id string) accounts.Status {
	if st, ok := r.status[id]; ok {
		return st
	}
	return accounts.Connected
}

func (r *testRegistry) Acquire(id string) (destination.Account, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accts[id]
	if !ok {
		return nil, nil, accounts.ErrNoAccount
	}
	if st := r.statusOf(id); st != accounts.Connected || a == nil {
		if a == nil && st == accounts.Connected {
			st = accounts.NotConfigured
		}
		return nil, nil, &accounts.Unavailable{ID: id, Status: st, Reason: r.reasons[id]}
	}
	r.leases++
	var once sync.Once
	return a, func() {
		once.Do(func() {
			r.mu.Lock()
			r.leases--
			r.mu.Unlock()
		})
	}, nil
}

func (r *testRegistry) Caps(ctx context.Context, id, typ string) destination.Capabilities {
	r.mu.Lock()
	a := r.accts[id]
	r.mu.Unlock()
	if a != nil {
		return a.Capabilities(ctx)
	}
	if d, ok := destination.Lookup(typ); ok {
		return d.Manifest().Defaults
	}
	return destination.Capabilities{}
}

func (r *testRegistry) Get(id string) (accounts.Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return accounts.Entry{}, false
	}
	return accounts.Entry{Row: row, Status: r.statusOf(id), Reason: r.reasons[id]}, true
}

func (r *testRegistry) List(owner string) []accounts.Entry {
	r.mu.Lock()
	ids := make([]string, 0, len(r.rows))
	for id, row := range r.rows {
		if row.OwnerID == owner {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	slices.Sort(ids)
	out := make([]accounts.Entry, 0, len(ids))
	for _, id := range ids {
		e, _ := r.Get(id)
		out = append(out, e)
	}
	return out
}

func (r *testRegistry) ReportAuthFailure(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reported = append(r.reported, id)
}

func (r *testRegistry) held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases
}

// ensureAccounts gives every target's account a stored row, so a post
// naming it can be saved.
func ensureAccounts(st *store.Store, targets []TargetSpec) error {
	for _, t := range targets {
		if err := ensureAccount(st, t.AccountID, t.Type); err != nil {
			return err
		}
	}
	return nil
}

func ensureAccount(st *store.Store, id, typ string) error {
	_, err := st.Account(context.Background(), store.DefaultOwner, id)
	if !errors.Is(err, store.ErrAccountNotFound) {
		return err
	}
	return st.InsertAccount(context.Background(), store.Account{ID: id, Type: typ, Label: typ, EnvKey: "test-" + id,
		Enabled: true, DefaultSelected: true, PublicFeed: true})
}

// saveWithAccounts saves p after giving each target without an account
// its type's test account.
func saveWithAccounts(st *store.Store, p *store.Post) error {
	for i := range p.Targets {
		tg := &p.Targets[i]
		if tg.AccountID == "" {
			tg.AccountID = acct(tg.Platform)
		}
		if err := ensureAccount(st, tg.AccountID, tg.Platform); err != nil {
			return err
		}
	}
	return st.SavePost(p)
}

// typeSpec is a post naming destination types, each sent as its type's
// test account (or Accounts[type]).
type typeSpec struct {
	MasterText   string
	Platforms    []string
	DelaySeconds int
	Source       string
	Overrides    map[string]Overrides
	Texts        map[string]string // a type's own text
	Images       []Img
	Layouts      map[string]thread.Layout
	MediaRecords []store.Media
	Number       bool
	Accounts     map[string]string
}

func (s typeSpec) id(typ string) string {
	if id := s.Accounts[typ]; id != "" {
		return id
	}
	return acct(typ)
}

func (s typeSpec) spec() PostSpec {
	out := PostSpec{MasterText: s.MasterText, DelaySeconds: s.DelaySeconds, Source: s.Source, Images: s.Images,
		MediaRecords: s.MediaRecords, Number: s.Number}
	for _, typ := range s.Platforms {
		out.Targets = append(out.Targets, TargetSpec{AccountID: s.id(typ), Type: typ, Text: s.Texts[typ],
			Overrides: s.Overrides[typ], Layout: s.Layouts[typ]})
	}
	return out
}

// typeInteract is an interaction naming types: the source's type's test
// account acts, and each Fanout type's posts a link-quote copy.
type typeInteract struct {
	Action             string
	SourcePlatform     string
	Ref                InteractRef
	SourceURL          string
	SourceAuthor       string
	Text               string
	Overrides          map[string]Overrides
	Texts              map[string]string
	Fanout             []string
	Force              bool
	Images             []Img
	MediaRecords       []store.Media
	Number             bool
	Layout             thread.Layout
	SourcePreview      SourcePreview
	SourceImages       []Img
	SourceMediaRecords []store.Media
	Source             string
	Accounts           map[string]string
}

func (s typeInteract) id(typ string) string {
	if id := s.Accounts[typ]; id != "" {
		return id
	}
	return acct(typ)
}

func (s typeInteract) spec() InteractSpec {
	out := InteractSpec{Action: s.Action, SourcePlatform: s.SourcePlatform, SourceAccount: s.id(s.SourcePlatform),
		Ref: s.Ref, SourceURL: s.SourceURL, SourceAuthor: s.SourceAuthor, Text: s.Text, Force: s.Force,
		Images: s.Images, MediaRecords: s.MediaRecords, Number: s.Number, SourcePreview: s.SourcePreview,
		SourceImages: s.SourceImages, SourceMediaRecords: s.SourceMediaRecords, Source: s.Source}
	out.Targets = append(out.Targets, TargetSpec{AccountID: out.SourceAccount, Type: s.SourcePlatform,
		Text: s.Texts[s.SourcePlatform], Overrides: s.Overrides[s.SourcePlatform], Layout: s.Layout})
	for _, typ := range s.Fanout {
		out.Targets = append(out.Targets, TargetSpec{AccountID: s.id(typ), Type: typ, Text: s.Texts[typ],
			Overrides: s.Overrides[typ]})
	}
	return out
}

// The engine's inline entry points, for tests: production saves posts for
// the background workers (Enqueue, EnqueueInteraction) and makes retries
// due (store.MarkDue); these send inline and return the post as recorded.

func (d *Dispatcher) Post(ctx context.Context, s typeSpec) *store.Post {
	return d.PostSpec(ctx, newID(), s.spec())
}

// PostSpec plans, saves and sends spec inline.
func (d *Dispatcher) PostSpec(ctx context.Context, id string, spec PostSpec) *store.Post {
	ctx = logging.With(ctx, "post_id", id)
	if d.Store != nil {
		if err := ensureAccounts(d.Store, spec.Targets); err != nil {
			panic(err)
		}
	}
	now := d.now()
	rec, thumbs := d.planPostCards(ctx, id, spec, &now, "sending")
	if err := d.drive(ctx, id, &memMedia{post: spec.Images, thumbs: thumbs}, func() error { return d.create(ctx, rec) }); err != nil {
		return notSaved(rec, err)
	}
	return d.recorded(ctx, rec)
}

// Schedule saves a post for a future time without sending it.
func (d *Dispatcher) Schedule(ctx context.Context, s typeSpec, at time.Time) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("schedule requires a store")
	}
	spec := s.spec()
	if err := ensureAccounts(d.Store, spec.Targets); err != nil {
		return nil, err
	}
	atUTC := at.UTC()
	rec := d.planPost(ctx, newID(), spec, &atUTC, "scheduled")
	rec.ScheduledAt = &atUTC
	if err := d.Store.SavePost(rec); err != nil {
		return nil, err
	}
	d.kick()
	return rec, nil
}

// Fire sends a scheduled post now, inline.
func (d *Dispatcher) Fire(ctx context.Context, postID string) (*store.Post, error) {
	return d.retryWhere(ctx, postID, func(t store.Target) bool { return t.Status == "scheduled" })
}

// Retry re-sends the unfinished targets of a post inline: failed, missed,
// and threads stopped part-way. With types, only those types' targets.
func (d *Dispatcher) Retry(ctx context.Context, postID string, types []string) (*store.Post, error) {
	return d.retryWhere(ctx, postID, func(t store.Target) bool {
		retryable := t.Status == "failed" || t.Status == "missed" || (t.Status == "partial" && len(t.Items) > 1)
		return retryable && (len(types) == 0 || slices.Contains(types, t.Platform))
	})
}

func (d *Dispatcher) retryWhere(ctx context.Context, postID string, want func(store.Target) bool) (*store.Post, error) {
	if d.Store == nil {
		return nil, errors.New("retry requires a store")
	}
	ctx = logging.With(ctx, "post_id", postID)
	post, err := d.Store.GetPost(postID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, t := range post.Targets {
		if want(t) {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return post, nil
	}
	if err := d.drive(ctx, postID, nil, func() error {
		_, err := d.Store.MarkDue(ctx, postID, ids, d.now())
		return err
	}); err != nil {
		return nil, err
	}
	return d.Store.GetPost(postID)
}

// RetryRelay is RetryTargetRelay on the first target with the relay.
func (d *Dispatcher) RetryRelay(ctx context.Context, postID, relayURL string) (*store.Post, error) {
	return d.RetryTargetRelay(ctx, postID, 0, relayURL)
}

func (d *Dispatcher) Interact(ctx context.Context, s typeInteract) *store.Post {
	return d.InteractSpec(ctx, newID(), s.spec())
}

// InteractSpec plans, saves and sends an interaction inline.
func (d *Dispatcher) InteractSpec(ctx context.Context, id string, spec InteractSpec) *store.Post {
	ctx = logging.With(ctx, "post_id", id)
	if d.Store != nil {
		if err := ensureAccounts(d.Store, spec.Targets); err != nil {
			panic(err)
		}
	}
	rec := d.planInteract(ctx, id, spec)
	if err := d.drive(ctx, id, &memMedia{post: spec.Images, source: spec.SourceImages},
		func() error { return d.create(ctx, rec) }); err != nil {
		return notSaved(rec, err)
	}
	return d.recorded(ctx, rec)
}
