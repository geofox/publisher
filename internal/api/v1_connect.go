package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/store"
)

// Connecting an account by signing in at its network (spec §6.1): the
// owner starts a flow (POST /v1/accounts:connect, or :reconnect on an
// account), signs in at the network in a new tab, and the network sends
// them back to /oauth/callback/<type>. The tab that started the flow reads
// it (GET /v1/connect-flows/{id}) until it ends, and picks the account
// when the network offered several (:choose).

// AccountConnector is the registry's side of the connect flows
// (accounts.Registry).
type AccountConnector interface {
	BeginConnect(ctx context.Context, q accounts.ConnectStart) (accounts.StartedFlow, error)
	CompleteConnect(ctx context.Context, typ string, callback url.Values, sessionUser string) (accounts.FlowView, error)
	Flow(ctx context.Context, owner, user, id string) (accounts.FlowView, error)
	Choose(ctx context.Context, owner, user, id string, n int) (accounts.FlowView, error)
}

// connectStartsPerMinute: each start may register a client at the
// network and holds a flow for 10 minutes.
const connectStartsPerMinute = 10

// connectText says what each connect error code means, in fixed words:
// the network's own text is never shown (spec §6.1).
var connectText = map[string]string{
	destination.ConnectDenied:           "the sign-in was declined at the network",
	destination.ConnectExpired:          "the sign-in took longer than 10 minutes; start again",
	destination.ConnectStateInvalid:     "this sign-in link isn't one Publisher is waiting for (already used, or from another server)",
	destination.ConnectSessionMismatch:  "this browser is signed in to Publisher as someone other than who started the sign-in",
	destination.ConnectIdentityChanged:  "the network signed in another account than the one being reconnected",
	destination.ConnectDuplicateAccount: "this account is already in Publisher",
	destination.ConnectExchangeFailed:   "the network didn't complete the sign-in",
	destination.ConnectIssuerMismatch:   "the server's sign-in isn't on the server's own address",
	destination.ConnectMasterKey:        "the server's master key (PUBLISHER_MASTER_KEY) is needed and isn't usable",
}

func connectDetail(code string) string {
	if t, ok := connectText[code]; ok {
		return t
	}
	return connectText[destination.ConnectExchangeFailed]
}

// accountConnector is the registry's connect side, or it answers 503.
func (a *API) accountConnector(w http.ResponseWriter, r *http.Request) (AccountConnector, bool) {
	if c, ok := a.Accounts.(AccountConnector); ok {
		return c, true
	}
	problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "this server can't connect accounts by signing in at their network")
	return nil, false
}

// v1ConnectIn is POST /v1/accounts:connect's body.
type v1ConnectIn struct {
	Type string `json:"type"`
	// Method is the sign-in method (GET /v1/destinations lists them); it
	// may be left out when the type has one.
	Method string `json:"method"`
	Label  string `json:"label"`
	// Config holds the type's settings and the method's fields (a
	// Mastodon server's address).
	Config map[string]any `json:"config"`
}

// v1ReconnectIn is POST /v1/accounts/{id}:reconnect's body (optional).
type v1ReconnectIn struct {
	Method string         `json:"method"`
	Config map[string]any `json:"config"`
}

// v1ConnectStartBody is a flow just started.
type v1ConnectStartBody struct {
	Flow         string    `json:"flow"`
	AuthorizeURL string    `json:"authorize_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// v1ConnectFlowBody is a flow as its owner reads it.
type v1ConnectFlowBody struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	// Account is the account connected (completed).
	Account string `json:"account,omitempty"`
	// Error is the failure's code and ErrorDetail what it means.
	Error       string    `json:"error,omitempty"`
	ErrorDetail string    `json:"error_detail,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Candidates are the accounts to pick from (choose), by index.
	Candidates []v1ConnectCandidate `json:"candidates,omitempty"`
}

// v1ConnectCandidate is one account a network offered.
type v1ConnectCandidate struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

