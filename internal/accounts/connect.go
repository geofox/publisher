package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// Connecting an account by signing in at its network (spec §6.1). The
// owner starts a flow; Publisher sends them to the network with a state
// (32 random bytes, kept only as a hash, good for one callback within 10
// minutes) and a PKCE challenge; the network sends them back to
// /oauth/callback/<type> with a code, which the driver turns into
// credentials. The account is then stored as one added with typed
// credentials is: opened, identified, sealed for its server.
//
// The callback needs no session (a home-screen app may come back in
// another browser): the state is the capability. A session that is there
// must be the flow's user's. Without one, the account waits for the owner
// to confirm it in the tab that started the flow (a :choose of one), so a
// sign-in link that leaked can't add someone else's account unseen. The
// tab that started the flow reads its outcome.

const (
	// FlowTTL is how long a flow waits for its callback.
	FlowTTL = 10 * time.Minute
	// flowKept is how long an expired flow's outcome stays readable.
	flowKept = time.Hour
	// claimTimeout: a flow whose callback came this long ago without an
	// outcome was interrupted (a restart, a failed write). Its requests
	// are bounded far below it.
	claimTimeout = 2 * time.Minute
)

// FlowStore is the store's side of the connect flows.
type FlowStore interface {
	CreateOAuthFlow(ctx context.Context, f store.OAuthFlow, stateHash string, data []store.SealedSecret) error
	OAuthFlow(ctx context.Context, owner, id string) (store.OAuthFlow, error)
	OAuthFlowByState(ctx context.Context, stateHash string) (store.OAuthFlow, error)
	ClaimOAuthFlow(ctx context.Context, id string, now time.Time) error
	FinishOAuthFlow(ctx context.Context, owner, id string, o store.FlowOutcome) error
	PurgeOAuthFlows(ctx context.Context, cutoff time.Time) (int, error)
	OAuthClientFor(ctx context.Context, owner, typ, issuer, redirectURI string) (store.OAuthClient, error)
	SaveOAuthClient(ctx context.Context, c store.OAuthClient, secrets []store.SealedSecret) error
	DeleteOAuthClient(ctx context.Context, owner, id string) error
	Secrets(ctx context.Context, owner, scope, ref string) ([]store.SealedSecret, error)
}

// ConnectStart is a flow the owner starts.
type ConnectStart struct {
	Owner, User, Actor, TokenID string
	Type, Method, Label         string
	Config                      map[string]any
	// Reconnect is the account a reconnect is for ("": a new account).
	Reconnect string
	// RedirectBase is Publisher's public origin (PUBLIC_URL, or the
	// OIDC app's base): the callback is RedirectBase/oauth/callback/<type>.
	RedirectBase string
}

// StartedFlow is a flow under way: the owner opens AuthorizeURL.
type StartedFlow struct {
	ID           string
	AuthorizeURL string
	ExpiresAt    time.Time
}

// FlowView is a flow as its owner reads it.
type FlowView struct {
	ID         string
	Status     string
	AccountID  string
	Error      string
	Type       string
	ExpiresAt  time.Time
	Candidates []destination.Identity
}

// Connect errors (besides *InvalidError and destination.ConnectError).
var (
	ErrNoRedirectBase = errors.New("set PUBLIC_URL first: the network sends you back to Publisher's public address")
	ErrNoFlows        = errors.New("this server can't connect accounts by signing in at their network")
)

// flowData is what a flow keeps sealed between its two steps.
type flowData struct {
	Driver   json.RawMessage `json:"driver,omitempty"`
	Verifier string          `json:"verifier"`
	Config   map[string]any  `json:"config,omitempty"`
}

