package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/feed"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/relaysync"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/translate"
	"github.com/geofox/publisher/internal/verify"
)

// The tools: translation, signature verification, Nostr relay sync and the
// public feed.

// ─── POST /v1/translate ──────────────────────────────────────────────────

type v1TranslateReq struct {
	Text       string `json:"text"`
	TargetLang string `json:"target_lang"`
}

type v1TranslateBody struct {
	Text                   string `json:"text"`
	DetectedSourceLanguage string `json:"detected_source_language"`
}

func (a *API) v1Translate(w http.ResponseWriter, r *http.Request) {
	var req v1TranslateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	tr := a.translator()
	if tr == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "translation is not configured on this server (DEEPL_API_KEY)")
		return
	}
	var errs []fieldError
	switch {
	case strings.TrimSpace(req.Text) == "":
		errs = append(errs, fieldError{Pointer: "/text", Code: "required", Detail: "the text to translate"})
	case utf8.RuneCountInString(req.Text) > maxPostTextRunes:
		errs = append(errs, fieldError{Pointer: "/text", Code: "too_long", Detail: "at most 50000 characters"})
	}
	if !translate.IsSupported(req.TargetLang) {
		errs = append(errs, fieldError{Pointer: "/target_lang", Code: "unsupported_language",
			Detail: "an ISO 639-1 code DeepL translates into; GET /v1/instance lists yours as translate_targets"})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	text, src, err := tr.Translate(r.Context(), req.Text, strings.ToLower(req.TargetLang))
	if err != nil {
		slog.WarnContext(r.Context(), "v1: translate", "err", store.Scrub(err.Error()))
		problem(w, r, http.StatusBadGateway, codeUpstream, "the translation service failed or could not be reached; try again")
		return
	}
	writeV1(w, http.StatusOK, v1TranslateBody{Text: text, DetectedSourceLanguage: src})
}

// ─── POST /v1/verify ─────────────────────────────────────────────────────

type v1VerifyReq struct {
	Input    string `json:"input"`
	Platform string `json:"platform"`
	Expected string `json:"expected"`
}

// v1Verify checks who signed a pasted Nostr event or a post URL. The
// verdict is the answer, whatever it says: "error" means the check could
// not complete, which is not evidence of forgery.
func (a *API) v1Verify(w http.ResponseWriter, r *http.Request) {
	var req v1VerifyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if a.Verify == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "verification is not configured on this server")
		return
	}
	var errs []fieldError
	switch {
	case strings.TrimSpace(req.Input) == "":
		errs = append(errs, fieldError{Pointer: "/input", Code: "required", Detail: "a Nostr event (JSON), a post URL or an at:// URI"})
	case len(req.Input) > int(maxVerifyRequestBytes):
		errs = append(errs, fieldError{Pointer: "/input", Code: "too_long", Detail: "at most 512 KiB"})
	}
	// The platform override names a destination type.
	if p := strings.ToLower(strings.TrimSpace(req.Platform)); p != "" && !contains(destination.Types(), p) {
		errs = append(errs, fieldError{Pointer: "/platform", Code: "invalid", Detail: "one of " + strings.Join(destination.Types(), ", ") + ", or leave it out to detect"})
	}
	if utf8.RuneCountInString(req.Expected) > 500 {
		errs = append(errs, fieldError{Pointer: "/expected", Code: "too_long", Detail: "at most 500 characters"})
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	v := a.Verify.Verify(r.Context(), verify.Input{Raw: req.Input, Platform: req.Platform, Expected: req.Expected})
	if v.Checks == nil {
		v.Checks = []verify.Check{}
	}
	writeV1(w, http.StatusOK, v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ─── /v1/accounts/{id}/nostr/relays ──────────────────────────────────────
//
// Relay sync copies your Nostr events between your home relay and your
// other relays: the NIP-65 write relays your profile lists, plus a
// secondary list kept here.

const (
	maxSyncRelays   = 50
	maxSyncRelayURL = 512
	// relaySyncBudget bounds one scan or sync; name fewer relays per call
	// when many are slow.
	relaySyncBudget = 5 * time.Minute
)

type v1RelaysBody struct {
	// Home is the relay the others are compared with.
	Home string `json:"home"`
	// NIP65 are the write relays your profile lists (read from relays).
	NIP65 []string `json:"nip65"`
	// Secondary are the relays added here.
	Secondary []string `json:"secondary"`
	// Targets are the relays a scan or sync goes through, in order.
	Targets []relaysync.Target `json:"targets"`
	// NIP65Error says why the NIP-65 relays couldn't be read (they are
	// then missing from nip65 and targets); null when they were.
	NIP65Error *string `json:"nip65_error"`
}

type v1RelaysPut struct {
	Secondary []string `json:"secondary"`
}

type v1RelaysScanReq struct {
	Relays []string `json:"relays"`
}

type v1RelaysSyncReq struct {
	Direction string   `json:"direction"`
	Relays    []string `json:"relays"`
}

// relayAccount is a leased account that has relays (a Nostr account).
type relayAccount struct {
	id      string
	lister  pubnostr.RelayLister
	release func()
}

// relayOwner is who a scan or sync works for. The home relay is the
// account's own (its network policy); the targets are relays the owner
// set (NIP-65, or added here), dialed through the guard for owner-set
// addresses whoever set the account up.
func (a *API) relayOwner(ra relayAccount) relaysync.Owner {
	return relaysync.Owner{Home: ra.lister.HomeRelay(), Pubkey: ra.lister.OwnerPubkey(),
		HomeClient: ra.lister.RelayClient(), TargetClient: destination.GuardedNet(a.Guard.Allow).Client(0)}
}

// relayAccountOf leases the request's account when it is one of the
// caller's accounts with relays; otherwise it writes the problem and
// returns false. release must be called when it returns true.
func (a *API) relayAccountOf(w http.ResponseWriter, r *http.Request) (relayAccount, bool) {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return relayAccount{}, false
	}
	if a.Accounts == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "accounts aren't loaded on this server")
		return relayAccount{}, false
	}
	p, _ := principalFrom(r.Context())
	id := r.PathValue("id")
	e, ok := a.Accounts.Get(id)
	if !ok || e.Row.OwnerID != p.OwnerID {
		problem(w, r, http.StatusNotFound, codeNotFound, "no account "+id)
		return relayAccount{}, false
	}
	if e.Row.Type != pubnostr.Type {
		problem(w, r, http.StatusNotFound, codeNotFound, "relays belong to Nostr accounts; "+id+" is not one")
		return relayAccount{}, false
	}
	acct, release, err := a.Accounts.Acquire(id)
	if err != nil {
		problem(w, r, http.StatusConflict, codeInvalidState, err.Error())
		return relayAccount{}, false
	}
	lister, ok := acct.(pubnostr.RelayLister)
	if !ok {
		release()
		problem(w, r, http.StatusNotFound, codeNotFound, "this account has no relays to sync")
		return relayAccount{}, false
	}
	return relayAccount{id: id, lister: lister, release: release}, true
}

