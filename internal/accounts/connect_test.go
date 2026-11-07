package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// connectDriver is a type whose accounts connect by signing in at the
// network. Its "network" turns the callback's code into a token:
//
//   - "c-<x>": the token <x> (the account follows from it, as credAcct's do);
//   - "many": two accounts to choose from (tokens "pick-a", "pick-b");
//   - "fail": the exchange fails, the network quoting a secret;
//   - "blocked": the server is on a private address.
type connectDriver struct {
	mu     sync.Mutex
	begins []destination.ConnectRequest
	done   []destination.ConnectComplete
}

const connectType = "connecttest"

func (d *connectDriver) Manifest() destination.Manifest {
	return destination.Manifest{Contract: destination.ContractVersion, Type: connectType, Name: "Connect Test",
		Kinds: []destination.Kind{destination.KindNote},
		Auth: []destination.AuthMethod{{ID: "oauth", Flow: destination.FlowRedirect, Label: "Sign in", NeedsPublicURL: true,
			Fields: []destination.FieldSpec{{Key: "server", Label: "Server", Type: destination.FieldString, Required: true, Audience: true}}}},
	}
}

func (d *connectDriver) Open(context.Context, destination.OpenRequest) (destination.Account, error) {
	return nil, errors.New("opened through the test opener")
}

func (d *connectDriver) BeginConnect(_ context.Context, req destination.ConnectRequest) (destination.ConnectStart, error) {
	d.mu.Lock()
	d.begins = append(d.begins, req)
	d.mu.Unlock()
	if req.Config["server"] == "blocked" {
		return destination.ConnectStart{}, destination.ErrBlockedAddress
	}
	q := url.Values{"state": {req.State}, "code_challenge": {req.CodeChallenge}, "redirect_uri": {req.RedirectURI}}
	data, _ := json.Marshal(map[string]string{"server": req.Config["server"].(string), "challenge": req.CodeChallenge})
	return destination.ConnectStart{AuthorizeURL: "https://net.example/authorize?" + q.Encode(), Data: data}, nil
}

func (d *connectDriver) CompleteConnect(_ context.Context, req destination.ConnectComplete) (destination.ConnectResult, error) {
	d.mu.Lock()
	d.done = append(d.done, req)
	d.mu.Unlock()
	var data map[string]string
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return destination.ConnectResult{}, err
	}
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != data["challenge"] {
		return destination.ConnectResult{}, errors.New("PKCE: the verifier isn't the challenge's")
	}
	cfg := map[string]any{"server": data["server"]}
	switch code := req.Callback.Get("code"); {
	case strings.HasPrefix(code, "c-"):
		return destination.ConnectResult{Config: cfg, Secrets: map[string]string{"token": strings.TrimPrefix(code, "c-")}}, nil
	case code == "many":
		return destination.ConnectResult{Candidates: []destination.ConnectResult{
			{Config: cfg, Secrets: map[string]string{"token": "pick-a"}, Identity: destination.Identity{RemoteID: "remote-pick-a", Handle: "a"}},
			{Config: cfg, Secrets: map[string]string{"token": "pick-b"}, Identity: destination.Identity{RemoteID: "remote-pick-b", Handle: "b"}},
		}}, nil
	}
	return destination.ConnectResult{}, &destination.ConnectError{Code: destination.ConnectExchangeFailed,
		Err: errors.New("the network said: bad code, client_secret=s3cret")}
}

var registerConnect sync.Once
var connectDrv = &connectDriver{}

func newConnectEnv(t *testing.T, noKey bool) *writeEnv {
	t.Helper()
	registerConnect.Do(func() { destination.Register(connectDrv) })
	return newWriteEnv(t, noKey)
}

