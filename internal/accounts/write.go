package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/redact"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// Accounts added, changed, adopted and deleted in the app. Each change that
// sets credentials opens the account with them first and asks its network
// who it is (Identity), so nothing is stored that the network refuses,
// and the same network account can't be added twice.

// AccountWriter is the store's side of the account writes.
type AccountWriter interface {
	Account(ctx context.Context, owner, id string) (store.Account, error)
	CreateAccountWithSecrets(ctx context.Context, n store.NewAccount) (store.Account, error)
	ReplaceAccountCredentials(ctx context.Context, owner, id string, c store.CredentialChange) (store.Account, error)
	AdoptAccount(ctx context.Context, owner, id string, c store.CredentialChange) (store.Account, error)
	DeleteAccount(ctx context.Context, owner, id, actor, tokenID string) error
	ForgetEnvAccount(ctx context.Context, owner, id, actor, tokenID string) error
}

// trialTimeout bounds opening an account with new credentials and asking
// its network who it is.
const trialTimeout = 15 * time.Second

// maxUpstreamDetail bounds the network's words quoted back to the owner.
const maxUpstreamDetail = 200

// FieldProblem is one field's problem; Pointer is the JSON pointer of the
// field in the request (/credentials/app_password, /config/pds_url).
type FieldProblem struct {
	Pointer, Code, Detail string
}

// InvalidError: what the owner sent can't be used.
type InvalidError struct{ Problems []FieldProblem }

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Pointer+": "+p.Detail)
	}
	return strings.Join(parts, "; ")
}

func invalid(pointer, code, detail string) *InvalidError {
	return &InvalidError{Problems: []FieldProblem{{Pointer: pointer, Code: code, Detail: detail}}}
}

// RejectedError: the network refused the credentials. Detail is its own
// words, without what the owner typed, scrubbed and short.
type RejectedError struct{ Detail string }

func (e *RejectedError) Error() string { return "the network refused the credentials: " + e.Detail }

// UpstreamError: the network couldn't be asked (unreachable, failing, or
// rate limiting); nothing was stored.
type UpstreamError struct{ Detail string }

func (e *UpstreamError) Error() string { return e.Detail }

// Errors of the account writes.
var (
	ErrIdentityChanged = errors.New("these credentials are another account on the network: add that one as a new account instead")
	ErrAlreadyInApp    = errors.New("the account is already managed in the app")
	ErrNothingToAdopt  = errors.New("the environment doesn't configure this account now: there is nothing to adopt")
	ErrNotRevealable   = errors.New("this account has no credential that can be shown")
	// ErrCredentialsUnreadable: an app-added account's stored credentials
	// can't be opened with the master key (changed or moved in the
	// database): connect it again.
	ErrCredentialsUnreadable = errors.New("this account's stored credentials can't be opened (changed or moved in the database, or its server address was edited there): connect it again")
	errNoWriter              = errors.New("this server can't change accounts")
)

// NewAccount is an account the owner adds in the app.
type NewAccount struct {
	Owner, Type, Label, Method string
	// Config and Credentials are as typed (JSON values): the manifest's
	// config fields, and the method's fields.
	Config, Credentials map[string]any
	Actor, TokenID      string
}

// CredentialUpdate changes an app-added account's configuration or
// credentials.
type CredentialUpdate struct {
	Owner, ID string
	// Method switches the way it connects ("" keeps it); a new method
	// needs its credentials.
	Method string
	// Config is a patch: the fields named are set, a null one goes back
	// to its default.
	Config map[string]any
	// Credentials, when set, replace the credentials; nil keeps them, and
	// so does a secret left out with the same method (a new handle, the
	// same app password). A new server address needs them again.
	Credentials map[string]any
	IfUpdatedAt time.Time
	Actor       string
	TokenID     string
}

// fieldOptions checks addresses the owner typed with the guard's rules.
func (r *Registry) fieldOptions(partial bool) destination.FieldOptions {
	return destination.FieldOptions{Partial: partial, URL: func(raw string, rule destination.URLRule) (string, error) {
		return r.opt.Guard.OwnerURL(raw, netguard.URLOptions{Secure: rule.Secure, Plain: rule.Plain, Path: rule.Path})
	}}
}