// relaySyncAvailable reports whether relay sync can run for one of the
// caller's accounts now.
func (a *API) relaySyncAvailable(ctx context.Context) bool {
	if a.Sync == nil || a.Accounts == nil {
		return false
	}
	p, _ := principalFrom(ctx)
	for _, e := range a.Accounts.List(p.OwnerID) {
		if e.Status != accounts.Connected {
			continue
		}
		acct, release, err := a.Accounts.Acquire(e.Row.ID)
		if err != nil {
			continue
		}
		_, ok := acct.(pubnostr.RelayLister)
		release()
		if ok {
			return true
		}
	}
	return false
}

func (a *API) v1RelaysView(ctx context.Context, ra relayAccount) (v1RelaysBody, error) {
	var nip65 []string
	var nip65Err *string
	got, err := ra.lister.ResolveWriteRelays(ctx)
	if err != nil {
		slog.WarnContext(ctx, "v1: relays: NIP-65 lookup", "account", ra.id, "err", err)
		msg := "your relay list (NIP-65) could not be read from the bootstrap relay; try again"
		nip65Err = &msg
	} else {
		nip65 = got
	}
	secondary, err := a.Store.AccountSyncRelays(ctx, ra.id)
	if err != nil {
		return v1RelaysBody{}, err
	}
	if nip65 == nil {
		nip65 = []string{}
	}
	if secondary == nil {
		secondary = []string{}
	}
	home := ra.lister.HomeRelay()
	return v1RelaysBody{Home: home, NIP65: nip65, Secondary: secondary,
		Targets: relaysync.ResolveTargets(nip65, secondary, home), NIP65Error: nip65Err}, nil
}

func (a *API) writeRelays(w http.ResponseWriter, r *http.Request, ra relayAccount) {
	body, err := a.v1RelaysView(r.Context(), ra)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: relays", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the relays failed")
		return
	}
	writeV1(w, http.StatusOK, body)
}

func (a *API) v1GetRelays(w http.ResponseWriter, r *http.Request) {
	if ra, ok := a.relayAccountOf(w, r); ok {
		defer ra.release()
		a.writeRelays(w, r, ra)
	}
}