func startConnect(t *testing.T, w *writeEnv, q ConnectStart) (StartedFlow, url.Values) {
	t.Helper()
	if q.Owner == "" {
		q.Owner = store.DefaultOwner
	}
	if q.User == "" {
		q.User = "user-1"
	}
	if q.Type == "" && q.Reconnect == "" {
		q.Type = connectType
	}
	if q.RedirectBase == "" {
		q.RedirectBase = "https://pub.example/"
	}
	f, err := w.r.BeginConnect(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(f.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	return f, u.Query()
}

func callback(state, code string) url.Values {
	return url.Values{"state": {state}, "code": {code}}
}

// A flow connects a new account: the state is random and only its hash
// is stored, the challenge is S256 of a verifier kept sealed, the
// redirect URI is the public one, and the account is stored as one with
// typed credentials is. A second callback (a reload) shows the outcome
// without asking the network again.
func TestConnectNewAccount(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	f, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}, Label: "Mine"})
	state := q.Get("state")
	if len(state) != 43 || q.Get("code_challenge") == "" || q.Get("redirect_uri") != "https://pub.example/oauth/callback/"+connectType {
		t.Fatalf("authorize query = %v", q)
	}
	if f.ExpiresAt.Sub(time.Now()) > FlowTTL || f.ExpiresAt.Sub(time.Now()) < FlowTTL-time.Minute {
		t.Fatalf("expires at %v", f.ExpiresAt)
	}
	// Nothing holds the state or the verifier in the clear.
	raw, _ := w.st.Secrets(ctx, store.DefaultOwner, store.SecretScopeOAuthFlow, f.ID)
	if len(raw) != 1 || strings.Contains(raw[0].Sealed, state) {
		t.Fatalf("flow data: %+v", raw)
	}
	if v, err := w.r.Flow(ctx, store.DefaultOwner, "user-1", f.ID); err != nil || v.Status != store.FlowPending {
		t.Fatalf("pending: %+v %v", v, err)
	}
	// Another user doesn't see it.
	if _, err := w.r.Flow(ctx, store.DefaultOwner, "user-2", f.ID); !errors.Is(err, store.ErrFlowNotFound) {
		t.Fatalf("another user: %v", err)
	}

	v, err := w.r.CompleteConnect(ctx, connectType, callback(state, "c-tok1"), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != store.FlowCompleted || v.AccountID == "" || v.Error != "" {
		t.Fatalf("completed: %+v", v)
	}
	en, ok := w.r.Get(v.AccountID)
	if !ok || en.Row.Label != "Mine" || en.Row.AuthMethod != "oauth" || en.Row.RemoteID != "remote-tok1" || en.Status != Connected {
		t.Fatalf("account: %+v %v", en, ok)
	}
	if got := w.secretsOf(v.AccountID, "https://net.example"); got["token"] != "tok1" {
		t.Fatalf("secrets: %v", got)
	}
	if raw, _ := w.st.Secrets(ctx, store.DefaultOwner, store.SecretScopeOAuthFlow, f.ID); len(raw) != 0 {
		t.Fatal("the flow's data outlived it")
	}
	n := len(connectDrv.done)
	again, err := w.r.CompleteConnect(ctx, connectType, callback(state, "c-tok1"), "user-1")
	if err != nil || again.Status != store.FlowCompleted || again.AccountID != v.AccountID || len(connectDrv.done) != n {
		t.Fatalf("second callback: %+v %v (exchanges %d → %d)", again, err, n, len(connectDrv.done))
	}
	// The same account again is a duplicate.
	_, q2 := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	dup, err := w.r.CompleteConnect(ctx, connectType, callback(q2.Get("state"), "c-tok1"), "user-1")
	if err != nil || dup.Status != store.FlowFailed || dup.Error != destination.ConnectDuplicateAccount {
		t.Fatalf("duplicate: %+v %v", dup, err)
	}
}

