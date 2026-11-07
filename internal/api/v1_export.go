package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/export"
	"github.com/geofox/publisher/internal/localauth"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/store"
)

// Export and import (spec §9): the owner's accounts and settings, with
// their credentials, in a file sealed with a passphrase the server makes.
// Both need the owner signed in and re-authenticated; neither answer is
// ever stored (idempotency) or logged with what it holds.

const (
	opExport = "export"
	opImport = "import"
	// importTTL is how long a read file waits for its confirmation.
	importTTL = 10 * time.Minute
	// exportsPerMinute bounds exports and files read, each a 64 MiB KDF.
	exportsPerMinute = 5
	// importWorkers open and identify imported accounts at once.
	importWorkers = 8
	// importBudget is how long a confirmed import's answer may take: a
	// file's 100 accounts, 8 at once, each opened within 15 seconds.
	importBudget = 4 * time.Minute
)

// AccountPorter is the registry's export side (accounts.Registry).
type AccountPorter interface {
	ExportAccounts(ctx context.Context, owner string) ([]export.Account, error)
	ImportAccount(ctx context.Context, owner string, x export.Account, actor string) (accounts.Imported, error)
	CheckImport(owner string, x export.Account) (why, here string)
	EnvMatch(owner string, x export.Account) (store.Account, bool)
}

func (a *API) accountPorter(w http.ResponseWriter, r *http.Request) (AccountPorter, bool) {
	if p, ok := a.Accounts.(AccountPorter); ok && a.Store != nil {
		return p, true
	}
	problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "this server can't export or import accounts")
	return nil, false
}

// porting holds what export and import share: the hashing slots when
// the owner signs in with OIDC (in local mode, the password hasher's), and
// the files read, waiting for their confirmation.
type porting struct {
	once    sync.Once
	hasher  *localauth.Hasher
	mu      sync.Mutex
	pending map[string]*pendingImport
	// running is held by the import being confirmed: one at a time, so
	// two files are never merged.
	running sync.Mutex
}

type pendingImport struct {
	owner, session string
	contents       export.Contents
	header         export.Header
	expires        time.Time
}

func (a *API) port() *porting {
	a.porting.once.Do(func() {
		a.porting.hasher = localauth.NewHasher()
		a.porting.pending = map[string]*pendingImport{}
	})
	return &a.porting
}

// withKDF runs fn, which derives an export file's key (64 MiB), where
// password hashes run: false when they are all taken (the caller answers
// 429).
func (a *API) withKDF(fn func()) bool {
	h := a.port().hasher
	if a.Local != nil && a.Local.Hasher != nil {
		h = a.Local.Hasher
	}
	return h.Do(fn) == nil
}

// v1ExportBody is POST /v1/export's answer: the passphrase, shown this
// once, and the file.
type v1ExportBody struct {
	Passphrase  string    `json:"passphrase"`
	Filename    string    `json:"filename"`
	File        string    `json:"file"`
	CreatedAt   time.Time `json:"created_at"`
	Accounts    int       `json:"accounts"`
	AccountSets int       `json:"account_sets"`
	Settings    int       `json:"settings"`
}