// candidate is one identity a network offered, with its credentials.
type candidate struct {
	Config   map[string]any       `json:"config,omitempty"`
	Secrets  map[string]string    `json:"secrets,omitempty"`
	Identity destination.Identity `json:"identity"`
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// StateHash is how a flow is found by its state.
func StateHash(state string) string {
	sum := sha256.Sum256([]byte("publisher/oauth-state/v1\x00" + state))
	return hex.EncodeToString(sum[:])
}

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RedirectURI is the callback address of a type's flows.
func RedirectURI(base, typ string) string {
	return strings.TrimRight(base, "/") + "/oauth/callback/" + url.PathEscape(typ)
}

// flowBinding binds a flow's sealed values to its id, owner, user and
// type.
func flowBinding(f store.OAuthFlow, name string) secrets.Binding {
	return secrets.Binding{Owner: f.OwnerID, Scope: secrets.ScopeOAuthFlow, Ref: f.ID, Name: name, Audience: f.UserID + "\x1f" + f.Type}
}

func (r *Registry) sealFlow(f store.OAuthFlow, name string, v any) (store.SealedSecret, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return store.SealedSecret{}, err
	}
	b := flowBinding(f, name)
	sealed, err := r.opt.Keys.Seal(b, string(raw))
	if err != nil {
		return store.SealedSecret{}, err
	}
	kid, err := secrets.KeyID(sealed)
	if err != nil {
		return store.SealedSecret{}, err
	}
	return store.SealedSecret{Owner: b.Owner, Scope: b.Scope, Ref: b.Ref, Name: b.Name, Audience: b.Audience, Sealed: sealed, KeyID: kid}, nil
}

func (r *Registry) openFlow(ctx context.Context, f store.OAuthFlow, name string, v any) error {
	ss, err := r.opt.Flows.Secrets(ctx, f.OwnerID, secrets.ScopeOAuthFlow, f.ID)
	if err != nil {
		return err
	}
	for _, s := range ss {
		if s.Name != name {
			continue
		}
		plain, err := r.opt.Keys.Open(flowBinding(f, name), s.Sealed)
		if err != nil {
			return &destination.ConnectError{Code: destination.ConnectMasterKey, Err: err}
		}
		return json.Unmarshal([]byte(plain), v)
	}
	return &destination.ConnectError{Code: destination.ConnectExpired, Err: errors.New("the flow's data is gone")}
}

// connectorOf is a type's driver, its connector and the redirect method.
func connectorOf(typ, method string) (destination.Driver, destination.Connector, destination.AuthMethod, error) {
	d, ok := destination.Lookup(typ)
	if !ok {
		return nil, nil, destination.AuthMethod{}, invalid("/type", "not_allowed", "no such type; GET /v1/destinations lists them")
	}
	m := d.Manifest()
	var ids []string
	for _, a := range m.Auth {
		if a.Flow == destination.FlowRedirect {
			ids = append(ids, a.ID)
		}
	}
	if method == "" && len(ids) > 0 {
		method = ids[0]
	}
	a, ok := m.AuthMethod(method)
	conn, can := d.(destination.Connector)
	switch {
	case len(ids) == 0 || !can:
		return nil, nil, destination.AuthMethod{}, invalid("/type", "not_allowed", "accounts of this type can't be connected by signing in at the network: add one with its credentials")
	case !ok || a.Flow != destination.FlowRedirect:
		return nil, nil, destination.AuthMethod{}, invalid("/method", "not_allowed", "use one of: "+strings.Join(ids, ", "))
	}
	return d, conn, a, nil
}

// connectRequest is what a type's connector gets for a flow.
func (r *Registry) connectRequest(owner string, f store.OAuthFlow, a destination.AuthMethod, config map[string]any) (destination.ConnectRequest, error) {
	n := destination.GuardedNet(r.opt.Guard.Allow)
	if r.opt.ConnectNet != nil {
		n = r.opt.ConnectNet(f.Type)
	}
	req := destination.ConnectRequest{Method: a.ID, Config: config, RedirectURI: f.RedirectURI,
		Net:     n,
		Clients: &oauthClients{r: r, owner: owner, typ: f.Type, redirect: f.RedirectURI},
		Logger:  r.log.With("connect_flow", f.ID, "account_type", f.Type)}
	if r.opt.Instance != nil {
		req.InstanceConfig, req.InstanceSecrets = r.opt.Instance(f.Type)
	}
	if why := missingInstance(f.Type, a, req.InstanceConfig, req.InstanceSecrets); why != "" {
		return req, invalid("/method", "not_allowed", why)
	}
	return req, nil
}