// What a callback can't use: an unknown state (or one for another type),
// an expired flow, another user's session, a refusal at the network, a
// failed exchange (logged without what the network quoted), and a flow
// whose data can't be opened any more.
func TestConnectRefusals(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	start := func() url.Values {
		_, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
		return q
	}
	code := func(v FlowView, err error) string {
		t.Helper()
		if err != nil {
			return destination.ConnectCode(err)
		}
		if v.Status != store.FlowFailed {
			t.Fatalf("not failed: %+v", v)
		}
		return v.Error
	}
	if c := code(w.r.CompleteConnect(ctx, connectType, callback("nope", "c-x"), "user-1")); c != destination.ConnectStateInvalid {
		t.Errorf("unknown state: %s", c)
	}
	q := start()
	if c := code(w.r.CompleteConnect(ctx, "other", callback(q.Get("state"), "c-x"), "")); c != destination.ConnectStateInvalid {
		t.Errorf("another type's callback: %s", c)
	}
	if c := code(w.r.CompleteConnect(ctx, connectType, url.Values{"code": {"c-x"}}, "")); c != destination.ConnectStateInvalid {
		t.Errorf("no state: %s", c)
	}
	if c := code(w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-x"), "user-2")); c != destination.ConnectSessionMismatch {
		t.Errorf("another user's session: %s", c)
	}
	// Claimed by that callback: the owner's own comes too late.
	if c := code(w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-x"), "user-1")); c != destination.ConnectSessionMismatch {
		t.Errorf("after the mismatch: %s", c)
	}
	q = start()
	denied := url.Values{"state": {q.Get("state")}, "error": {"access_denied"}, "error_description": {"no"}}
	if c := code(w.r.CompleteConnect(ctx, connectType, denied, "user-1")); c != destination.ConnectDenied {
		t.Errorf("denied: %s", c)
	}
	q = start()
	if c := code(w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "fail"), "")); c != destination.ConnectExchangeFailed {
		t.Errorf("failed exchange: %s", c)
	}
	// Expired.
	q = start()
	w.r.opt.Now = func() time.Time { return time.Now().Add(FlowTTL + time.Second) }
	if c := code(w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-x"), "")); c != destination.ConnectExpired {
		t.Errorf("expired: %s", c)
	}
	w.r.opt.Now = time.Now
	// The master key changed mid-flow: its data can't be opened.
	q = start()
	w.r.opt.Keys.Current = testKey(t, 9)
	if c := code(w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-x"), "")); c != destination.ConnectMasterKey {
		t.Errorf("master key changed: %s", c)
	}
	if w.count() != 0 {
		t.Fatalf("%d accounts stored", w.count())
	}
}

// A network that offers several accounts waits for the owner's choice;
// the choice is stored as any connected account.
func TestConnectChoose(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	f, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "many"), "")
	if err != nil || v.Status != store.FlowChoose || len(v.Candidates) != 2 || v.Candidates[1].Handle != "b" {
		t.Fatalf("choose: %+v %v", v, err)
	}
	if _, err := w.r.Choose(ctx, store.DefaultOwner, "user-2", f.ID, 1); !errors.Is(err, store.ErrFlowNotFound) {
		t.Fatalf("another user chose: %v", err)
	}
	if _, err := w.r.Choose(ctx, store.DefaultOwner, "user-1", f.ID, 2); !errors.Is(err, ErrNoSuchCandidate) {
		t.Fatalf("out of range: %v", err)
	}
	v, err = w.r.Choose(ctx, store.DefaultOwner, "user-1", f.ID, 1)
	if err != nil || v.Status != store.FlowCompleted {
		t.Fatalf("chosen: %+v %v", v, err)
	}
	if en, _ := w.r.Get(v.AccountID); en.Row.RemoteID != "remote-pick-b" {
		t.Fatalf("account: %+v", en.Row)
	}
}