func (r *Registry) getenv(k string) string {
	if r.opt.Getenv != nil {
		return r.opt.Getenv(k)
	}
	return os.Getenv(k)
}

// Manifest is a type's manifest as the owner's forms show it: its config
// fields carry the operator's defaults for a new account (NOSTR_RELAYS).
func (r *Registry) Manifest(typ string) (destination.Manifest, bool) {
	d, ok := destination.Lookup(typ)
	if !ok {
		return destination.Manifest{}, false
	}
	m := d.Manifest()
	m.Config = r.configSpecs(d)
	return m, true
}

// configSpecs are the type's config fields with the operator's defaults
// for a new account.
func (r *Registry) configSpecs(d destination.Driver) []destination.FieldSpec {
	m := d.Manifest()
	if cd, ok := d.(destination.ConfigDefaulter); ok {
		return destination.WithDefaults(m.Config, cd.ConfigDefaults(r.getenv))
	}
	return m.Config
}

func problems(prefix string, errs []destination.FieldError) []FieldProblem {
	out := make([]FieldProblem, 0, len(errs))
	for _, e := range errs {
		out = append(out, FieldProblem{Pointer: prefix + e.Key, Code: e.Code, Detail: e.Detail})
	}
	return out
}

// methodOf finds a method the owner can connect with by typing (or
// generating) its credentials.
func methodOf(m destination.Manifest, id string) (destination.AuthMethod, *InvalidError) {
	if id == "" && len(m.Auth) == 1 {
		id = m.Auth[0].ID
	}
	a, ok := m.AuthMethod(id)
	if !ok {
		ids := make([]string, 0, len(m.Auth))
		for _, x := range m.Auth {
			ids = append(ids, x.ID)
		}
		code := "not_allowed"
		if id == "" {
			code = "required"
		}
		return a, invalid("/method", code, "use one of: "+strings.Join(ids, ", "))
	}
	if a.Flow != destination.FlowFields && a.Flow != destination.FlowGenerate {
		return a, invalid("/method", "not_allowed", "this way of connecting goes through the network's own sign-in page, which this version can't do yet")
	}
	return a, nil
}

// pointerOf is where a config or credential key was sent.
func pointerOf(a destination.AuthMethod, key string) string {
	for _, f := range a.Fields {
		if f.Key == key {
			return "/credentials/" + key
		}
	}
	return "/config/" + key
}

// discover lets the driver fill what the owner may leave out (a Bluesky
// PDS), validating what it finds as if typed.
func (r *Registry) discover(ctx context.Context, d destination.Driver, a destination.AuthMethod, config map[string]any) (map[string]any, error) {
	dc, ok := d.(destination.ConfigDiscoverer)
	if !ok {
		return config, nil
	}
	found, err := dc.DiscoverConfig(ctx, destination.DiscoverRequest{Method: a.ID, Config: maps.Clone(config),
		Net: destination.GuardedNet(r.opt.Guard.Allow), Tuning: r.opt.Tuning})
	var fe *destination.FieldError
	switch {
	case errors.As(err, &fe):
		return nil, invalid(pointerOf(a, fe.Key), fe.Code, fe.Detail)
	case err != nil:
		return nil, &UpstreamError{Detail: "looking up the account's server failed; try again"}
	}
	specs := slices.Concat(d.Manifest().Config, a.Fields)
	out := maps.Clone(config)
	for k, v := range found {
		i := slices.IndexFunc(specs, func(f destination.FieldSpec) bool { return f.Key == k && !f.Secret })
		if i < 0 {
			return nil, fmt.Errorf("the %s driver found %s, which isn't one of its fields", d.Manifest().Type, k)
		}
		got, errs := destination.ValidateFields(specs[i:i+1], map[string]any{k: v}, r.fieldOptions(false))
		if len(errs) > 0 {
			return nil, &InvalidError{Problems: problems(strings.TrimSuffix(pointerOf(a, k), k), errs)}
		}
		out[k] = got[k]
	}
	return out, nil
}