// missingInstance says what server setting a method needs first, or "".
func missingInstance(typ string, a destination.AuthMethod, cfg map[string]any, sec map[string]string) string {
	for _, k := range a.NeedsInstanceConfig {
		if v, ok := cfg[k]; ok && v != nil && v != "" {
			continue
		}
		if sec[k] != "" {
			continue
		}
		name := typ
		if d, ok := destination.Lookup(typ); ok {
			name = d.Manifest().Name
		}
		return "this needs the server's " + name + " app first (Settings, Integrations)"
	}
	return ""
}

// ConnectUnavailable says why a type's sign-in method can't start now
// with redirectURI, as starting it would ("" when it can, or it isn't
// one).
func (r *Registry) ConnectUnavailable(typ string, a destination.AuthMethod, redirectURI string) string {
	if a.Flow != destination.FlowRedirect {
		return ""
	}
	if r.opt.Flows == nil || r.opt.Writes == nil {
		return ErrNoFlows.Error()
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return err.Error()
	}
	var cfg map[string]any
	var sec map[string]string
	if r.opt.Instance != nil {
		cfg, sec = r.opt.Instance(typ)
	}
	if why := missingInstance(typ, a, cfg, sec); why != "" {
		return why
	}
	if d, ok := destination.Lookup(typ); ok {
		if rc, ok := d.(destination.RedirectChecker); ok {
			return rc.RedirectProblem(a.ID, redirectURI)
		}
	}
	return ""
}

// BeginConnect starts a flow: a new account, or a reconnect of an
// app-added one (its own config unless q.Config sets some).
func (r *Registry) BeginConnect(ctx context.Context, q ConnectStart) (StartedFlow, error) {
	if r.opt.Flows == nil || r.opt.Writes == nil {
		return StartedFlow{}, ErrNoFlows
	}
	if strings.TrimSpace(q.RedirectBase) == "" {
		return StartedFlow{}, ErrNoRedirectBase
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return StartedFlow{}, err
	}
	typ, method, cur := q.Type, q.Method, map[string]any{}
	if q.Reconnect != "" {
		row, err := r.opt.Writes.Account(ctx, q.Owner, q.Reconnect)
		switch {
		case err != nil:
			return StartedFlow{}, err
		case !row.DeletedAt.IsZero():
			return StartedFlow{}, store.ErrAccountNotFound
		case row.Source != store.SourceDB:
			return StartedFlow{}, store.ErrNotDBAccount
		}
		typ = row.Type
		if cur, err = rowConfig(row); err != nil {
			return StartedFlow{}, err
		}
	}
	d, conn, a, err := connectorOf(typ, method)
	if err != nil {
		return StartedFlow{}, err
	}
	// The owner's fields: the type's config and the method's fields (an
	// instance's address); a reconnect keeps its own unless given.
	specs := slices.Concat(r.configSpecs(d), nonSecret(a.Fields))
	given := map[string]any{}
	for _, f := range specs {
		if v, ok := q.Config[f.Key]; ok {
			given[f.Key] = v
		} else if v, ok := cur[f.Key]; ok {
			given[f.Key] = v
		}
	}
	for k := range q.Config {
		if !slices.ContainsFunc(specs, func(f destination.FieldSpec) bool { return f.Key == k }) {
			return StartedFlow{}, invalid("/config/"+k, "unknown_field", "no such field")
		}
	}
	config, errs := destination.ValidateFields(specs, given, r.fieldOptions(false))
	if len(errs) > 0 {
		return StartedFlow{}, &InvalidError{Problems: problems("/config/", errs)}
	}
	state, err := randomToken()
	if err != nil {
		return StartedFlow{}, err
	}
	verifier, err := randomToken()
	if err != nil {
		return StartedFlow{}, err
	}
	now := r.now()
	f := store.OAuthFlow{ID: store.NewFlowID(), OwnerID: q.Owner, UserID: q.User, Type: typ, Method: a.ID,
		AccountID: q.Reconnect, Label: strings.TrimSpace(q.Label), RedirectURI: RedirectURI(q.RedirectBase, typ),
		ExpiresAt: now.Add(FlowTTL)}
	req, err := r.connectRequest(q.Owner, f, a, config)
	if err != nil {
		return StartedFlow{}, err
	}
	req.State, req.CodeChallenge = state, challengeOf(verifier)
	start, err := conn.BeginConnect(ctx, req)
	if err != nil {
		return StartedFlow{}, r.connectFailure(f, err)
	}
	data, err := r.sealFlow(f, "data", flowData{Driver: start.Data, Verifier: verifier, Config: config})
	if err != nil {
		return StartedFlow{}, err
	}
	if err := r.opt.Flows.CreateOAuthFlow(ctx, f, StateHash(state), []store.SealedSecret{data}); err != nil {
		return StartedFlow{}, err
	}
	return StartedFlow{ID: f.ID, AuthorizeURL: start.AuthorizeURL, ExpiresAt: f.ExpiresAt}, nil
}