// A reconnect gives an app-added account new credentials, only for the
// same network account; it keeps the account's settings.
func TestConnectReconnect(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	_, q := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	v, err := w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-same1"), "user-1")
	if err != nil || v.Status != store.FlowCompleted {
		t.Fatalf("connect: %+v %v", v, err)
	}
	id := v.AccountID
	_, q = startConnect(t, w, ConnectStart{Reconnect: id})
	if last := connectDrv.begins[len(connectDrv.begins)-1]; last.Config["server"] != "https://net.example" {
		t.Fatalf("reconnect began with %v", last.Config)
	}
	v, err = w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-same2"), "user-1")
	if err != nil || v.Status != store.FlowCompleted || v.AccountID != id {
		t.Fatalf("reconnect: %+v %v", v, err)
	}
	if got := w.secretsOf(id, "https://net.example"); got["token"] != "same2" {
		t.Fatalf("secrets after reconnect: %v", got)
	}
	_, q = startConnect(t, w, ConnectStart{Reconnect: id})
	v, err = w.r.CompleteConnect(ctx, connectType, callback(q.Get("state"), "c-other"), "user-1")
	if err != nil || v.Error != destination.ConnectIdentityChanged {
		t.Fatalf("another account: %+v %v", v, err)
	}
	if got := w.secretsOf(id, "https://net.example"); got["token"] != "same2" {
		t.Fatalf("secrets changed by a refused reconnect: %v", got)
	}
	if w.count() != 1 {
		t.Fatalf("%d accounts", w.count())
	}
}

// Starting needs the public address, a master key, the method's fields,
// and a reachable server.
func TestConnectStartRefusals(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	q := ConnectStart{Owner: store.DefaultOwner, User: "user-1", Type: connectType, Config: map[string]any{"server": "https://net.example"}}
	if _, err := w.r.BeginConnect(ctx, q); !errors.Is(err, ErrNoRedirectBase) {
		t.Errorf("no public address: %v", err)
	}
	q.RedirectBase = "https://pub.example"
	var ie *InvalidError
	bad := q
	bad.Config = map[string]any{}
	if _, err := w.r.BeginConnect(ctx, bad); !errors.As(err, &ie) || ie.Problems[0].Pointer != "/config/server" {
		t.Errorf("no server: %v", err)
	}
	bad.Config = map[string]any{"server": "x", "extra": 1}
	if _, err := w.r.BeginConnect(ctx, bad); !errors.As(err, &ie) {
		t.Errorf("an unknown field: %v", err)
	}
	bad = q
	bad.Type = "bluesky"
	if _, err := w.r.BeginConnect(ctx, bad); !errors.As(err, &ie) || ie.Problems[0].Pointer != "/type" {
		t.Errorf("a type without sign-in: %v", err)
	}
	bad = q
	bad.Method = "token"
	if _, err := w.r.BeginConnect(ctx, bad); !errors.As(err, &ie) || ie.Problems[0].Pointer != "/method" {
		t.Errorf("another method: %v", err)
	}
	bad = q
	bad.Config = map[string]any{"server": "blocked"}
	if _, err := w.r.BeginConnect(ctx, bad); !errors.As(err, &ie) || !strings.Contains(ie.Error(), "PRIVATE_DESTINATIONS") {
		t.Errorf("a private server: %v", err)
	}
	nokey := newConnectEnv(t, true)
	if _, err := nokey.r.BeginConnect(ctx, q); err == nil {
		t.Error("started without a master key")
	}
}

// Expired flows are purged an hour later, with their data.
func TestConnectPurge(t *testing.T) {
	w := newConnectEnv(t, false)
	ctx := context.Background()
	f, _ := startConnect(t, w, ConnectStart{Config: map[string]any{"server": "https://net.example"}})
	if n, _ := w.r.PurgeFlows(ctx); n != 0 {
		t.Fatalf("purged a live flow")
	}
	w.r.opt.Now = func() time.Time { return time.Now().Add(FlowTTL + flowKept + time.Minute) }
	if n, err := w.r.PurgeFlows(ctx); n != 1 || err != nil {
		t.Fatalf("purge: %d %v", n, err)
	}
	if raw, _ := w.st.Secrets(ctx, store.DefaultOwner, store.SecretScopeOAuthFlow, f.ID); len(raw) != 0 {
		t.Fatal("data kept")
	}
}