// v1PutRelays replaces the secondary relay list.
func (a *API) v1PutRelays(w http.ResponseWriter, r *http.Request) {
	var req v1RelaysPut
	if !decodeJSON(w, r, &req) {
		return
	}
	ra, ok := a.relayAccountOf(w, r)
	if !ok {
		return
	}
	defer ra.release()
	if req.Secondary == nil {
		invalid(w, r, []fieldError{{Pointer: "/secondary", Code: "required", Detail: "the relay list ([] for none)"}})
		return
	}
	urls, errs := checkSyncRelays(req.Secondary, ra.lister.HomeRelay(), "/secondary")
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if err := a.Store.SetAccountSyncRelays(r.Context(), ra.id, urls); err != nil {
		slog.ErrorContext(r.Context(), "v1: set relays", "account", ra.id, "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the relays failed")
		return
	}
	a.writeRelays(w, r, ra)
}

// hasBody reports whether the request carries a body, peeking when its
// length isn't declared (a chunked request).
func hasBody(r *http.Request) bool {
	if r.ContentLength >= 0 || r.Body == nil {
		return r.ContentLength > 0
	}
	br := bufio.NewReader(r.Body)
	_, err := br.Peek(1)
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return err == nil
}

// checkSyncRelays checks a relay-sync list as the owner sets it (PUT, or
// an import): at most 50 ws:// or wss:// relays, none of them the home
// relay, one on Tor or I2P, on a loopback or link-local address, or twice.
// It answers the list normalized, or what is wrong, under ptr.
func checkSyncRelays(list []string, home, ptr string) ([]string, []fieldError) {
	if len(list) > maxSyncRelays {
		return nil, []fieldError{{Pointer: ptr, Code: "too_many", Detail: "at most 50 relays"}}
	}
	var errs []fieldError
	seen := map[string]bool{}
	urls := []string{}
	home = normalizeRelay(home)
	for i, raw := range list {
		p := ptr + "/" + strconv.Itoa(i)
		u := normalizeRelay(raw)
		switch {
		case len(raw) > maxSyncRelayURL:
			errs = append(errs, fieldError{Pointer: p, Code: "too_long", Detail: "at most 512 characters"})
		case !validRelayURL(u):
			errs = append(errs, fieldError{Pointer: p, Code: "invalid", Detail: "a ws:// or wss:// relay URL"})
		case u == home:
			errs = append(errs, fieldError{Pointer: p, Code: "not_allowed", Detail: "this is the home relay, which the others are compared with"})
		case pubnostr.IsOverlayRelay(u):
			errs = append(errs, fieldError{Pointer: p, Code: "not_allowed", Detail: "Tor and I2P relays can't be reached from the server"})
		case localRelay(u):
			errs = append(errs, fieldError{Pointer: p, Code: "not_allowed", Detail: "loopback and link-local addresses are refused (a relay on your network is fine)"})
		case seen[u]:
			errs = append(errs, fieldError{Pointer: p, Code: "duplicate", Detail: "this relay is already in the list"})
		default:
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls, errs
}

// normalizeRelay is the form relay URLs are stored and compared in: no
// surrounding space or trailing slash, scheme and host in lower case, and
// no default port.
func normalizeRelay(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "wss" && p == "443") || (u.Scheme == "ws" && p == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return strings.TrimRight(u.String(), "/")
}

// localRelay reports a relay on this machine or a link-local address (the
// cloud metadata service lives there): never a relay worth syncing with,
// and a way to probe the server's own ports. Private (LAN) addresses stay
// allowed for a relay at home.
func localRelay(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return true
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast())
}

// validRelayURL reports a ws(s) URL with a host and nothing a relay URL
// doesn't carry.
func validRelayURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "ws" || u.Scheme == "wss") && u.Host != "" && u.User == nil &&
		u.Fragment == "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, " \t\r\n")
}

// errNIP65: a named relay may be a NIP-65 one that couldn't be read.
var errNIP65 = errors.New("NIP-65 relays unavailable")

// syncTargets narrows the sync targets to the relays a request names
// (every target when it names none). A named relay that isn't a target is
// a field error.
func (a *API) syncTargets(ctx context.Context, ra relayAccount, relays []string) ([]relaysync.Target, []fieldError, error) {
	view, err := a.v1RelaysView(ctx, ra)
	if err != nil {
		return nil, nil, err
	}
	if len(relays) == 0 {
		return view.Targets, nil, nil
	}
	if len(relays) > maxSyncRelays+len(view.NIP65) {
		return nil, []fieldError{{Pointer: "/relays", Code: "too_many", Detail: "name each relay once"}}, nil
	}
	byURL := map[string]relaysync.Target{}
	for _, t := range view.Targets {
		byURL[normalizeRelay(t.URL)] = t
	}
	var out []relaysync.Target
	var errs []fieldError
	picked := map[string]bool{}
	for i, u := range relays {
		n := normalizeRelay(u)
		t, ok := byURL[n]
		if !ok && view.NIP65Error != nil {
			return nil, nil, errNIP65
		}
		if !ok {
			errs = append(errs, fieldError{Pointer: "/relays/" + strconv.Itoa(i), Code: "unknown_relay",
				Detail: "not one of this account's relays; see targets in GET on this path"})
			continue
		}
		if !picked[n] {
			picked[n] = true
			out = append(out, t)
		}
	}
	return out, errs, nil
}

// v1RelaysAction serves the custom methods on the relay list: :scan
// compares each relay with the home relay; :sync copies the missing events
// one way (pull: from the relays to home; push: from home to the relays).
func (a *API) v1RelaysAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var relays []string
		var direction string
		if action == "scan" {
			// The body is optional: none at all means every target.
			var req v1RelaysScanReq
			if hasBody(r) && !decodeJSON(w, r, &req) {
				return
			}
			relays = req.Relays
		} else {
			var req v1RelaysSyncReq
			if !decodeJSON(w, r, &req) {
				return
			}
			relays, direction = req.Relays, req.Direction
		}
		ra, ok := a.relayAccountOf(w, r)
		if !ok {
			return
		}
		defer ra.release()
		if a.Sync == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "relay sync is not configured on this server")
			return
		}
		if action == "sync" && direction != "pull" && direction != "push" {
			invalid(w, r, []fieldError{{Pointer: "/direction", Code: "invalid",
				Detail: "pull (copy from the relays to your home relay) or push (from home to the relays)"}})
			return
		}
		targets, errs, err := a.syncTargets(r.Context(), ra, relays)
		switch {
		case errors.Is(err, errNIP65):
			problem(w, r, http.StatusBadGateway, codeUpstream, "your relay list (NIP-65) could not be read from the bootstrap relay; try again")
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "v1: relays", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading the relays failed")
			return
		case len(errs) > 0:
			invalid(w, r, errs)
			return
		}
		// One scan or sync at a time: each holds a connection per relay
		// and every event of the account in memory.
		if !a.relaySyncMu.TryLock() {
			problem(w, r, http.StatusConflict, codeInvalidState, "a relay scan or sync is already running; try again when it ends")
			return
		}
		defer a.relaySyncMu.Unlock()
		// Relays are visited one after another, each allowed a minute: bound
		// the whole call, and keep the connection open long enough to
		// answer. Relays not reached in time are reported in their rows.
		ctx, cancel := context.WithTimeout(r.Context(), relaySyncBudget)
		defer cancel()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(relaySyncBudget + 30*time.Second))
		if action == "scan" {
			out := a.Sync.Scan(ctx, a.relayOwner(ra), targets)
			if out == nil {
				out = []relaysync.RelayDiff{}
			}
			writeV1(w, http.StatusOK, map[string]any{"relays": out})
			return
		}
		out := a.Sync.Apply(ctx, a.relayOwner(ra), targets, direction)
		if out == nil {
			out = []relaysync.ApplyResult{}
		}
		writeV1(w, http.StatusOK, map[string]any{"results": out})
	}
}

// ─── GET /v1/public/feed ─────────────────────────────────────────────────

// v1PublicFeed is your published posts for a homepage, gated by its own
// token (PUBLIC_FEED_TOKEN), not an API token: it is meant for a site's
// server, which should hold nothing that can post. Not found while no
// token is configured.
func (a *API) v1PublicFeed(w http.ResponseWriter, r *http.Request) {
	token := a.feedToken()
	if token == "" {
		problem(w, r, http.StatusNotFound, codeNotFound, "the public feed is off on this server (set its token in Settings, or PUBLIC_FEED_TOKEN)")
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) ||
		subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="public-feed"`)
		problem(w, r, http.StatusUnauthorized, codeUnauthenticated, "send Authorization: Bearer with the public feed token")
		return
	}
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			problem(w, r, http.StatusBadRequest, codeInvalidParameter, "limit must be a whole number from 1 to 100",
				fieldError{Parameter: "limit", Code: "out_of_range", Detail: "from 1 to 100"})
			return
		}
		limit = n
	}
	posts, err := a.Store.PublicFeed(limit)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: public feed", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the feed failed")
		return
	}
	writeV1(w, http.StatusOK, feed.Build(posts, limit))
}