func flowBody(v accounts.FlowView) v1ConnectFlowBody {
	b := v1ConnectFlowBody{ID: v.ID, Type: v.Type, Status: v.Status, Account: v.AccountID, Error: v.Error, ExpiresAt: v.ExpiresAt.UTC()}
	if v.Error != "" {
		b.ErrorDetail = connectDetail(v.Error)
	}
	for _, c := range v.Candidates {
		b.Candidates = append(b.Candidates, v1ConnectCandidate{Handle: c.Handle, DisplayName: c.DisplayName, AvatarURL: c.AvatarURL})
	}
	return b
}

// v1ConnectAccount starts connecting a new account (spec §6.1): 201 with
// the address where the owner signs in.
func (a *API) v1ConnectAccount(w http.ResponseWriter, r *http.Request) {
	var in v1ConnectIn
	if !decodeJSON(w, r, &in) {
		return
	}
	var errs []fieldError
	if strings.TrimSpace(in.Type) == "" {
		errs = append(errs, fieldError{Pointer: "/type", Code: "required", Detail: "the network: GET /v1/destinations lists them"})
	}
	label, fe := checkLabel(in.Label, true)
	if fe != nil {
		errs = append(errs, *fe)
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	a.startConnect(w, r, accounts.ConnectStart{Type: strings.TrimSpace(in.Type), Method: in.Method, Label: label, Config: in.Config})
}

// v1ReconnectAccount starts signing an account added in the app in
// again (its credentials expired, or were revoked): the same network
// account must come back.
func (a *API) v1ReconnectAccount(w http.ResponseWriter, r *http.Request) {
	var in v1ReconnectIn
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	cur, ok := a.v1LiveAccount(w, r)
	if !ok {
		return
	}
	a.startConnect(w, r, accounts.ConnectStart{Reconnect: cur.ID, Method: in.Method, Config: in.Config})
}

func (a *API) startConnect(w http.ResponseWriter, r *http.Request, q accounts.ConnectStart) {
	conn, ok := a.accountConnector(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	if wait := a.limits().connects.Take(p.OwnerID); wait > 0 {
		rateLimited(w, r, wait, "at most 10 sign-ins at a network started a minute; try again shortly")
		return
	}
	q.Owner, q.User = p.OwnerID, p.ID
	q.Actor, q.TokenID = actorOf(p)
	// Never from the request: the network sends the owner back to the
	// address the operator set.
	q.RedirectBase = a.AppBaseURL
	f, err := conn.BeginConnect(r.Context(), q)
	if err != nil {
		a.connectError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/connect-flows/"+f.ID)
	writeV1(w, http.StatusCreated, v1ConnectStartBody{Flow: f.ID, AuthorizeURL: f.AuthorizeURL, ExpiresAt: f.ExpiresAt.UTC()})
}

// connectError answers a flow that couldn't start, or a choice that
// couldn't finish.
func (a *API) connectError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *destination.ConnectError
	switch {
	case errors.Is(err, accounts.ErrNoRedirectBase):
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
	case errors.Is(err, accounts.ErrNoFlows):
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, err.Error())
	case errors.Is(err, store.ErrFlowNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no connect flow "+r.PathValue("id"))
	case errors.Is(err, accounts.ErrNoSuchCandidate):
		invalid(w, r, []fieldError{{Pointer: "/candidate", Code: "invalid", Detail: "the index of one of the flow's candidates"}})
	case errors.As(err, &ce):
		writeProblem(w, r, problemBody{Status: http.StatusBadGateway, Code: codeConnectFailed, ConnectError: ce.Code,
			Detail: connectDetail(ce.Code)})
	default:
		a.accountWriteError(w, r, "starting the sign-in", err)
	}
}

// v1ConnectFlow reads one of the caller's flows.
func (a *API) v1ConnectFlow(w http.ResponseWriter, r *http.Request) {
	conn, ok := a.accountConnector(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	v, err := conn.Flow(r.Context(), p.OwnerID, p.ID, r.PathValue("id"))
	if err != nil {
		a.flowError(w, r, err)
		return
	}
	writeV1(w, http.StatusOK, flowBody(v))
}

// flowError answers a flow that can't be read.
func (a *API) flowError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrFlowNotFound):
		problem(w, r, http.StatusNotFound, codeNotFound, "no connect flow "+r.PathValue("id"))
	case errors.Is(err, accounts.ErrNoFlows):
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, err.Error())
	default:
		slog.ErrorContext(r.Context(), "v1: reading a connect flow failed", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the connect flow failed")
	}
}