// connectFailure logs a driver's failure (its error may quote the
// network) and answers it as a *destination.ConnectError.
func (r *Registry) connectFailure(f store.OAuthFlow, err error) error {
	// A driver's own word on what the owner must change first (Threads:
	// PUBLIC_URL must be https), with its fixed text.
	var fe *destination.FieldError
	if errors.As(err, &fe) {
		ptr := "/" + fe.Key
		if fe.Key != "method" {
			ptr = "/config/" + fe.Key // a field the owner typed (a server's address)
		}
		return invalid(ptr, fe.Code, fe.Detail)
	}
	// Publisher's own store failed (keeping a server's app): not the
	// network's doing.
	var le *localError
	if errors.As(err, &le) {
		return le.err
	}
	var ce *destination.ConnectError
	if !errors.As(err, &ce) {
		ce = &destination.ConnectError{Code: destination.ConnectExchangeFailed, Err: err}
	}
	r.log.Warn("connecting an account failed", "connect_flow", f.ID, "account_type", f.Type, "code", ce.Code,
		"err", cleanDetail(err, nil))
	if errors.Is(err, destination.ErrBlockedAddress) {
		p := ""
		if d, ok := destination.Lookup(f.Type); ok && d.Manifest().AudienceKey() != "" {
			p = "/config/" + d.Manifest().AudienceKey()
		}
		return invalid(p, "not_allowed", "that server is on a private or local address; the operator can allow it with PRIVATE_DESTINATIONS")
	}
	return ce
}