// v1Export makes an export file of the owner's accounts and settings.
func (a *API) v1Export(w http.ResponseWriter, r *http.Request) {
	porter, ok := a.accountPorter(w, r)
	if !ok {
		return
	}
	p, _ := principalFrom(r.Context())
	// The nonce is spent only once the export runs (a busy answer keeps
	// it for the next try).
	if !a.checkReauth(w, r, opExport, "") {
		return
	}
	ctx := r.Context()
	accts, err := porter.ExportAccounts(ctx, p.OwnerID)
	if err != nil {
		a.accountWriteError(w, r, "reading the accounts", err)
		return
	}
	for i := range accts {
		if accts[i].Type != pubnostr.Type {
			continue
		}
		relays, err := a.Store.AccountSyncRelays(ctx, accts[i].ID)
		if err != nil {
			a.internalError(w, r, "reading the relay-sync lists", err)
			return
		}
		accts[i].SyncRelays = relays
	}
	sets, err := a.Store.AccountSets(ctx, p.OwnerID)
	if err != nil {
		a.internalError(w, r, "reading the account sets", err)
		return
	}
	if len(accts) > export.MaxAccounts || len(sets) > export.MaxSets {
		problem(w, r, http.StatusConflict, codeInvalidState, fmt.Sprintf("an export file holds at most %d accounts and %d account sets: delete some first",
			export.MaxAccounts, export.MaxSets))
		return
	}
	c := export.Contents{Version: export.ContentsVersion, Accounts: accts, Sets: make([]export.Set, 0, len(sets)),
		Owner: map[string]any{}, Instance: map[string]any{}}
	if c.Accounts == nil {
		c.Accounts = []export.Account{}
	}
	for _, s := range sets {
		c.Sets = append(c.Sets, export.Set{Name: s.Name, Accounts: s.Accounts, IsDefault: s.IsDefault})
	}
	if a.Settings != nil {
		snap := a.Settings.Current()
		c.Owner, c.Instance = snap.Saved(settings.Owner), snap.Saved(settings.Instance)
	}
	plain, err := json.Marshal(c)
	if err != nil {
		a.internalError(w, r, "writing the export", err)
		return
	}
	defer clear(plain)
	pass, err := export.NewPassphrase()
	if err != nil {
		a.internalError(w, r, "making the passphrase", err)
		return
	}
	now := time.Now()
	var file []byte
	var serr error
	var wait time.Duration
	spent := false
	if !a.withKDF(func() {
		if wait = a.limits().exports.Take(p.OwnerID); wait > 0 {
			return
		}
		if spent = a.requireReauth(w, r, opExport, ""); spent {
			file, serr = export.Seal(plain, pass, now)
		}
	}) {
		rateLimited(w, r, 2*time.Second, "an export or import is running; try again in a moment")
		return
	}
	switch {
	case wait > 0:
		rateLimited(w, r, wait, "too many exports and imports in a minute")
		return
	case !spent:
		return // requireReauth answered
	}
	if errors.Is(serr, export.ErrTooLarge) {
		problem(w, r, http.StatusConflict, codeInvalidState, "the accounts and settings are more than an export file holds (768 KiB)")
		return
	}
	if serr != nil {
		a.internalError(w, r, "sealing the export", serr)
		return
	}
	slog.InfoContext(ctx, "v1: the owner's accounts and settings were exported", "accounts", len(c.Accounts),
		"account_sets", len(c.Sets), "settings", len(c.Owner)+len(c.Instance))
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, v1ExportBody{Passphrase: pass, Filename: "publisher-" + now.UTC().Format("2006-01-02") + ".export",
		File: string(file), CreatedAt: now.UTC().Truncate(time.Second), Accounts: len(c.Accounts), AccountSets: len(c.Sets),
		Settings: len(c.Owner) + len(c.Instance)})
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	slog.ErrorContext(r.Context(), "v1: "+doing+" failed", "err", err)
	problem(w, r, http.StatusInternalServerError, codeInternal, doing+" failed")
}

// maxPassphrase bounds the passphrase field (24 words are at most 215
// characters; room for spacing and numbers pasted with them).
const maxPassphrase = 1024

// v1ImportIn is POST /v1/import's body.
type v1ImportIn struct {
	File       string `json:"file"`
	Passphrase string `json:"passphrase"`
}

// Import actions, in a summary.
const (
	importAdd    = "add"    // added (an account, a set)
	importUpdate = "update" // an environment account here: what the owner chose for it
	importSet    = "set"    // a setting saved
	importSkip   = "skip"
)

type v1ImportAccount struct {
	// FileID is its id in the file (account sets name it so).
	FileID string `json:"file_id"`
	Type   string `json:"type"`
	Label  string `json:"label"`
	Handle string `json:"handle,omitempty"`
	// Server is where its credentials are sent ("" for a network with
	// one address).
	Server string `json:"server,omitempty"`
	Action string `json:"action"`
	Why    string `json:"why,omitempty"`
	// After the import: its id here, and how it went (connected,
	// refused, unchecked, skipped, updated).
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	// here is the account already here that it is (skipped as such): the
	// file's account sets get it in its place.
	here string
}

type v1ImportSetting struct {
	Scope string `json:"scope"`
	Key   string `json:"key"`
	Label string `json:"label"`
	// Value is what it would be: the value, an address's origin for a
	// secret one, and nothing for another secret (Secret true).
	Value  any    `json:"value,omitempty"`
	Secret bool   `json:"secret,omitempty"`
	Action string `json:"action"`
	Why    string `json:"why,omitempty"`
}

type v1ImportSet struct {
	Name     string `json:"name"`
	Accounts int    `json:"accounts"`
	Action   string `json:"action"`
	Why      string `json:"why,omitempty"`
}

// v1ImportSummary is what a file would do (POST /v1/import), or what it
// did (:confirm).
type v1ImportSummary struct {
	Confirmation string            `json:"confirmation,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	Accounts     []v1ImportAccount `json:"accounts"`
	AccountSets  []v1ImportSet     `json:"account_sets"`
	Settings     []v1ImportSetting `json:"settings"`
}

// importBlocked says why the owner can't import now ("" when they can):
// an import goes into a server without accounts added in the app, so two
// owners' setups are never merged.
func (a *API) importBlocked(owner string) string {
	for _, e := range a.Accounts.List(owner) {
		if e.Row.Source == store.SourceDB {
			return "an import goes into a server with no accounts added in the app; this one has some (delete them first, or import on a new server)"
		}
	}
	return ""
}

// v1Import reads an export file and answers what importing it would do,
// with a confirmation to send to :confirm within importTTL.
func (a *API) v1Import(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.accountPorter(w, r); !ok {
		return
	}
	var in v1ImportIn
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	if p.Session == nil {
		problem(w, r, http.StatusForbidden, codeSessionRequired, "only the owner, signed in in a browser, can do this; a token can't")
		return
	}
	if why := a.importBlocked(p.OwnerID); why != "" {
		problem(w, r, http.StatusConflict, codeInvalidState, why)
		return
	}
	po := a.port()
	if !po.running.TryLock() {
		problem(w, r, http.StatusConflict, codeInvalidState, errImportRunning)
		return
	}
	po.running.Unlock()
	if err := a.MasterKey.WriteError(); err != nil {
		a.accountWriteError(w, r, "importing", err)
		return
	}
	file := []byte(in.File)
	if _, _, _, err := export.ReadHeader(file); err != nil {
		invalid(w, r, []fieldError{{Pointer: "/file", Code: "invalid", Detail: err.Error()}})
		return
	}
	if len(in.Passphrase) > maxPassphrase {
		invalid(w, r, []fieldError{{Pointer: "/passphrase", Code: "too_long", Detail: export.ErrPassphraseForm.Error()}})
		return
	}
	pass, err := export.ParsePassphrase(in.Passphrase)
	if err != nil {
		invalid(w, r, []fieldError{{Pointer: "/passphrase", Code: "invalid", Detail: err.Error()}})
		return
	}
	var h export.Header
	var plain []byte
	var oerr error
	var wait time.Duration
	if !a.withKDF(func() {
		// Counted only when it runs: a busy answer costs no try.
		if wait = a.limits().exports.Take(p.OwnerID); wait == 0 {
			h, plain, oerr = export.Open(file, pass)
		}
	}) {
		rateLimited(w, r, 2*time.Second, "an export or import is running; try again in a moment")
		return
	}
	if wait > 0 {
		rateLimited(w, r, wait, "too many exports and imports in a minute")
		return
	}
	if oerr != nil {
		ptr := "/file"
		if errors.Is(oerr, export.ErrDoesntOpen) {
			ptr = "/passphrase"
		}
		invalid(w, r, []fieldError{{Pointer: ptr, Code: "invalid", Detail: oerr.Error()}})
		return
	}
	c, err := export.Decode(plain)
	clear(plain)
	if err != nil {
		invalid(w, r, []fieldError{{Pointer: "/file", Code: "invalid", Detail: err.Error()}})
		return
	}
	for i := range c.Accounts {
		// A label this server wouldn't take: the network's name instead.
		if l, fe := checkLabel(c.Accounts[i].Label, true); fe != nil {
			c.Accounts[i].Label = ""
		} else {
			c.Accounts[i].Label = l
		}
	}
	sum := a.planImport(r.Context(), p.OwnerID, c)
	sum.CreatedAt = h.CreatedAt
	id := "imp_" + randomHex(16)
	exp := time.Now().Add(importTTL).UTC().Truncate(time.Second)
	sum.Confirmation, sum.ExpiresAt = id, &exp
	pi := &pendingImport{owner: p.OwnerID, session: p.Session.ID, contents: c, header: h, expires: exp}
	po.mu.Lock()
	for k, x := range po.pending {
		// One file waits per session; expired ones go.
		if x.session == p.Session.ID || time.Now().After(x.expires) {
			delete(po.pending, k)
		}
	}
	po.pending[id] = pi
	po.mu.Unlock()
	// What the file holds (credentials in the clear) is dropped when its
	// time is up, whether or not anything else comes.
	time.AfterFunc(importTTL, func() { po.drop(id, pi) })
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, sum)
}

// errImportRunning is the answer while another import is confirmed.
const errImportRunning = "an import is running on this server; wait for it to finish"

// drop forgets a read file, if it is still the one waiting under id.
func (po *porting) drop(id string, x *pendingImport) {
	po.mu.Lock()
	if po.pending[id] == x {
		delete(po.pending, id)
	}
	po.mu.Unlock()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// planImport is what importing c would do, without doing it.
func (a *API) planImport(ctx context.Context, owner string, c export.Contents) v1ImportSummary {
	sum := v1ImportSummary{Accounts: []v1ImportAccount{}, AccountSets: []v1ImportSet{}, Settings: []v1ImportSetting{}}
	porter, _ := a.Accounts.(AccountPorter)
	added := map[string]bool{}
	seen := map[string]bool{} // type and remote id of an app account already planned
	for _, x := range c.Accounts {
		v := v1ImportAccount{FileID: x.ID, Type: x.Type, Label: x.Label, Handle: x.Handle}
		d, ok := destination.Lookup(x.Type)
		if ok && x.Source == export.SourceApp {
			// Only the origin: never a user name or a token in an address.
			v.Server = settings.Origin(destination.AudienceOf(d.Manifest(), x.Config))
		}
		switch {
		case x.Source == export.SourceEnv:
			if row, ok := porter.EnvMatch(owner, x); ok {
				v.Action, v.Why = importUpdate, "this server's environment sets it; from the file: "+metaSummary(x)
				v.ID = row.ID
				added[x.ID] = true
			} else {
				v.Action, v.Why = importSkip, "set by the environment where it was exported, and not here"
			}
		case !ok:
			v.Action, v.Why = importSkip, "this server has no accounts of that network"
		default:
			key := x.Type + "\x00" + x.RemoteID
			if why, here := porter.CheckImport(owner, x); why != "" {
				v.Action, v.Why, v.here = importSkip, why, here
				if here != "" {
					added[x.ID] = true
				}
			} else if x.RemoteID != "" && seen[key] {
				v.Action, v.Why = importSkip, "it is in the file twice"
			} else {
				seen[key] = true
				v.Action = importAdd
				added[x.ID] = true
			}
		}
		sum.Accounts = append(sum.Accounts, v)
	}
	// Each set is checked as one made here is, with the accounts that
	// would be here in it.
	names := map[string]bool{}
	count := 0
	if sets, err := a.Store.AccountSets(ctx, owner); err == nil {
		count = len(sets)
		for _, s := range sets {
			names[strings.ToLower(s.Name)] = true
		}
	}
	for _, s := range c.Sets {
		members := []string{}
		for _, id := range s.Accounts {
			if added[id] && !slices.Contains(members, id) {
				members = append(members, id)
			}
		}
		v := v1ImportSet{Name: s.Name, Accounts: len(members), Action: importAdd}
		name := strings.ToLower(strings.TrimSpace(s.Name))
		switch {
		case len(members) == 0:
			v.Action, v.Why = importSkip, "none of its accounts would be here"
		case names[name]:
			v.Action, v.Why = importSkip, errNameTaken.Detail
		case count >= maxAccountSets:
			v.Action, v.Why = importSkip, fmt.Sprintf("at most %d account sets", maxAccountSets)
		default:
			if _, errs := checkAccountSet(accountSetIn(s, members), added, true); len(errs) > 0 {
				v.Action, v.Why = importSkip, errs[0].Detail
			} else {
				names[name] = true
				count++
			}
		}
		sum.AccountSets = append(sum.AccountSets, v)
	}
	if a.Settings != nil {
		for _, sc := range []struct {
			scope settings.Scope
			vals  map[string]any
		}{{settings.Instance, c.Instance}, {settings.Owner, c.Owner}} {
			set, skipped := a.Settings.PlanImport(ctx, sc.scope, sc.vals)
			sum.Settings = append(sum.Settings, a.importSettings(sc.scope, sc.vals, set, skipped)...)
		}
	}
	return sum
}

// accountSetIn is a file's set as POST /v1/account-sets takes one, with
// members.
func accountSetIn(s export.Set, members []string) v1AccountSetIn {
	name, _ := json.Marshal(s.Name)
	list, _ := json.Marshal(members)
	def, _ := json.Marshal(s.IsDefault)
	return v1AccountSetIn{Name: name, Accounts: list, IsDefault: def}
}

// importSettings lists a scope's settings in a summary: those set, then
// those left as they are.
func (a *API) importSettings(scope settings.Scope, vals map[string]any, set, skipped []settings.Item) []v1ImportSetting {
	var out []v1ImportSetting
	for _, it := range set {
		out = append(out, a.importSetting(scope, it.Key, vals[it.Key], importSet, it.Why))
	}
	for _, it := range skipped {
		out = append(out, a.importSetting(scope, it.Key, vals[it.Key], importSkip, it.Why))
	}
	return out
}

// metaSummary says what a file sets on an account the environment sets
// here: what the owner chose for it.
func metaSummary(x export.Account) string {
	var parts []string
	if l := strings.TrimSpace(x.Label); l != "" {
		parts = append(parts, "the label “"+l+"”")
	}
	if x.Enabled {
		parts = append(parts, "turned on")
	} else {
		parts = append(parts, "turned off")
	}
	if x.DefaultSelected {
		parts = append(parts, "picked for new posts")
	} else {
		parts = append(parts, "not picked for new posts")
	}
	if x.PublicFeed {
		parts = append(parts, "in the public feed")
	} else {
		parts = append(parts, "not in the public feed")
	}
	if len(x.Defaults) > 0 && string(x.Defaults) != "{}" && string(x.Defaults) != "null" {
		parts = append(parts, "its defaults")
	}
	if x.SyncRelays != nil {
		parts = append(parts, fmt.Sprintf("%d relays to sync", len(x.SyncRelays)))
	}
	return strings.Join(parts, ", ")
}

// importSetting is one setting in a summary: a secret shows nothing but
// an address's origin.
func (a *API) importSetting(scope settings.Scope, key string, val any, action, why string) v1ImportSetting {
	v := v1ImportSetting{Scope: string(scope), Key: key, Label: key, Action: action, Why: why}
	d, ok := a.Settings.Def(key)
	if !ok {
		return v
	}
	v.Label = d.Field.Label
	switch {
	case d.Field.Secret && d.Field.Type == destination.FieldURL:
		s, _ := val.(string)
		v.Secret, v.Value = true, settings.Origin(s)
	case d.Field.Secret:
		v.Secret = true
	default:
		v.Value = val
	}
	return v
}

// v1ImportConfirmIn is POST /v1/import:confirm's body.
type v1ImportConfirmIn struct {
	Confirmation string `json:"confirmation"`
}

// v1ImportConfirm imports a file read by POST /v1/import, as its summary
// said, and answers what it did.
func (a *API) v1ImportConfirm(w http.ResponseWriter, r *http.Request) {
	porter, ok := a.accountPorter(w, r)
	if !ok {
		return
	}
	var in v1ImportConfirmIn
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := principalFrom(r.Context())
	po := a.port()
	const gone = "no import waiting for this confirmation here (they wait 10 minutes): read the file again"
	po.mu.Lock()
	x := po.pending[in.Confirmation]
	po.mu.Unlock()
	if x != nil && time.Now().After(x.expires) {
		po.drop(in.Confirmation, x)
		x = nil
	}
	if x == nil || p.Session == nil || x.owner != p.OwnerID || x.session != p.Session.ID {
		problem(w, r, http.StatusNotFound, codeNotFound, gone)
		return
	}
	// One import at a time, and the server checked again for accounts
	// added in the app while it holds it.
	if !po.running.TryLock() {
		problem(w, r, http.StatusConflict, codeInvalidState, errImportRunning)
		return
	}
	defer po.running.Unlock()
	if why := a.importBlocked(p.OwnerID); why != "" {
		problem(w, r, http.StatusConflict, codeInvalidState, why)
		return
	}
	if !a.requireReauth(w, r, opImport, "") {
		return
	}
	po.mu.Lock()
	if po.pending[in.Confirmation] != x {
		// Read again, or confirmed, since.
		po.mu.Unlock()
		problem(w, r, http.StatusNotFound, codeNotFound, gone)
		return
	}
	delete(po.pending, in.Confirmation)
	po.mu.Unlock()
	// Begun, it finishes, even if the caller goes; the answer may take
	// longer than others.
	ctx := context.WithoutCancel(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(importBudget + 30*time.Second))
	actor, _ := actorOf(p)
	sum := a.applyImport(ctx, p.OwnerID, actor, x.contents, porter)
	// Done with what the file holds (its timer still points at it).
	x.contents = export.Contents{}
	sum.CreatedAt = x.header.CreatedAt
	slog.InfoContext(ctx, "v1: an export file was imported", "accounts", countAction(sum.Accounts), "settings", len(sum.Settings))
	w.Header().Set("Cache-Control", "no-store")
	writeV1(w, http.StatusOK, sum)
}

func countAction(as []v1ImportAccount) int {
	n := 0
	for _, a := range as {
		if a.ID != "" {
			n++
		}
	}
	return n
}

// applyImport does what planImport said: the settings (the server's, then
// the owner's), the accounts (several at once: each is opened and
// identified), then the account sets.
func (a *API) applyImport(ctx context.Context, owner, actor string, c export.Contents, porter AccountPorter) v1ImportSummary {
	sum := a.planImport(ctx, owner, c)
	sum.Settings = sum.Settings[:0]
	if a.Settings != nil {
		for _, sc := range []struct {
			scope settings.Scope
			vals  map[string]any
		}{{settings.Instance, c.Instance}, {settings.Owner, c.Owner}} {
			saved, skipped, err := a.Settings.Import(ctx, sc.scope, sc.vals, actor)
			if err != nil {
				// Not a value refused (those are skipped): the database.
				slog.ErrorContext(ctx, "import: saving settings failed", "scope", sc.scope, "err", err)
				left := map[string]bool{}
				for _, it := range skipped {
					left[it.Key] = true
				}
				for _, k := range sortedKeys(sc.vals) {
					if !left[k] {
						skipped = append(skipped, settings.Item{Key: k, Why: "saving it failed"})
					}
				}
				saved = nil
			}
			sum.Settings = append(sum.Settings, a.importSettings(sc.scope, sc.vals, saved, skipped)...)
		}
	}
	byFile := map[string]export.Account{}
	for _, x := range c.Accounts {
		byFile[x.ID] = x
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, importWorkers)
	for i := range sum.Accounts {
		v := &sum.Accounts[i]
		x := byFile[v.FileID]
		switch v.Action {
		case importUpdate:
			v.Status = "updated"
			v.Detail = a.applyAccountMeta(ctx, owner, actor, v.ID, x, true)
		case importAdd:
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() {
					if p := recover(); p != nil {
						// Not the value: it may hold what the account was
						// opened with.
						slog.ErrorContext(ctx, "import: adding an account panicked", "type", x.Type, "panic", fmt.Sprintf("%T", p))
						if v.ID == "" {
							v.Status, v.Detail = accounts.ImportSkipped, "adding it failed on this server"
						}
					}
					<-sem
					wg.Done()
				}()
				res, err := porter.ImportAccount(ctx, owner, x, actor)
				if err != nil {
					slog.ErrorContext(ctx, "import: adding an account failed", "type", x.Type, "err", err)
					v.Status, v.Detail = accounts.ImportSkipped, "adding it failed on this server"
					return
				}
				v.ID, v.Status, v.Detail = res.ID, res.Status, res.Detail
				if res.Handle != "" {
					v.Handle = res.Handle
				}
				if res.ID == "" {
					return
				}
				if note := a.applyAccountMeta(ctx, owner, actor, res.ID, x, false); note != "" {
					v.Detail = strings.TrimSpace(v.Detail + " " + note)
				}
			}()
		default:
			v.Status = accounts.ImportSkipped
		}
	}
	wg.Wait()
	ids := map[string]string{}
	for _, v := range sum.Accounts {
		switch {
		case v.ID != "":
			ids[v.FileID] = v.ID
		case v.here != "":
			ids[v.FileID] = v.here
		}
	}
	live := map[string]bool{}
	for _, e := range a.Accounts.List(owner) {
		live[e.Row.ID] = true
	}
	for i := range sum.AccountSets {
		v := &sum.AccountSets[i]
		if v.Action != importAdd {
			continue
		}
		set := c.Sets[i]
		members := []string{}
		for _, id := range set.Accounts {
			if here, ok := ids[id]; ok && !slices.Contains(members, here) {
				members = append(members, here)
			}
		}
		v.Accounts = len(members)
		if len(members) == 0 {
			v.Action, v.Why = importSkip, "none of its accounts are here"
			continue
		}
		// Checked as one made here is.
		ch, errs := checkAccountSet(accountSetIn(set, members), live, true)
		if len(errs) > 0 {
			v.Action, v.Why = importSkip, errs[0].Detail
			continue
		}
		_, err := a.Store.CreateAccountSet(ctx, owner, ch)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrAccountSetName):
			v.Action, v.Why = importSkip, errNameTaken.Detail
		case errors.Is(err, store.ErrAccountSetsFull):
			v.Action, v.Why = importSkip, fmt.Sprintf("at most %d account sets", maxAccountSets)
		default:
			slog.ErrorContext(ctx, "import: an account set wasn't saved", "err", err)
			v.Action, v.Why = importSkip, "saving it failed"
		}
	}
	return sum
}

// applyAccountMeta gives an account here what the owner chose for it in
// the file: its label, flags, defaults and relay-sync list. It answers a
// note when some of it couldn't be kept.
func (a *API) applyAccountMeta(ctx context.Context, owner, actor, id string, x export.Account, env bool) string {
	row, err := a.Store.Account(ctx, owner, id)
	if err != nil {
		slog.ErrorContext(ctx, "import: reading an account failed", "account", id, "err", err)
		return "its label and flags weren't kept"
	}
	m := store.AccountMeta{Enabled: &x.Enabled, DefaultSelected: &x.DefaultSelected, PublicFeed: &x.PublicFeed, Actor: actor}
	if l := strings.TrimSpace(x.Label); l != "" && env {
		m.Label = &l
	}
	note := ""
	if len(x.Defaults) > 0 {
		if s, errs := a.checkAccountDefaults(ctx, row, x.Defaults); len(errs) == 0 {
			m.Defaults = &s
		} else {
			note = "its defaults weren't kept (" + errs[0].Detail + ")."
		}
	}
	saved, err := a.Store.UpdateAccountMeta(ctx, owner, id, m)
	if err != nil {
		slog.ErrorContext(ctx, "import: an account's label and flags weren't saved", "account", id, "err", err)
		return "its label and flags weren't kept"
	}
	if err := a.Accounts.Update(id, saved); err != nil {
		slog.WarnContext(ctx, "import: the registry didn't take an account's flags", "account", id, "err", err)
	}
	if x.SyncRelays != nil && row.Type == pubnostr.Type {
		if why := a.importSyncRelays(ctx, id, x); why != "" {
			note = strings.TrimSpace(note + " its relay-sync list wasn't kept (" + why + ").")
		}
	}
	return note
}

// importSyncRelays gives an account here a file's relay-sync list,
// checked as one the owner sets: "" when it was saved, else why not. A
// relay that is this account's home relay here is left out.
func (a *API) importSyncRelays(ctx context.Context, id string, x export.Account) string {
	home := ""
	if acct, release, err := a.Accounts.Acquire(id); err == nil {
		if l, ok := acct.(pubnostr.RelayLister); ok {
			home = l.HomeRelay()
		}
		release()
	}
	if s, ok := x.Config[pubnostr.ConfigBootstrapRelay].(string); ok && home == "" {
		home = s
	}
	list := slices.DeleteFunc(slices.Clone(x.SyncRelays), func(u string) bool { return home != "" && normalizeRelay(u) == normalizeRelay(home) })
	urls, errs := checkSyncRelays(list, home, "")
	if len(errs) > 0 {
		return errs[0].Detail
	}
	if err := a.Store.SetAccountSyncRelays(ctx, id, urls); err != nil {
		slog.ErrorContext(ctx, "import: a relay-sync list wasn't saved", "account", id, "err", err)
		return "saving it failed"
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