// v1ConnectFlowAction serves POST /v1/connect-flows/{id}:choose.
func (a *API) v1ConnectFlowAction(w http.ResponseWriter, r *http.Request) {
	id, action, found := strings.Cut(r.PathValue("id"), ":")
	if !found {
		w.Header().Set("Allow", "GET, HEAD")
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST is not allowed here; use GET, HEAD, or POST to :choose")
		return
	}
	r.SetPathValue("id", id)
	if action != "choose" {
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; connect flows take :choose")
		return
	}
	var in struct {
		Candidate *int `json:"candidate"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Candidate == nil {
		invalid(w, r, []fieldError{{Pointer: "/candidate", Code: "required", Detail: "the index of one of the flow's candidates"}})
		return
	}
	conn, ok := a.accountConnector(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	v, err := conn.Choose(r.Context(), p.OwnerID, p.ID, id, *in.Candidate)
	if err != nil {
		a.connectError(w, r, err)
		return
	}
	writeV1(w, http.StatusOK, flowBody(v))
}

// The callback's page: fixed words, no script, nothing from the network.
const callbackCSS = "body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#222;background:#fff}" +
	"@media (prefers-color-scheme:dark){body{color:#eee;background:#111}a{color:#8cf}}h1{font-size:1.4rem}code{font-size:.9em}"

var (
	callbackCSP = func() string {
		sum := sha256.Sum256([]byte(callbackCSS))
		return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
			"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	}()
	callbackPage = template.Must(template.New("cb").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><title>{{.Title}} · Publisher</title><style>{{.CSS}}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Text}}</p>{{if .Code}}<p><small>Code: <code>{{.Code}}</code></small></p>{{end}}
<p><a href="/">Open Publisher</a></p></body></html>
`))
)

type callbackView struct {
	Title, Text, Code string
	CSS               template.CSS
}

// handleConnectCallback is where a network sends the owner back (GET
// /oauth/callback/{type}?code&state…). It needs no session: the state is
// the flow's capability, and a home-screen app may come back in another
// browser. A session that came with it must be the flow's user's.
func (a *API) handleConnectCallback(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", callbackCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Type", "text/html; charset=utf-8")
	page := func(status int, v callbackView) {
		v.CSS = template.CSS(callbackCSS)
		w.WriteHeader(status)
		_ = callbackPage.Execute(w, v)
	}
	conn, ok := a.Accounts.(AccountConnector)
	if !ok {
		page(http.StatusNotFound, callbackView{Title: "Not connected", Text: "This server can't connect accounts by signing in at their network."})
		return
	}
	user := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		if _, u, ok := a.sessionUser(c.Value); ok {
			user = u.ID
		}
	}
	v, err := conn.CompleteConnect(r.Context(), r.PathValue("type"), r.URL.Query(), user)
	var ce *destination.ConnectError
	switch {
	case errors.As(err, &ce):
		page(http.StatusBadRequest, callbackView{Title: "Not connected", Text: upperFirst(connectDetail(ce.Code)) + ".", Code: ce.Code})
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "connect callback failed", "account_type", r.PathValue("type"), "err", err)
		page(http.StatusInternalServerError, callbackView{Title: "Not connected", Text: "Something went wrong on this server; start again from Publisher."})
		return
	}
	switch v.Status {
	case store.FlowCompleted:
		page(http.StatusOK, callbackView{Title: "Connected", Text: "The account is connected. You can close this tab and go back to Publisher."})
	case store.FlowChoose:
		page(http.StatusOK, callbackView{Title: "Almost there", Text: "The network offered more than one account: pick one in Publisher, in the tab you started from."})
	case store.FlowFailed:
		page(http.StatusBadRequest, callbackView{Title: "Not connected", Text: upperFirst(connectDetail(v.Error)) + ".", Code: v.Error})
	default:
		page(http.StatusOK, callbackView{Title: "Still connecting", Text: "Go back to Publisher, in the tab you started from."})
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