// CompleteConnect is the callback of a type's flow: it finds the flow by
// state, claims it once, and finishes it. sessionUser is the user of a
// session that came with the callback ("" when none). It answers the
// flow as it now stands; a callback that came before answers the first
// outcome. An error is a *destination.ConnectError when no flow is
// known to report on (state_invalid).
func (r *Registry) CompleteConnect(ctx context.Context, typ string, callback url.Values, sessionUser string) (FlowView, error) {
	if r.opt.Flows == nil {
		return FlowView{}, ErrNoFlows
	}
	state := callback.Get("state")
	if state == "" {
		return FlowView{}, &destination.ConnectError{Code: destination.ConnectStateInvalid}
	}
	f, err := r.opt.Flows.OAuthFlowByState(ctx, StateHash(state))
	if errors.Is(err, store.ErrFlowNotFound) || (err == nil && f.Type != typ) {
		return FlowView{}, &destination.ConnectError{Code: destination.ConnectStateInvalid}
	}
	if err != nil {
		return FlowView{}, err
	}
	if f.UsedAt != nil {
		return r.flowView(ctx, f)
	}
	now := r.now()
	fail := func(code string, cause error) (FlowView, error) {
		if cause != nil {
			r.log.Warn("connecting an account failed", "connect_flow", f.ID, "account_type", f.Type, "code", code,
				"err", cleanDetail(cause, nil))
		}
		if err := r.opt.Flows.FinishOAuthFlow(context.WithoutCancel(ctx), f.OwnerID, f.ID,
			store.FlowOutcome{Status: store.FlowFailed, ErrorCode: code}); err != nil && !errors.Is(err, store.ErrFlowClaimed) {
			return FlowView{}, err
		}
		g, err := r.opt.Flows.OAuthFlow(ctx, f.OwnerID, f.ID)
		if err != nil {
			return FlowView{}, err
		}
		return r.flowView(ctx, g)
	}
	if err := r.opt.Flows.ClaimOAuthFlow(ctx, f.ID, now); err != nil {
		if !errors.Is(err, store.ErrFlowClaimed) {
			return FlowView{}, err
		}
		g, gerr := r.opt.Flows.OAuthFlow(ctx, f.OwnerID, f.ID)
		if gerr != nil {
			return FlowView{}, gerr
		}
		if g.UsedAt == nil && g.Status == store.FlowPending { // not claimed: expired
			return fail(destination.ConnectExpired, nil)
		}
		return r.flowView(ctx, g)
	}
	// Claimed: what follows finishes even if the browser goes away.
	ctx = context.WithoutCancel(ctx)
	if sessionUser != "" && sessionUser != f.UserID {
		return fail(destination.ConnectSessionMismatch, nil)
	}
	if e := callback.Get("error"); e != "" {
		code := destination.ConnectExchangeFailed
		if e == "access_denied" {
			code = destination.ConnectDenied
		}
		return fail(code, fmt.Errorf("the network answered %q", redactWord(e)))
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return fail(destination.ConnectMasterKey, err)
	}
	var data flowData
	if err := r.openFlow(ctx, f, "data", &data); err != nil {
		return fail(destination.ConnectCode(err), err)
	}
	_, conn, a, err := connectorOf(f.Type, f.Method)
	if err != nil {
		return fail(destination.ConnectExchangeFailed, err)
	}
	req, err := r.connectRequest(f.OwnerID, f, a, data.Config)
	if err != nil {
		return fail(destination.ConnectExchangeFailed, err)
	}
	res, err := conn.CompleteConnect(ctx, destination.ConnectComplete{ConnectRequest: req, Data: data.Driver,
		Callback: callback, CodeVerifier: data.Verifier})
	if err != nil {
		return fail(destination.ConnectCode(err), err)
	}
	// The owner's fields, under what the network answered.
	withFields := func(c map[string]any) map[string]any {
		out := maps.Clone(data.Config)
		if out == nil {
			out = map[string]any{}
		}
		maps.Copy(out, c)
		return out
	}
	cands := []candidate{{Config: withFields(res.Config), Secrets: res.Secrets, Identity: res.Identity}}
	if len(res.Candidates) > 0 {
		cands = make([]candidate, 0, len(res.Candidates))
		for _, c := range res.Candidates {
			cands = append(cands, candidate{Config: withFields(c.Config), Secrets: c.Secrets, Identity: c.Identity})
		}
	}
	if len(cands) == 1 {
		if sessionUser != "" {
			return r.finishWith(ctx, f, a, cands[0], fail)
		}
		// Back in a browser that isn't signed in (a home-screen app, or
		// someone else's): the owner confirms the account, by who it is,
		// in the tab that started the flow.
		c, err := r.identify(ctx, f, a, cands[0])
		if err != nil {
			return fail(destination.ConnectExchangeFailed, err)
		}
		cands[0] = c
	}
	put, err := r.sealFlow(f, "candidates", cands)
	if err != nil {
		return fail(destination.ConnectExchangeFailed, err)
	}
	if err := r.opt.Flows.FinishOAuthFlow(ctx, f.OwnerID, f.ID, store.FlowOutcome{Status: store.FlowChoose,
		Put: []store.SealedSecret{put}}); err != nil {
		return FlowView{}, err
	}
	g, err := r.opt.Flows.OAuthFlow(ctx, f.OwnerID, f.ID)
	if err != nil {
		return FlowView{}, err
	}
	return r.flowView(ctx, g)
}