// trialHost is the host of an account opened to try new credentials: it
// reaches the network as an app-added account does, and keeps what the
// account saves (a token's provisional expiry) to be stored with it.
type trialHost struct {
	r   *Registry
	row store.Account

	mu      sync.Mutex
	secrets map[string]string
	config  map[string]any
}

func (h *trialHost) Save(_ context.Context, u destination.StateUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets, h.config = destination.MergeState(h.secrets, h.config, u)
	return nil
}

func (h *trialHost) Net(kind destination.NetPolicy) destination.Net {
	if kind == destination.NetAccount {
		return destination.GuardedNet(h.r.opt.Guard.Allow)
	}
	return destination.GuardedNet(h.r.opt.Guard.AllowContent)
}

func (h *trialHost) Logger() *slog.Logger {
	return h.r.log.With("account", h.row.ID, "account_type", h.row.Type, "trial", true)
}

// trial opens the account with the credentials and asks who it is. It
// returns the identity and the state it would keep (what the account
// saved while opening, merged in).
func (r *Registry) trial(ctx context.Context, d destination.Driver, row store.Account, config map[string]any, secrets map[string]string) (destination.Identity, map[string]string, map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, trialTimeout)
	defer cancel()
	h := &trialHost{r: r, row: row, secrets: maps.Clone(secrets), config: maps.Clone(config)}
	acct, err := r.opt.Open(cctx, row.Type, destination.OpenRequest{AccountID: row.ID, Method: row.AuthMethod,
		Config: maps.Clone(config), Secrets: maps.Clone(secrets), Tuning: r.opt.Tuning, Host: h})
	if err != nil {
		return destination.Identity{}, nil, nil, &InvalidError{Problems: []FieldProblem{{Pointer: "", Code: "invalid",
			Detail: "these settings can't be used: " + cleanDetail(err, secrets)}}}
	}
	defer closeAccount(acct)
	var ident destination.Identity
	if idr, ok := acct.(destination.Identifier); ok {
		if ident, err = idr.Identity(cctx); err != nil {
			return destination.Identity{}, nil, nil, r.trialError(d, err, secrets)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return ident, h.secrets, h.config, nil
}

// trialError says why the network didn't answer who the account is.
func (r *Registry) trialError(d destination.Driver, err error, secrets map[string]string) error {
	if errors.Is(err, destination.ErrBlockedAddress) {
		key := d.Manifest().AudienceKey()
		p := "/config/" + key
		if key == "" {
			p = ""
		}
		return invalid(p, "not_allowed", "that server is on a private or local address; the operator can allow it with PRIVATE_DESTINATIONS")
	}
	switch destination.Classify(err).Class {
	case destination.ClassAuth, destination.ClassInvalid, destination.ClassRejected:
		return &RejectedError{Detail: cleanDetail(err, secrets)}
	}
	// Logged as the owner would see it: a network may echo what it was
	// sent.
	r.log.Warn("trying an account's credentials: the network didn't answer", "type", d.Manifest().Type, "err", cleanDetail(err, secrets))
	return &UpstreamError{Detail: "the network didn't answer; try again in a moment"}
}

// cleanDetail is an upstream error as the owner may see it: the network's
// own message when it gave one, without the values the owner typed,
// scrubbed, on one line and short.
func cleanDetail(err error, secrets map[string]string) string {
	msg := err.Error()
	var he *destination.HTTPError
	if errors.As(err, &he) {
		var body struct {
			Error            any    `json:"error"`
			ErrorDescription string `json:"error_description"`
			Message          string `json:"message"`
		}
		if json.Unmarshal([]byte(he.Body), &body) == nil {
			var code string
			switch v := body.Error.(type) {
			case string:
				code = v
			case map[string]any:
				code, _ = v["message"].(string)
			}
			for _, s := range []string{body.ErrorDescription, body.Message, code} {
				if s != "" {
					msg = s
					break
				}
			}
		} else {
			msg = fmt.Sprintf("the server answered %d", he.StatusCode)
		}
	}
	for _, v := range secrets {
		if len(v) >= 4 {
			msg = strings.ReplaceAll(msg, v, "[redacted]")
		}
	}
	msg = redact.Upstream(msg)
	if len(msg) > maxUpstreamDetail {
		cut := maxUpstreamDetail
		for cut > 0 && msg[cut]&0xC0 == 0x80 {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return msg
}

// Create adds an account in the app: its fields are checked, its server
// found when left out, it is opened with its credentials and its network
// says who it is; then it is stored (credentials sealed for its server)
// and opened for use. The same network account twice is a
// *store.DuplicateAccountError.
func (r *Registry) Create(ctx context.Context, n NewAccount) (Entry, error) {
	if r.opt.Writes == nil {
		return Entry{}, errNoWriter
	}
	d, ok := destination.Lookup(n.Type)
	if !ok {
		return Entry{}, invalid("/type", "not_allowed", "no such type; GET /v1/destinations lists them")
	}
	m := d.Manifest()
	a, ierr := methodOf(m, n.Method)
	if ierr != nil {
		return Entry{}, ierr
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return Entry{}, err
	}
	creds, errs := destination.ValidateFields(a.Fields, n.Credentials, r.fieldOptions(false))
	config, cerrs := destination.ValidateFields(r.configSpecs(d), n.Config, r.fieldOptions(false))
	if ps := append(problems("/credentials/", errs), problems("/config/", cerrs)...); len(ps) > 0 {
		return Entry{}, &InvalidError{Problems: ps}
	}
	secrets, more := destination.SplitCredentials(a, creds)
	maps.Copy(config, more)
	if a.Flow == destination.FlowGenerate {
		g, ok := d.(destination.CredentialGenerator)
		if !ok {
			return Entry{}, invalid("/method", "not_allowed", "this type can't generate credentials")
		}
		var err error
		if secrets, err = g.GenerateCredentials(a.ID); err != nil {
			return Entry{}, fmt.Errorf("generating credentials: %w", err)
		}
	}
	config, err := r.discover(ctx, d, a, config)
	if err != nil {
		return Entry{}, err
	}
	row := store.Account{ID: store.NewAccountID(), OwnerID: n.Owner, Type: n.Type, Source: store.SourceDB, AuthMethod: a.ID}
	ident, secrets, config, err := r.trial(ctx, d, row, config, secrets)
	if err != nil {
		return Entry{}, err
	}
	put, _, err := sealUpdate(r.opt.Keys, r.opt.KeyState, n.Owner, row.ID, destination.AudienceOf(m, config), secrets)
	if err != nil {
		return Entry{}, err
	}
	label := strings.TrimSpace(n.Label)
	if label == "" {
		label = m.Name
	}
	saved, err := r.opt.Writes.CreateAccountWithSecrets(ctx, store.NewAccount{ID: row.ID, OwnerID: n.Owner, Type: n.Type,
		Label: label, Handle: ident.Handle, RemoteID: ident.RemoteID, AuthMethod: a.ID, Config: config, Secrets: put,
		Actor: n.Actor, TokenID: n.TokenID})
	if err != nil {
		return Entry{}, err
	}
	if r.opt.Seed != nil {
		// Stored: what follows finishes even if the caller has gone.
		ctx = context.WithoutCancel(ctx)
		if err := r.opt.Seed(ctx, saved); err != nil {
			r.log.Error("a new account's starting state wasn't set", "account", saved.ID, "type", saved.Type, "err", err)
		}
	}
	return r.Add(context.WithoutCancel(ctx), saved)
}

// specKeys are the config keys a method's account keeps from the owner:
// the type's config fields and the method's fields that aren't secret.
// Anything else in its config is state it saved (a token's expiry).
func specKeys(m destination.Manifest, a destination.AuthMethod) map[string]bool {
	keys := map[string]bool{}
	for _, f := range m.Config {
		keys[f.Key] = true
	}
	for _, f := range a.Fields {
		if !f.Secret {
			keys[f.Key] = true
		}
	}
	return keys
}

func rowConfig(row store.Account) (map[string]any, error) {
	config := map[string]any{}
	if row.Config != "" {
		if err := json.Unmarshal([]byte(row.Config), &config); err != nil {
			return nil, fmt.Errorf("its stored configuration can't be read: %w", err)
		}
	}
	return config, nil
}

// openSecrets opens an app-added account's stored credentials for
// audience.
func (r *Registry) openSecrets(ctx context.Context, row store.Account, audience string) (map[string]string, []store.SealedSecret, error) {
	if r.opt.Secrets == nil {
		return nil, nil, errNoWriter
	}
	sealed, err := r.opt.Secrets.Secrets(ctx, row.OwnerID, store.SecretScopeAccount, row.ID)
	if err != nil {
		return nil, nil, err
	}
	plain := make(map[string]string, len(sealed))
	for _, s := range sealed {
		b := Binding(s)
		b.Audience = audience
		v, err := r.opt.Keys.Open(b, s.Sealed)
		if err != nil {
			return nil, nil, openSecretError(err)
		}
		plain[s.Name] = v
	}
	return plain, sealed, nil
}

// openSecretError says why a stored credential didn't open, as an error
// the account API answers (409, not 500): no key, another key, or a sealed
// value that doesn't open.
func openSecretError(err error) error {
	switch {
	case errors.Is(err, secrets.ErrNoKey):
		return ErrMasterKeyRequired
	case errors.Is(err, secrets.ErrUnknownKey):
		return ErrMasterKeyMismatch
	default:
		return ErrCredentialsUnreadable
	}
}

// ChangeCredentials changes an app-added account's configuration or
// credentials: the result is opened and identified first, and must be the
// same network account (ErrIdentityChanged otherwise). A new server
// address needs the credentials again. An env account is
// store.ErrNotDBAccount.
func (r *Registry) ChangeCredentials(ctx context.Context, u CredentialUpdate) (Entry, error) {
	if r.opt.Writes == nil {
		return Entry{}, errNoWriter
	}
	row, err := r.opt.Writes.Account(ctx, u.Owner, u.ID)
	switch {
	case err != nil:
		return Entry{}, err
	case !row.DeletedAt.IsZero():
		return Entry{}, store.ErrAccountNotFound
	case row.Source != store.SourceDB:
		return Entry{}, store.ErrNotDBAccount
	}
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return Entry{}, fmt.Errorf("this version of Publisher has no driver for %s", row.Type)
	}
	m := d.Manifest()
	method := row.AuthMethod
	if u.Method != "" {
		method = u.Method
	}
	a, ierr := methodOf(m, method)
	if ierr != nil {
		return Entry{}, ierr
	}
	switch {
	case method != row.AuthMethod && u.Credentials == nil:
		return Entry{}, invalid("/credentials", "required", "a new way of connecting needs its credentials")
	case a.Flow == destination.FlowGenerate && u.Credentials != nil:
		return Entry{}, invalid("/method", "not_allowed", "a new key would be another account; add it as a new account instead")
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return Entry{}, err
	}
	cur, err := rowConfig(row)
	if err != nil {
		return Entry{}, err
	}
	// null puts a field back to the operator's default, as for a new
	// account.
	patch, perrs := destination.ValidateFields(r.configSpecs(d), u.Config, r.fieldOptions(true))
	var cerrs []destination.FieldError
	var secrets map[string]string
	// keep: saved secrets the owner left out of new credentials, with the
	// same way of connecting (a new handle, the same app password); they
	// are kept only while they go to the same server.
	var keep []string
	config := map[string]any{}
	fields := slices.Sorted(maps.Keys(patch))
	if u.Credentials != nil {
		specs := slices.Clone(a.Fields)
		if method == row.AuthMethod {
			for i, f := range specs {
				if _, given := u.Credentials[f.Key]; f.Secret && !given {
					specs[i].Required = false
					keep = append(keep, f.Key)
				}
			}
		}
		var creds map[string]any
		creds, cerrs = destination.ValidateFields(specs, u.Credentials, r.fieldOptions(false))
		var more map[string]any
		secrets, more = destination.SplitCredentials(a, creds)
		// Another identity (a new handle) may live on another server: its
		// address is found again unless the owner gives it.
		moved := false
		for _, f := range a.Fields {
			if !f.Secret && !sameValue(more[f.Key], cur[f.Key]) {
				moved = true
			}
		}
		for _, f := range m.Config { // new credentials start their state afresh
			if _, set := patch[f.Key]; moved && f.Audience && !set {
				continue
			}
			if v, ok := cur[f.Key]; ok {
				config[f.Key] = v
			}
		}
		maps.Copy(config, more)
		fields = append(fields, slices.Sorted(maps.Keys(creds))...)
	} else {
		config = maps.Clone(cur)
	}
	if ps := append(problems("/config/", perrs), problems("/credentials/", cerrs)...); len(ps) > 0 {
		return Entry{}, &InvalidError{Problems: ps}
	}
	for k, v := range patch {
		if v == nil {
			delete(config, k)
		} else {
			config[k] = v
		}
	}
	// Defaults fill what was reset; the owner's fields are checked whole.
	keys := specKeys(m, a)
	owned := map[string]any{}
	for k, v := range config {
		if keys[k] {
			owned[k] = v
		}
	}
	checked, errs := destination.ValidateFields(slices.Concat(r.configSpecs(d), nonSecret(a.Fields)), owned, r.fieldOptions(false))
	if len(errs) > 0 {
		ps := make([]FieldProblem, 0, len(errs))
		for _, e := range errs {
			ps = append(ps, FieldProblem{Pointer: pointerOf(a, e.Key), Code: e.Code, Detail: e.Detail})
		}
		return Entry{}, &InvalidError{Problems: ps}
	}
	maps.Copy(config, checked)
	// Nothing to change: answered without asking the network.
	if u.Credentials == nil && method == row.AuthMethod && sameValue(config, cur) {
		if en, ok := r.Get(row.ID); ok {
			return en, nil
		}
	}
	if config, err = r.discover(ctx, d, a, config); err != nil {
		return Entry{}, err
	}
	oldAudience := destination.AudienceOf(m, mustConfig(row))
	if u.Credentials == nil || len(keep) > 0 {
		if destination.AudienceOf(m, config) != oldAudience {
			ptr := "/credentials"
			if len(keep) > 0 {
				ptr += "/" + keep[0]
			}
			return Entry{}, invalid(ptr, "required", "a new server address needs the credentials again, so they never go to a server they weren't given for")
		}
		saved, _, err := r.openSecrets(ctx, row, oldAudience)
		if err != nil {
			return Entry{}, err
		}
		if u.Credentials == nil {
			secrets = saved
		}
		for _, k := range keep {
			if v, ok := saved[k]; ok {
				secrets[k] = v
			}
		}
	}
	trialRow := row
	trialRow.AuthMethod = a.ID
	ident, secrets, config, err := r.trial(ctx, d, trialRow, config, secrets)
	if err != nil {
		return Entry{}, err
	}
	if row.RemoteID != "" && ident.RemoteID != "" && ident.RemoteID != row.RemoteID {
		return Entry{}, ErrIdentityChanged
	}
	put, _, err := sealUpdate(r.opt.Keys, r.opt.KeyState, row.OwnerID, row.ID, destination.AudienceOf(m, config), secrets)
	if err != nil {
		return Entry{}, err
	}
	if u.Method != "" && u.Method != row.AuthMethod {
		fields = append(fields, "method")
	}
	next, err := r.opt.Writes.ReplaceAccountCredentials(ctx, row.OwnerID, row.ID, store.CredentialChange{AuthMethod: a.ID,
		Config: config, Secrets: put, Handle: ident.Handle, RemoteID: ident.RemoteID, IfUpdatedAt: u.IfUpdatedAt,
		Fields: fields, Actor: u.Actor, TokenID: u.TokenID})
	if err != nil {
		return Entry{}, err
	}
	return r.ReopenWith(context.WithoutCancel(ctx), next) // stored: finish even if the caller has gone
}

// envMethod is how an account from the environment connects: the one the
// environment names, else the first way of typing credentials whose
// required secrets the environment sets.
func envMethod(m destination.Manifest, env *EnvAccount) (destination.AuthMethod, bool) {
	if env.Method != "" {
		return m.AuthMethod(env.Method)
	}
	for _, a := range m.Auth {
		if a.Flow != destination.FlowFields {
			continue
		}
		ok := true
		for _, f := range a.Fields {
			if f.Secret && f.Required && env.Secrets[f.Key] == "" {
				ok = false
			}
		}
		if ok {
			return a, true
		}
	}
	return destination.AuthMethod{}, false
}

func nonSecret(fs []destination.FieldSpec) []destination.FieldSpec {
	out := make([]destination.FieldSpec, 0, len(fs))
	for _, f := range fs {
		if !f.Secret {
			out = append(out, f)
		}
	}
	return out
}

func mustConfig(row store.Account) map[string]any {
	c, _ := rowConfig(row)
	return c
}

// Adopt moves an account from the environment into the app: its current
// configuration and credentials (a renewed token included) are stored,
// sealed, and from then on its variables are ignored. Its addresses must
// pass the checks an address typed in the app does, since it then reaches
// the network as an app-added account.
func (r *Registry) Adopt(ctx context.Context, owner, id, actor, tokenID string) (Entry, error) {
	if r.opt.Writes == nil {
		return Entry{}, errNoWriter
	}
	row, err := r.opt.Writes.Account(ctx, owner, id)
	switch {
	case err != nil:
		return Entry{}, err
	case !row.DeletedAt.IsZero():
		return Entry{}, store.ErrAccountNotFound
	case row.Source != store.SourceEnv:
		return Entry{}, ErrAlreadyInApp
	}
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return Entry{}, fmt.Errorf("this version of Publisher has no driver for %s", row.Type)
	}
	env := r.envFor(row)
	if env == nil {
		return Entry{}, ErrNothingToAdopt
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return Entry{}, err
	}
	m := d.Manifest()
	secrets, config := maps.Clone(env.Secrets), maps.Clone(env.Config)
	if r.opt.State != nil {
		if secrets, config, err = r.opt.State.Restore(ctx, row, secrets, config); err != nil {
			return Entry{}, fmt.Errorf("its saved state can't be read: %w", err)
		}
	}
	a, ok := envMethod(m, env)
	if !ok {
		return Entry{}, errors.New("the environment's credentials for it match none of its type's ways of connecting")
	}
	// Its addresses as an app-added account would have them.
	var ps []FieldProblem
	for _, f := range slices.Concat(m.Config, nonSecret(a.Fields)) {
		v, ok := config[f.Key]
		if !ok || (f.Type != destination.FieldURL && f.Type != destination.FieldURLList) {
			continue
		}
		got, errs := destination.ValidateFields([]destination.FieldSpec{f}, map[string]any{f.Key: v}, r.fieldOptions(false))
		if len(errs) > 0 {
			for _, e := range errs {
				ps = append(ps, FieldProblem{Pointer: pointerOf(a, e.Key), Code: e.Code,
					Detail: "in the app this address would be refused (" + e.Detail + ")"})
			}
			continue
		}
		config[f.Key] = got[f.Key]
	}
	if len(ps) > 0 {
		return Entry{}, &InvalidError{Problems: ps}
	}
	put, _, err := sealUpdate(r.opt.Keys, r.opt.KeyState, row.OwnerID, row.ID, destination.AudienceOf(m, config), secrets)
	if err != nil {
		return Entry{}, err
	}
	fields := slices.Concat(slices.Sorted(maps.Keys(config)), slices.Sorted(maps.Keys(secrets)))
	next, err := r.opt.Writes.AdoptAccount(ctx, row.OwnerID, row.ID, store.CredentialChange{AuthMethod: a.ID, Config: config,
		Secrets: put, Fields: fields, Actor: actor, TokenID: tokenID})
	if err != nil {
		return Entry{}, err
	}
	return r.ReopenWith(context.WithoutCancel(ctx), next) // stored: finish even if the caller has gone
}

// Delete deletes an app-added account (store.DeleteAccount), or one from
// the environment whose variables are no longer set
// (store.ForgetEnvAccount), and closes it. An environment account still
// configured is store.ErrNotDBAccount.
func (r *Registry) Delete(ctx context.Context, owner, id, actor, tokenID string) error {
	if r.opt.Writes == nil {
		return errNoWriter
	}
	row, err := r.opt.Writes.Account(ctx, owner, id)
	if err != nil {
		return err
	}
	if row.Source == store.SourceEnv {
		// One the environment no longer configures can go; one it still
		// does is the environment's.
		if r.EnvConfigured(row) {
			return store.ErrNotDBAccount
		}
		err = r.opt.Writes.ForgetEnvAccount(ctx, owner, id, actor, tokenID)
	} else {
		err = r.opt.Writes.DeleteAccount(ctx, owner, id, actor, tokenID)
	}
	if err != nil {
		return err
	}
	if err := r.Remove(id); err != nil && !errors.Is(err, ErrNoAccount) {
		return err
	}
	return nil
}

// Reveal returns an app-added account's credentials that its type lets
// the owner see again (a Nostr key), in the form the owner would type.
func (r *Registry) Reveal(ctx context.Context, owner, id string) (map[string]string, error) {
	if r.opt.Writes == nil {
		return nil, errNoWriter
	}
	row, err := r.opt.Writes.Account(ctx, owner, id)
	switch {
	case err != nil:
		return nil, err
	case !row.DeletedAt.IsZero():
		return nil, store.ErrAccountNotFound
	case row.Source != store.SourceDB:
		return nil, store.ErrNotDBAccount
	}
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return nil, ErrNotRevealable
	}
	m := d.Manifest()
	plain, _, err := r.openSecrets(ctx, row, destination.AudienceOf(m, mustConfig(row)))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range plain {
		if f, ok := m.SecretField(k); ok && f.Revealable {
			if sf, ok := d.(destination.SecretFormatter); ok {
				v = sf.FormatSecret(k, v)
			}
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, ErrNotRevealable
	}
	return out, nil
}

// CredentialInfo says that a credential is set, never what it is.
type CredentialInfo struct {
	Set       bool      `json:"set"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Details is what the account API shows of an account's settings: its
// configuration (the owner's fields only, not saved state), which
// credentials are set, and, for an account from the environment, the
// fields the environment sets (locked in the app).
type Details struct {
	// Method is how it connects ("" when its type doesn't list it).
	Method       string
	Config       map[string]any
	Credentials  map[string]CredentialInfo
	LockedFields []string
}

// Details reads an account's settings for display.
func (r *Registry) Details(ctx context.Context, row store.Account) (Details, error) {
	out := Details{Config: map[string]any{}, Credentials: map[string]CredentialInfo{}}
	d, ok := destination.Lookup(row.Type)
	if !ok {
		return out, nil
	}
	m := d.Manifest()
	switch row.Source {
	case store.SourceDB:
		a, _ := m.AuthMethod(row.AuthMethod)
		out.Method = a.ID
		keys := specKeys(m, a)
		cfg, err := rowConfig(row)
		if err != nil {
			return out, err
		}
		for k, v := range cfg {
			if keys[k] {
				out.Config[k] = v
			}
		}
		if r.opt.Secrets != nil {
			sealed, err := r.opt.Secrets.Secrets(ctx, row.OwnerID, store.SecretScopeAccount, row.ID)
			if err != nil {
				return out, err
			}
			for _, s := range sealed {
				if !strings.HasPrefix(s.Name, "_") {
					out.Credentials[s.Name] = CredentialInfo{Set: true, UpdatedAt: s.UpdatedAt}
				}
			}
		}
	case store.SourceEnv:
		env := r.envFor(row)
		if env == nil {
			return out, nil
		}
		a, _ := envMethod(m, env)
		out.Method = a.ID
		keys := specKeys(m, a)
		for k, v := range env.Config {
			if keys[k] {
				out.Config[k] = v
				out.LockedFields = append(out.LockedFields, k)
			}
		}
		for _, f := range a.Fields {
			if f.Secret && env.Secrets[f.Key] != "" {
				out.Credentials[f.Key] = CredentialInfo{Set: true}
				out.LockedFields = append(out.LockedFields, f.Key)
			}
		}
		slices.Sort(out.LockedFields)
	}
	return out, nil
}

// sameValue reports whether two config values are the same as JSON (a
// stored value and one just typed).
func sameValue(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}
