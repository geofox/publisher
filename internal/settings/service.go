package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// Store is what the service reads and writes.
type Store interface {
	Settings(ctx context.Context, owner string) ([]store.SettingRow, []store.SealedSecret, error)
	ApplySettings(ctx context.Context, c store.SettingsChange) error
}

// Options build a Service.
type Options struct {
	Store Store
	// Owner is the owner whose settings Owner-scoped keys are.
	Owner string
	// Keys seal and open secret settings.
	Keys secrets.Keyring
	// CanSeal says whether a secret may be saved now (the master key's
	// status allows it); nil: always.
	CanSeal func() error
	// Env holds the variables the definitions read (EnvNames), read once
	// at startup, before they are removed from the environment.
	Env map[string]string
	// URL checks an address the owner typed (netguard.Policy.OwnerURL).
	URL func(raw string, rule destination.URLRule) (string, error)
	// Defs default to Defs().
	Defs []Def
}

// Service holds the current settings and saves changes.
type Service struct {
	o    Options
	defs []Def
	by   map[string]*Def

	mu   sync.Mutex // serializes writes and listener registration
	cur  atomic.Pointer[Snapshot]
	subs []func(*Snapshot)
}

// Errors of a write.
var (
	// ErrStale: If-Match didn't match the current version.
	ErrStale = errors.New("the settings changed since they were read")
)

// LockedError: the key is set by the environment.
type LockedError struct{ Key, Env string }

func (e *LockedError) Error() string {
	if e.Env == "" {
		return e.Key + " is set by the server's environment"
	}
	return e.Key + " is set by " + e.Env + " in the server's environment"
}

// ValidationError lists the fields a write refused, by key.
type ValidationError struct{ Fields []destination.FieldError }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Key+": "+f.Detail)
	}
	return strings.Join(parts, "; ")
}

// New loads the settings. An env value that can't be read is an error
// (the configuration should have refused it first).
func New(ctx context.Context, o Options) (*Service, error) {
	if o.Defs == nil {
		o.Defs = Defs()
	}
	if o.Owner == "" {
		o.Owner = store.DefaultOwner
	}
	s := &Service{o: o, defs: o.Defs, by: map[string]*Def{}}
	for i := range s.defs {
		d := &s.defs[i]
		d.Field.Key = d.Key
		if _, dup := s.by[d.Key]; dup {
			return nil, fmt.Errorf("settings: %s declared twice", d.Key)
		}
		s.by[d.Key] = d
	}
	snap, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	s.cur.Store(snap)
	return s, nil
}

// Defs returns the definitions of a scope, in order.
func (s *Service) Defs(scope Scope) []Def {
	var out []Def
	for _, d := range s.defs {
		if d.Scope == scope {
			out = append(out, d)
		}
	}
	return out
}

// Def returns a key's definition.
func (s *Service) Def(key string) (Def, bool) {
	d, ok := s.by[key]
	if !ok {
		return Def{}, false
	}
	return *d, true
}

// Instance is a type's InstanceConfig values and secrets now, under the
// driver's own keys (app_id, not threads.app_id): what
// accounts.Options.Instance answers.
func (s *Service) Instance(typ string) (map[string]any, map[string]string) {
	snap := s.Current()
	cfg, sec := map[string]any{}, map[string]string{}
	for _, d := range s.defs {
		if d.Driver == "" || d.Driver != typ {
			continue
		}
		if d.Field.Secret {
			if v := snap.Secret(d.Key); v != "" {
				sec[d.Name()] = v
			}
			continue
		}
		if v, ok := snap.Get(d.Key); ok && v.Value != nil {
			cfg[d.Name()] = v.Value
		}
	}
	return cfg, sec
}

// Current is the settings now. It never changes; a write makes a new one.
func (s *Service) Current() *Snapshot { return s.cur.Load() }