// identify asks the network who a candidate is (its handle, for the owner
// to confirm), with what the account would be opened with.
func (r *Registry) identify(ctx context.Context, f store.OAuthFlow, a destination.AuthMethod, c candidate) (candidate, error) {
	if c.Identity.Handle != "" {
		return c, nil
	}
	d, _ := destination.Lookup(f.Type)
	row := store.Account{ID: store.NewAccountID(), OwnerID: f.OwnerID, Type: f.Type, Source: store.SourceDB, AuthMethod: a.ID}
	ident, secrets, config, err := r.trial(ctx, d, row, c.Config, c.Secrets)
	if err != nil {
		return candidate{}, err
	}
	if ident.Handle == "" {
		ident.Handle = ident.RemoteID
	}
	return candidate{Config: config, Secrets: secrets, Identity: ident}, nil
}

// redactWord keeps an OAuth error code readable and harmless (it is the
// network's word, from the address bar).
func redactWord(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(c rune) rune {
		if c == '_' || c == '-' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return c
		}
		return '?'
	}, s)
}

// finishWith stores the account a flow connected (or reconnected) and
// records the outcome.
func (r *Registry) finishWith(ctx context.Context, f store.OAuthFlow, a destination.AuthMethod, c candidate,
	fail func(string, error) (FlowView, error)) (FlowView, error) {
	d, _ := destination.Lookup(f.Type)
	config := maps.Clone(c.Config)
	if config == nil {
		config = map[string]any{}
	}
	var en Entry
	var err error
	if f.AccountID == "" {
		label := f.Label
		if label == "" {
			label = d.Manifest().Name
		}
		// The connect routes take only a browser session.
		en, err = r.createChecked(ctx, NewAccount{Owner: f.OwnerID, Type: f.Type, Method: a.ID, Label: label,
			Actor: "session"}, d, a, config, c.Secrets)
	} else {
		var row store.Account
		row, err = r.opt.Writes.Account(ctx, f.OwnerID, f.AccountID)
		switch {
		case err != nil:
		case !row.DeletedAt.IsZero():
			err = store.ErrAccountNotFound
		case row.Source != store.SourceDB:
			err = store.ErrNotDBAccount
		case row.RemoteID != "" && c.Identity.RemoteID != "" && c.Identity.RemoteID != row.RemoteID:
			err = ErrIdentityChanged
		}
		if err == nil {
			// The account's own settings stay; state saved with the old
			// credentials (a token's expiry) starts afresh.
			var cur map[string]any
			if cur, err = rowConfig(row); err == nil {
				keep := map[string]any{}
				for _, fs := range d.Manifest().Config {
					if v, ok := cur[fs.Key]; ok {
						keep[fs.Key] = v
					}
				}
				maps.Copy(keep, config)
				fields := []string{"credentials"}
				if row.AuthMethod != a.ID { // an account added with a token now signs in
					fields = append(fields, "method")
				}
				en, err = r.replaceChecked(ctx, row, d, a, keep, c.Secrets, store.CredentialChange{
					Fields: fields, Actor: "session"})
			}
		}
	}
	var dup *store.DuplicateAccountError
	switch {
	case errors.As(err, &dup):
		return fail(destination.ConnectDuplicateAccount, nil)
	case errors.Is(err, ErrIdentityChanged):
		return fail(destination.ConnectIdentityChanged, nil)
	case err != nil:
		return fail(destination.ConnectExchangeFailed, err)
	}
	// The account is stored: the flow says so, even if recording that
	// fails twice (it then reads as interrupted, and the account is in
	// the list).
	done := store.FlowOutcome{Status: store.FlowCompleted, AccountID: en.Row.ID}
	err = r.opt.Flows.FinishOAuthFlow(ctx, f.OwnerID, f.ID, done)
	if err != nil && !errors.Is(err, store.ErrFlowClaimed) {
		err = r.opt.Flows.FinishOAuthFlow(ctx, f.OwnerID, f.ID, done)
	}
	if err != nil && !errors.Is(err, store.ErrFlowClaimed) {
		r.log.Error("connecting an account: its flow's outcome wasn't recorded", "connect_flow", f.ID, "account", en.Row.ID, "err", err)
		return FlowView{ID: f.ID, Status: store.FlowCompleted, AccountID: en.Row.ID, Type: f.Type, ExpiresAt: f.ExpiresAt}, nil
	}
	g, err := r.opt.Flows.OAuthFlow(ctx, f.OwnerID, f.ID)
	if err != nil {
		return FlowView{}, err
	}
	return r.flowView(ctx, g)
}

// Flow reads one of the owner's flows. A flow started by another user is
// not found.
func (r *Registry) Flow(ctx context.Context, owner, user, id string) (FlowView, error) {
	if r.opt.Flows == nil {
		return FlowView{}, ErrNoFlows
	}
	f, err := r.opt.Flows.OAuthFlow(ctx, owner, id)
	if err != nil {
		return FlowView{}, err
	}
	if f.UserID != user {
		return FlowView{}, store.ErrFlowNotFound
	}
	return r.flowView(ctx, f)
}

func (r *Registry) flowView(ctx context.Context, f store.OAuthFlow) (FlowView, error) {
	v := FlowView{ID: f.ID, Status: f.Status, AccountID: f.ResultAccountID, Error: f.ErrorCode, Type: f.Type, ExpiresAt: f.ExpiresAt}
	switch {
	case f.Status == store.FlowPending && f.UsedAt == nil && !r.now().Before(f.ExpiresAt):
		v.Status, v.Error = store.FlowFailed, destination.ConnectExpired
	case f.Status == store.FlowPending && f.UsedAt != nil && !r.now().Before(f.UsedAt.Add(claimTimeout)):
		v.Status, v.Error = store.FlowFailed, destination.ConnectInterrupted
	}
	if f.Status == store.FlowChoose {
		var cands []candidate
		if err := r.openFlow(ctx, f, "candidates", &cands); err != nil {
			v.Status, v.Error = store.FlowFailed, destination.ConnectCode(err)
			return v, nil
		}
		for _, c := range cands {
			v.Candidates = append(v.Candidates, c.Identity)
		}
	}
	return v, nil
}

// ErrNoSuchCandidate: the choice isn't one the network offered.
var ErrNoSuchCandidate = errors.New("no such candidate")

// Choose finishes a flow waiting for the owner's choice with the n-th
// candidate.
func (r *Registry) Choose(ctx context.Context, owner, user, id string, n int) (FlowView, error) {
	if r.opt.Flows == nil {
		return FlowView{}, ErrNoFlows
	}
	// One choice at a time, read again under the lock: two choices racing
	// would otherwise both store an account.
	r.chooseMu.Lock()
	defer r.chooseMu.Unlock()
	f, err := r.opt.Flows.OAuthFlow(ctx, owner, id)
	if err != nil {
		return FlowView{}, err
	}
	if f.UserID != user {
		return FlowView{}, store.ErrFlowNotFound
	}
	if f.Status != store.FlowChoose {
		return r.flowView(ctx, f)
	}
	var cands []candidate
	if err := r.openFlow(ctx, f, "candidates", &cands); err != nil {
		return FlowView{}, err
	}
	if n < 0 || n >= len(cands) {
		return FlowView{}, ErrNoSuchCandidate
	}
	_, _, a, err := connectorOf(f.Type, f.Method)
	if err != nil {
		return FlowView{}, err
	}
	ctx = context.WithoutCancel(ctx)
	fail := func(code string, cause error) (FlowView, error) {
		if cause != nil {
			r.log.Warn("connecting an account failed", "connect_flow", f.ID, "account_type", f.Type, "code", code,
				"err", cleanDetail(cause, nil))
		}
		if err := r.opt.Flows.FinishOAuthFlow(ctx, f.OwnerID, f.ID,
			store.FlowOutcome{Status: store.FlowFailed, ErrorCode: code}); err != nil && !errors.Is(err, store.ErrFlowClaimed) {
			return FlowView{}, err
		}
		g, err := r.opt.Flows.OAuthFlow(ctx, f.OwnerID, f.ID)
		if err != nil {
			return FlowView{}, err
		}
		return r.flowView(ctx, g)
	}
	return r.finishWith(ctx, f, a, cands[n], fail)
}

// PurgeFlows deletes the flows that expired more than an hour ago.
func (r *Registry) PurgeFlows(ctx context.Context) (int, error) {
	if r.opt.Flows == nil {
		return 0, nil
	}
	return r.opt.Flows.PurgeOAuthFlows(ctx, r.now().Add(-flowKept))
}

// oauthClients keeps a type's OAuth clients for one redirect URI, their
// secrets sealed for their server.
type oauthClients struct {
	r        *Registry
	owner    string
	typ      string
	redirect string
}

func clientBinding(owner, id, issuer string) secrets.Binding {
	return secrets.Binding{Owner: owner, Scope: secrets.ScopeOAuthClient, Ref: id, Name: "client_secret", Audience: issuer}
}

func (c *oauthClients) Client(ctx context.Context, issuer string) (destination.OAuthClient, bool, error) {
	row, err := c.r.opt.Flows.OAuthClientFor(ctx, c.owner, c.typ, issuer, c.redirect)
	if errors.Is(err, store.ErrOAuthClientNotFound) {
		return destination.OAuthClient{}, false, nil
	}
	if err != nil {
		return destination.OAuthClient{}, false, &localError{err}
	}
	out := destination.OAuthClient{Issuer: row.Issuer, ClientID: row.ClientID, Scopes: row.Scopes}
	ss, err := c.r.opt.Flows.Secrets(ctx, c.owner, secrets.ScopeOAuthClient, row.ID)
	if err != nil {
		return destination.OAuthClient{}, false, &localError{err}
	}
	for _, s := range ss {
		if s.Name != "client_secret" {
			continue
		}
		plain, err := c.r.opt.Keys.Open(clientBinding(c.owner, row.ID, issuer), s.Sealed)
		if err != nil {
			// Sealed under another key: register again.
			return destination.OAuthClient{}, false, nil
		}
		out.ClientSecret = plain
	}
	return out, true, nil
}

func (c *oauthClients) SaveClient(ctx context.Context, cl destination.OAuthClient) error {
	row := store.OAuthClient{ID: store.NewOAuthClientID(), OwnerID: c.owner, Type: c.typ, Issuer: cl.Issuer,
		RedirectURI: c.redirect, ClientID: cl.ClientID, Scopes: cl.Scopes}
	var put []store.SealedSecret
	if cl.ClientSecret != "" {
		b := clientBinding(c.owner, row.ID, cl.Issuer)
		sealed, err := c.r.opt.Keys.Seal(b, cl.ClientSecret)
		if err != nil {
			return err
		}
		kid, err := secrets.KeyID(sealed)
		if err != nil {
			return err
		}
		put = append(put, store.SealedSecret{Owner: b.Owner, Scope: b.Scope, Ref: b.Ref, Name: b.Name, Audience: b.Audience, Sealed: sealed, KeyID: kid})
	}
	if err := c.r.opt.Flows.SaveOAuthClient(ctx, row, put); err != nil {
		return &localError{err}
	}
	return nil
}

func (c *oauthClients) ForgetClient(ctx context.Context, issuer, clientID string) error {
	row, err := c.r.opt.Flows.OAuthClientFor(ctx, c.owner, c.typ, issuer, c.redirect)
	if errors.Is(err, store.ErrOAuthClientNotFound) || (err == nil && row.ClientID != clientID) {
		return nil // gone, or a newer app replaced it
	}
	if err != nil {
		return &localError{err}
	}
	if err := c.r.opt.Flows.DeleteOAuthClient(ctx, c.owner, row.ID); err != nil {
		return &localError{err}
	}
	return nil
}

// localError is Publisher's own store failing during a flow, not the
// network's answer.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }
