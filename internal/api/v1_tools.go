package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

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
	if a.Translator == nil {
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
	text, src, err := a.Translator.Translate(r.Context(), req.Text, strings.ToLower(req.TargetLang))
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

// nostrAccount answers the request's account if it is one of the caller's
// Nostr accounts; otherwise it writes the problem and returns false.
func (a *API) nostrAccount(w http.ResponseWriter, r *http.Request) bool {
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return false
	}
	p, _ := principalFrom(r.Context())
	accts, err := a.Store.Accounts(r.Context(), p.OwnerID)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: relays: accounts", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the account failed")
		return false
	}
	id := r.PathValue("id")
	for _, ac := range accts {
		if ac.ID != id {
			continue
		}
		if ac.Type != pubnostr.Type {
			problem(w, r, http.StatusNotFound, codeNotFound, "relays belong to Nostr accounts; "+id+" is not one")
			return false
		}
		if !ac.Enabled {
			problem(w, r, http.StatusConflict, codeInvalidState, "this account is disabled (not configured on this server)")
			return false
		}
		return true
	}
	problem(w, r, http.StatusNotFound, codeNotFound, "no account "+id)
	return false
}

func (a *API) v1RelaysView(r *http.Request) (v1RelaysBody, error) {
	var nip65 []string
	if a.np != nil {
		if got, err := a.np.ResolveWriteRelays(r.Context()); err == nil {
			nip65 = got
		}
	}
	secondary, err := a.Store.SyncRelays()
	if err != nil {
		return v1RelaysBody{}, err
	}
	if nip65 == nil {
		nip65 = []string{}
	}
	return v1RelaysBody{Home: a.HomeRelay, NIP65: nip65, Secondary: secondary,
		Targets: relaysync.ResolveTargets(nip65, secondary, a.HomeRelay)}, nil
}

func (a *API) writeRelays(w http.ResponseWriter, r *http.Request) {
	body, err := a.v1RelaysView(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: relays", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "reading the relays failed")
		return
	}
	writeV1(w, http.StatusOK, body)
}

func (a *API) v1GetRelays(w http.ResponseWriter, r *http.Request) {
	if a.nostrAccount(w, r) {
		a.writeRelays(w, r)
	}
}

// v1PutRelays replaces the secondary relay list.
func (a *API) v1PutRelays(w http.ResponseWriter, r *http.Request) {
	var req v1RelaysPut
	if !decodeJSON(w, r, &req) || !a.nostrAccount(w, r) {
		return
	}
	var errs []fieldError
	if req.Secondary == nil {
		errs = append(errs, fieldError{Pointer: "/secondary", Code: "required", Detail: "the relay list ([] for none)"})
	}
	if len(req.Secondary) > maxSyncRelays {
		errs = append(errs, fieldError{Pointer: "/secondary", Code: "too_many", Detail: "at most 50 relays"})
		invalid(w, r, errs)
		return
	}
	seen := map[string]bool{}
	urls := []string{}
	for i, raw := range req.Secondary {
		ptr := "/secondary/" + strconv.Itoa(i)
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		switch {
		case len(u) > maxSyncRelayURL:
			errs = append(errs, fieldError{Pointer: ptr, Code: "too_long", Detail: "at most 512 characters"})
		case !validRelayURL(u):
			errs = append(errs, fieldError{Pointer: ptr, Code: "invalid", Detail: "a ws:// or wss:// relay URL"})
		case !a.validSecondaryRelay(u):
			errs = append(errs, fieldError{Pointer: ptr, Code: "not_allowed", Detail: "not the home relay, and not a Tor or I2P relay (the server can't reach those)"})
		case seen[u]:
			errs = append(errs, fieldError{Pointer: ptr, Code: "duplicate", Detail: "this relay is already in the list"})
		default:
			seen[u] = true
			urls = append(urls, u)
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if err := a.Store.SetSyncRelays(urls); err != nil {
		slog.ErrorContext(r.Context(), "v1: set relays", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the relays failed")
		return
	}
	a.writeRelays(w, r)
}

// validRelayURL reports a ws(s) URL with a host and nothing a relay URL
// doesn't carry.
func validRelayURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "ws" || u.Scheme == "wss") && u.Host != "" && u.User == nil &&
		u.Fragment == "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, " \t\r\n")
}

// syncTargets narrows the sync targets to the relays a request names
// (every target when it names none). A named relay that isn't a target is
// a field error.
func (a *API) syncTargets(r *http.Request, relays []string) ([]relaysync.Target, []fieldError, error) {
	view, err := a.v1RelaysView(r)
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
		byURL[strings.TrimRight(t.URL, "/")] = t
	}
	var out []relaysync.Target
	var errs []fieldError
	picked := map[string]bool{}
	for i, u := range relays {
		n := strings.TrimRight(strings.TrimSpace(u), "/")
		t, ok := byURL[n]
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
			var req v1RelaysScanReq
			if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
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
		if !a.nostrAccount(w, r) {
			return
		}
		if a.Sync == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "relay sync is not configured on this server")
			return
		}
		targets, errs, err := a.syncTargets(r, relays)
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: relays", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading the relays failed")
			return
		}
		if action == "sync" && direction != "pull" && direction != "push" {
			errs = append(errs, fieldError{Pointer: "/direction", Code: "invalid",
				Detail: "pull (copy from the relays to your home relay) or push (from home to the relays)"})
		}
		if len(errs) > 0 {
			invalid(w, r, errs)
			return
		}
		if action == "scan" {
			out := a.Sync.Scan(r.Context(), targets)
			if out == nil {
				out = []relaysync.RelayDiff{}
			}
			writeV1(w, http.StatusOK, map[string]any{"relays": out})
			return
		}
		out := a.Sync.Apply(r.Context(), targets, direction)
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
	if a.PublicFeedToken == "" || a.Store == nil {
		problem(w, r, http.StatusNotFound, codeNotFound, "the public feed is off on this server (set PUBLIC_FEED_TOKEN)")
		return
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) ||
		subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(a.PublicFeedToken)) != 1 {
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