// OnChange calls fn with every new snapshot after a write (not with the
// current one).
func (s *Service) OnChange(fn func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

func (s *Service) ownerID(scope Scope) string {
	if scope == Instance {
		return store.SettingsInstance
	}
	return s.o.Owner
}

// binding is where a secret setting is sealed.
func (s *Service) binding(d *Def, audience string) secrets.Binding {
	return secrets.Binding{Owner: s.ownerID(d.Scope), Scope: store.SecretScopeSetting, Ref: d.Key, Name: "value", Audience: audience}
}

// load reads every setting: env, then saved, then default.
func (s *Service) load(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{vals: map[string]Value{}, secret: map[string]string{}}
	saved := map[string]store.SettingRow{}
	sealed := map[string]store.SealedSecret{}
	for _, scope := range []Scope{Owner, Instance} {
		rows, sec, err := s.o.Store.Settings(ctx, s.ownerID(scope))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if d, ok := s.by[r.Key]; ok && d.Scope == scope {
				saved[r.Key] = r
			}
		}
		for _, x := range sec {
			if d, ok := s.by[x.Ref]; ok && d.Scope == scope && x.Name == "value" {
				sealed[x.Ref] = x
			}
		}
	}
	// Addresses first: a secret bound to one needs it to open.
	order := slices.Clone(s.defs)
	sort.SliceStable(order, func(i, j int) bool { return order[i].AudienceOf == "" && order[j].AudienceOf != "" })
	for i := range order {
		d := s.by[order[i].Key]
		v := Value{Key: d.Key, Source: SourceDefault, Env: s.envName(d), def: d}
		ev, fromEnv, err := d.envValue(s.o.Env)
		if err != nil {
			return nil, err
		}
		switch {
		case fromEnv:
			v.Source, v.Locked = SourceEnv, true
			_, savedRow := saved[d.Key]
			_, savedSecret := sealed[d.Key]
			v.Behind = savedRow || savedSecret
			if d.Field.Secret {
				v.Set = ev.(string) != ""
				snap.secret[d.Key] = ev.(string)
				if d.Field.Type == destination.FieldURL {
					v.Origin = Origin(ev.(string))
				}
			} else {
				v.Value = ev
			}
		case d.Field.Secret:
			x, ok := sealed[d.Key]
			if !ok {
				break
			}
			v.UpdatedAt = x.UpdatedAt
			want := x.Audience
			if d.AudienceOf != "" {
				want = Origin(snap.secret[d.AudienceOf])
			}
			if x.Audience != want {
				v.Broken = "saved for another address: type it again"
				break
			}
			plain, err := s.o.Keys.Open(s.binding(d, x.Audience), x.Sealed)
			if err != nil {
				v.Broken = "can't be opened: " + openReason(err)
				slog.Warn("settings: a saved secret can't be opened", "key", d.Key, "err", err)
				break
			}
			if d.Field.Type == destination.FieldURL && Origin(plain) != x.Audience {
				v.Broken = "can't be opened: it doesn't match where it was saved for"
				break
			}
			v.Source, v.Set = SourceSaved, true
			snap.secret[d.Key] = plain
			if d.Field.Type == destination.FieldURL {
				v.Origin = x.Audience
			}
		default:
			r, ok := saved[d.Key]
			if !ok {
				break
			}
			val, err := decode(d, r.ValueJSON)
			if err != nil {
				slog.Warn("settings: a saved value can't be read; the default applies", "key", d.Key, "err", err)
				break
			}
			v.Source, v.Value, v.UpdatedAt = SourceSaved, val, r.UpdatedAt
		}
		if v.Source == SourceDefault && !d.Field.Secret {
			v.Value = d.Field.Default
		}
		snap.vals[d.Key] = v
	}
	snap.versions = map[Scope]string{Owner: s.version(snap, Owner), Instance: s.version(snap, Instance)}
	return snap, nil
}

func openReason(err error) string {
	switch {
	case errors.Is(err, secrets.ErrNoKey):
		return "the master key isn't set"
	case errors.Is(err, secrets.ErrUnknownKey):
		return "it was saved with another master key"
	}
	return "it was changed or damaged"
}

// decode reads a saved value by its field's type.
func decode(d *Def, raw string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	out, errs := destination.ValidateFields([]destination.FieldSpec{d.Field}, map[string]any{d.Key: v},
		destination.FieldOptions{Partial: true, URL: func(raw string, _ destination.URLRule) (string, error) { return raw, nil }})
	if len(errs) > 0 {
		return nil, errors.New(errs[0].Detail)
	}
	return out[d.Key], nil
}

// version is a scope's ETag: it changes with any value, source or save.
func (s *Service) version(snap *Snapshot, scope Scope) string {
	h := sha256.New()
	for _, d := range s.defs {
		if d.Scope != scope {
			continue
		}
		v := snap.vals[d.Key]
		b, _ := json.Marshal(v.Value)
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\x00%s\x00%d\x00%t\x00", d.Key, v.Source, b, v.Set, v.Broken, v.UpdatedAt.UnixNano(), v.Behind)
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// Origin is an address's scheme://host[:port] ("" when it isn't one).
func Origin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// Change is one write: values by key (nil: back to the default, or a
// secret cleared).
type Change struct {
	Scope  Scope
	Values map[string]any
	// IfMatch is the version the writer read ("" or "*": any).
	IfMatch string
	// By is who writes, for the record ("session", "token:<id>").
	By string
}

// Apply validates and saves a change, then publishes the new snapshot.
// A locked key is a *LockedError, bad values a *ValidationError, a
// secret without a usable master key the CanSeal error.
func (s *Service) Apply(ctx context.Context, c Change) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.Current()
	if c.IfMatch != "" && c.IfMatch != "*" && c.IfMatch != cur.versions[c.Scope] {
		return cur, ErrStale
	}
	keys := make([]string, 0, len(c.Values))
	for k := range c.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fieldErrs []destination.FieldError
	bad := func(key, code, detail string) {
		fieldErrs = append(fieldErrs, destination.FieldError{Key: key, Code: code, Detail: detail})
	}
	ch := store.SettingsChange{Owner: s.ownerID(c.Scope), Put: map[string]string{}, By: c.By}
	newSecret := map[string]string{} // key → plaintext ("" cleared)
	for _, k := range keys {
		d, ok := s.by[k]
		if !ok || d.Scope != c.Scope {
			bad(k, "unknown_field", "not a setting here")
			continue
		}
		if cur.vals[k].Locked {
			return cur, &LockedError{Key: k, Env: s.envName(d)}
		}
		raw := c.Values[k]
		if raw == nil {
			if d.Field.Secret {
				ch.ClearSecrets = append(ch.ClearSecrets, k)
				newSecret[k] = ""
			} else {
				ch.Clear = append(ch.Clear, k)
			}
			continue
		}
		out, errs := destination.ValidateFields([]destination.FieldSpec{d.Field}, map[string]any{k: raw},
			destination.FieldOptions{Partial: true, URL: s.o.URL})
		if len(errs) > 0 {
			fieldErrs = append(fieldErrs, errs...)
			continue
		}
		v := out[k]
		if v == nil { // an empty value: back to the default
			if d.Field.Secret {
				ch.ClearSecrets = append(ch.ClearSecrets, k)
				newSecret[k] = ""
			} else {
				ch.Clear = append(ch.Clear, k)
			}
			continue
		}
		if d.Field.Secret {
			newSecret[k] = v.(string)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return cur, err
		}
		ch.Put[k] = string(b)
	}
	// Blossom chosen, it needs its address.
	if c.Scope == Instance {
		store, url := cur.String(MediaStore), cur.String(MediaBlossomURL)
		if v, ok := ch.Put[MediaStore]; ok {
			_ = json.Unmarshal([]byte(v), &store)
		} else if slices.Contains(ch.Clear, MediaStore) {
			store = StoreNone
		}
		if v, ok := ch.Put[MediaBlossomURL]; ok {
			_ = json.Unmarshal([]byte(v), &url)
		} else if slices.Contains(ch.Clear, MediaBlossomURL) && !cur.vals[MediaBlossomURL].Locked {
			url = ""
		}
		if hasKey(c.Values, MediaStore) || hasKey(c.Values, MediaBlossomURL) {
			if store == StoreBlossom && strings.TrimSpace(url) == "" {
				bad(MediaBlossomURL, "required", "a Blossom server's address is needed to use one")
			}
		}
	}
	// A secret bound to an address: a new origin needs it again, and an
	// address cleared takes it along.
	for _, d := range s.defs {
		if d.Scope != c.Scope || d.AudienceOf == "" {
			continue
		}
		addr, changed := newSecret[d.AudienceOf]
		if !changed || Origin(addr) == Origin(cur.secret[d.AudienceOf]) {
			continue
		}
		if cur.vals[d.Key].Locked && addr != "" {
			// The operator's secret goes only to the operator's address.
			bad(d.AudienceOf, "locked", fmt.Sprintf("%s is set by %s, which goes only to an address from the environment: set %s there too, or remove %s",
				d.Field.Label, d.Env, s.by[d.AudienceOf].Env, d.Env))
			continue
		}
		if _, given := newSecret[d.Key]; given || cur.vals[d.Key].Locked || !cur.vals[d.Key].Set {
			continue
		}
		if addr == "" {
			ch.ClearSecrets = append(ch.ClearSecrets, d.Key)
			newSecret[d.Key] = ""
			continue
		}
		bad(d.Key, "required", fmt.Sprintf("%s goes with the address: type it again for the new one", d.Field.Label))
	}
	if len(fieldErrs) > 0 {
		sort.SliceStable(fieldErrs, func(i, j int) bool { return fieldErrs[i].Key < fieldErrs[j].Key })
		return cur, &ValidationError{Fields: fieldErrs}
	}
	for _, k := range sortedKeys(newSecret) {
		plain := newSecret[k]
		if plain == "" {
			continue
		}
		if s.o.CanSeal != nil {
			if err := s.o.CanSeal(); err != nil {
				return cur, err
			}
		}
		d := s.by[k]
		aud := ""
		switch {
		case d.Field.Type == destination.FieldURL:
			aud = Origin(plain)
		case d.AudienceOf != "":
			if a, ok := newSecret[d.AudienceOf]; ok {
				aud = Origin(a)
			} else {
				aud = Origin(cur.secret[d.AudienceOf])
			}
		}
		x, err := s.seal(d, aud, plain)
		if err != nil {
			return cur, err
		}
		ch.PutSecrets = append(ch.PutSecrets, x)
	}
	if err := s.o.Store.ApplySettings(ctx, ch); err != nil {
		return cur, err
	}
	return s.reload(ctx)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Service) seal(d *Def, audience, plain string) (store.SealedSecret, error) {
	sealed, err := s.o.Keys.Seal(s.binding(d, audience), plain)
	if err != nil {
		return store.SealedSecret{}, err
	}
	kid, err := secrets.KeyID(sealed)
	if err != nil {
		return store.SealedSecret{}, err
	}
	b := s.binding(d, audience)
	return store.SealedSecret{Owner: b.Owner, Scope: b.Scope, Ref: b.Ref, Name: b.Name, Audience: audience, Sealed: sealed, KeyID: kid}, nil
}

// reload reads the store again and publishes the snapshot (s.mu held).
func (s *Service) reload(ctx context.Context) (*Snapshot, error) {
	snap, err := s.load(ctx)
	if err != nil {
		return s.Current(), fmt.Errorf("%w: %w", ErrSavedNotRead, err)
	}
	s.cur.Store(snap)
	for _, fn := range s.subs {
		fn(snap)
	}
	return snap, nil
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

// ErrSavedNotRead: a change was saved, but reading the settings back
// failed; the one in effect is the last read until the next succeeds.
var ErrSavedNotRead = errors.New("the change was saved, but reading the settings back failed")

// ErrNotFromEnv: a setting copied into the app isn't set by the
// environment.
var ErrNotFromEnv = errors.New("isn't set by the environment")

// CopyFromEnv saves a key's environment value in the app, so the
// variable can be removed without the setting going back to its default.
// The setting stays locked while the variable is set. What one variable
// sets together is copied together (BLOSSOM_URL sets the store and its
// address), and so are an address and the secret that goes with it, when
// the environment sets both. Each value is checked as one typed in the app
// is: a value the app would refuse is refused here too, rather than saved
// and dropped at the next start.
func (s *Service) CopyFromEnv(ctx context.Context, key, by string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.Current()
	d, ok := s.by[key]
	if !ok {
		return cur, &ValidationError{Fields: []destination.FieldError{{Key: key, Code: "unknown_field", Detail: "not a setting"}}}
	}
	if cur.vals[key].Source != SourceEnv {
		return cur, ErrNotFromEnv
	}
	ch := store.SettingsChange{Owner: s.ownerID(d.Scope), Put: map[string]string{}, By: by}
	var fieldErrs []destination.FieldError
	for _, k := range s.copiedWith(cur, key) {
		d, v := s.by[k], cur.vals[k]
		var raw any
		if d.Field.Secret {
			raw = cur.secret[k]
		} else {
			// As a saved value is read back: through JSON.
			b, err := json.Marshal(v.Value)
			if err != nil {
				return cur, err
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return cur, err
			}
		}
		out, errs := destination.ValidateFields([]destination.FieldSpec{d.Field}, map[string]any{k: raw},
			destination.FieldOptions{Partial: true, URL: s.o.URL})
		if len(errs) > 0 {
			for _, e := range errs {
				e.Key = k
				e.Detail = s.envName(d) + "'s value can't be kept in the app: " + e.Detail + ". Change the variable, then copy it again"
				fieldErrs = append(fieldErrs, e)
			}
			continue
		}
		if out[k] == nil {
			continue
		}
		if d.Field.Secret {
			if s.o.CanSeal != nil {
				if err := s.o.CanSeal(); err != nil {
					return cur, err
				}
			}
			plain := out[k].(string)
			aud := ""
			switch {
			case d.Field.Type == destination.FieldURL:
				aud = Origin(plain)
			case d.AudienceOf != "":
				aud = Origin(cur.secret[d.AudienceOf])
			}
			x, err := s.seal(d, aud, plain)
			if err != nil {
				return cur, err
			}
			ch.PutSecrets = append(ch.PutSecrets, x)
			continue
		}
		b, err := json.Marshal(out[k])
		if err != nil {
			return cur, err
		}
		ch.Put[k] = string(b)
	}
	if len(fieldErrs) > 0 {
		return cur, &ValidationError{Fields: fieldErrs}
	}
	if err := s.o.Store.ApplySettings(ctx, ch); err != nil {
		return cur, err
	}
	return s.reload(ctx)
}

// copiedWith is key and what is copied along with it, each set by the
// environment: the settings one variable sets together, and an address
// with the secret that goes with it.
func (s *Service) copiedWith(cur *Snapshot, key string) []string {
	d := s.by[key]
	out := []string{key}
	add := func(k string) {
		if k != "" && !slices.Contains(out, k) && cur.vals[k].Source == SourceEnv {
			out = append(out, k)
		}
	}
	for _, o := range s.defs {
		if o.Scope != d.Scope || o.Key == key {
			continue
		}
		if (s.envName(d) != "" && s.envName(&o) == s.envName(d)) || o.AudienceOf == key {
			add(o.Key)
		}
	}
	add(d.AudienceOf)
	sort.Strings(out[1:])
	return out
}

// envName is the variable that sets d: its own, or the one its EnvFunc
// reads (media.store follows BLOSSOM_URL).
func (s *Service) envName(d *Def) string {
	if d.Env != "" {
		return d.Env
	}
	if d.EnvVar != "" {
		return d.EnvVar
	}
	return ""
}

// Value is one setting as it stands.
type Value struct {
	Key    string
	Value  any // nil for a secret
	Source Source
	Env    string
	Locked bool
	// Set: a secret has a value. Origin: a secret address's origin.
	Set    bool
	Origin string
	// Broken says why a saved secret can't be used ("" when it can).
	Broken string
	// Behind: the environment sets it, and a value saved in the app
	// waits behind, applying once the variable is removed.
	Behind    bool
	UpdatedAt time.Time
	def       *Def
}

// Def is the value's definition.
func (v Value) Def() Def { return *v.def }

// Snapshot is the settings at one moment. Its secrets never leave it but
// by Secret.
type Snapshot struct {
	vals     map[string]Value
	secret   map[string]string
	versions map[Scope]string
}

// Get returns a setting.
func (s *Snapshot) Get(key string) (Value, bool) {
	v, ok := s.vals[key]
	return v, ok
}

// Version is a scope's ETag.
func (s *Snapshot) Version(scope Scope) string { return s.versions[scope] }

// Bool, Int, String, Strings and Duration read a value by its type (the
// zero value when unset or of another type).
func (s *Snapshot) Bool(key string) bool { b, _ := s.vals[key].Value.(bool); return b }

func (s *Snapshot) Int(key string) int {
	switch n := s.vals[key].Value.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func (s *Snapshot) String(key string) string { str, _ := s.vals[key].Value.(string); return str }

func (s *Snapshot) Strings(key string) []string {
	switch l := s.vals[key].Value.(type) {
	case []string:
		return slices.Clone(l)
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if str, ok := x.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func (s *Snapshot) Duration(key string) time.Duration {
	d, _ := time.ParseDuration(s.String(key))
	return d
}

// Secret is a secret setting's value ("" when unset or unusable).
func (s *Snapshot) Secret(key string) string { return s.secret[key] }

// MarshalJSON keeps secrets out of any JSON a snapshot ends up in.
func (s *Snapshot) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }
