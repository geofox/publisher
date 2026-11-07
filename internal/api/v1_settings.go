package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/media"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/redact"
	"github.com/geofox/publisher/internal/settings"
)

// The settings API (spec §7.4): the owner's settings at /v1/settings, the
// server's at /v1/instance/settings, as nested JSON by group. Each leaf of
// a read is the value, where it comes from, the variable that sets it and
// whether that locks it, and the field that describes it; a write is a
// JSON Merge Patch (null puts a value back to its default or clears a
// secret). The scope's ETag guards a write against a stale read.

// opInstanceSecrets: writing an instance secret setting (the media
// store's key) re-authenticates the owner.
const opInstanceSecrets = "instance.secrets"

// settingsTestEvery bounds the :test calls (they reach a server).
const settingsTestEvery = 10 * time.Second

// v1SettingLeaf is one setting in a read.
type v1SettingLeaf struct {
	// Value is the value in effect; for a secret, v1SecretValue.
	Value  any    `json:"value"`
	Source string `json:"source"`
	// Env is the variable that sets it ("" when none can).
	Env    string `json:"env"`
	Locked bool   `json:"locked"`
	// UpdatedAt: when it was saved in the app.
	UpdatedAt string `json:"updated_at,omitempty"`
	// Problem says why a saved secret can't be used (typed again, it can).
	Problem string `json:"problem,omitempty"`
	// AlsoSaved: the environment sets it, and a value saved in the app
	// (copied into it) applies once the variable is removed.
	AlsoSaved bool `json:"also_saved,omitempty"`
	// Generate: the server can make a value for it (:generate).
	Generate bool                  `json:"generate,omitempty"`
	Field    destination.FieldSpec `json:"field"`
}

// v1SecretValue is a secret setting as a read shows it: whether it is
// set, never its value (an address shows its origin).
type v1SecretValue struct {
	Set       bool   `json:"set"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Origin    string `json:"origin,omitempty"`
	// PublicKey is a signing key's public half (npub1…), to allow on the
	// server that checks it.
	PublicKey string `json:"public_key,omitempty"`
}

// publicHalves give a secret setting's public half, by key.
var publicHalves = map[string]func(secret string) string{
	settings.MediaBlossomKey: pubnostr.Npub,
}

// settingsService answers 503 when the server has no settings service.
func (a *API) settingsService(w http.ResponseWriter, r *http.Request) (*settings.Service, bool) {
	if a.Settings == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "this server has no settings service")
		return nil, false
	}
	return a.Settings, true
}

// settingsBody is a scope's settings, by group then name.
func settingsBody(svc *settings.Service, snap *settings.Snapshot, scope settings.Scope) map[string]map[string]v1SettingLeaf {
	out := map[string]map[string]v1SettingLeaf{}
	for _, d := range svc.Defs(scope) {
		v, _ := snap.Get(d.Key)
		leaf := v1SettingLeaf{Value: v.Value, Source: string(v.Source), Env: v.Env, Locked: v.Locked, Problem: v.Broken,
			AlsoSaved: v.Behind, Generate: d.Generate, Field: d.Field}
		if !v.UpdatedAt.IsZero() {
			leaf.UpdatedAt = v.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if d.Field.Secret {
			sv := v1SecretValue{Set: v.Set, Origin: v.Origin}
			if pub := publicHalves[d.Key]; pub != nil && v.Set {
				sv.PublicKey = pub(snap.Secret(d.Key))
			}
			if !v.UpdatedAt.IsZero() {
				sv.UpdatedAt = leaf.UpdatedAt
			}
			leaf.Value = sv
		}
		if out[d.Group()] == nil {
			out[d.Group()] = map[string]v1SettingLeaf{}
		}
		out[d.Group()][d.Name()] = leaf
	}
	return out
}

func (a *API) v1GetSettings(w http.ResponseWriter, r *http.Request) {
	a.getSettings(w, r, settings.Owner)
}

func (a *API) v1GetInstanceSettings(w http.ResponseWriter, r *http.Request) {
	a.getSettings(w, r, settings.Instance)
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, scope settings.Scope) {
	svc, ok := a.settingsService(w, r)
	if !ok {
		return
	}
	snap := svc.Current()
	w.Header().Set("ETag", snap.Version(scope))
	writeV1(w, http.StatusOK, settingsBody(svc, snap, scope))
}

func (a *API) v1PatchSettings(w http.ResponseWriter, r *http.Request) {
	a.patchSettings(w, r, settings.Owner)
}

func (a *API) v1PatchInstanceSettings(w http.ResponseWriter, r *http.Request) {
	a.patchSettings(w, r, settings.Instance)
}

// settingsIfMatch reads If-Match for a settings write: "" for any version
// (absent or *), else the ETag as sent (a weak one never matches).
func settingsIfMatch(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "*" {
		return ""
	}
	return v
}

func (a *API) patchSettings(w http.ResponseWriter, r *http.Request, scope settings.Scope) {
	svc, ok := a.settingsService(w, r)
	if !ok {
		return
	}
	var in map[string]json.RawMessage
	if !decodeJSON(w, r, &in) {
		return
	}
	values := map[string]any{}
	var errs []fieldError
	for _, group := range sortedRawKeys(in) {
		raw := in[group]
		var leaves map[string]json.RawMessage
		if isJSONNull(raw) || json.Unmarshal(raw, &leaves) != nil || leaves == nil {
			errs = append(errs, fieldError{Pointer: "/" + group, Code: "invalid", Detail: "an object of the settings to change (null puts one back to its default)"})
			continue
		}
		for _, name := range sortedRawKeys(leaves) {
			var v any
			if err := json.Unmarshal(leaves[name], &v); err != nil {
				errs = append(errs, fieldError{Pointer: "/" + group + "/" + name, Code: "invalid", Detail: "not a JSON value"})
				continue
			}
			values[group+"."+name] = v
		}
	}
	if len(errs) > 0 {
		invalid(w, r, errs)
		return
	}
	if len(values) == 0 {
		snap := svc.Current()
		w.Header().Set("ETag", snap.Version(scope))
		writeV1(w, http.StatusOK, settingsBody(svc, snap, scope))
		return
	}
	if !a.mayWriteSettings(w, r, svc, scope, keysOf(values)) {
		return
	}
	if on, _ := values[settings.MediaPublic].(bool); on && scope == settings.Instance && !svc.Current().Bool(settings.MediaPublic) &&
		!a.checkMediaPublic(w, r) {
		return
	}
	p, _ := principalFrom(r.Context())
	snap, err := svc.Apply(r.Context(), settings.Change{Scope: scope, Values: values, IfMatch: settingsIfMatch(r), By: settingsActor(p)})
	if err != nil {
		a.settingsError(w, r, scope, snap, err)
		return
	}
	w.Header().Set("ETag", snap.Version(scope))
	writeV1(w, http.StatusOK, settingsBody(svc, snap, scope))
}

// mayWriteSettings checks who may write these keys; on false it has
// answered. Behind the reverse proxy alone no secret setting is written
// (the instance routes are refused there whole); an instance secret
// needs the owner to confirm it's them.
func (a *API) mayWriteSettings(w http.ResponseWriter, r *http.Request, svc *settings.Service, scope settings.Scope, keys []string) bool {
	p, _ := principalFrom(r.Context())
	secret := false
	for _, k := range keys {
		if d, ok := svc.Def(k); ok && d.Scope == scope && d.Field.Secret {
			secret = true
		}
	}
	if !secret {
		return true
	}
	if p.Kind == "proxy" || a.mode() == ModeProxy {
		problem(w, r, http.StatusForbidden, codeSignInRequired, "secret settings can't be set behind the reverse proxy alone: "+signInRequired)
		return false
	}
	if scope == settings.Instance {
		return a.requireReauth(w, r, opInstanceSecrets, "")
	}
	return true
}

// settingsActor is who changes a setting, for the record.
func settingsActor(p principal) string {
	switch p.Kind {
	case "token":
		return "token:" + p.ID
	case "":
		return ""
	}
	return p.Kind
}

// settingsError answers a settings write's error.
func (a *API) settingsError(w http.ResponseWriter, r *http.Request, scope settings.Scope, snap *settings.Snapshot, err error) {
	var locked *settings.LockedError
	var verr *settings.ValidationError
	switch {
	case errors.As(err, &locked):
		detail := locked.Error() + ": change it there"
		problem(w, r, http.StatusConflict, codeLockedByEnv, detail,
			fieldError{Pointer: settingPointer(locked.Key), Code: "locked", Detail: detail})
	case errors.As(err, &verr):
		errs := make([]fieldError, 0, len(verr.Fields))
		for _, f := range verr.Fields {
			errs = append(errs, fieldError{Pointer: settingPointer(f.Key), Code: f.Code, Detail: f.Detail})
		}
		invalid(w, r, errs)
	case errors.Is(err, settings.ErrStale):
		if snap != nil {
			w.Header().Set("ETag", snap.Version(scope))
		}
		problem(w, r, http.StatusPreconditionFailed, codePreconditionFailed, "the settings changed since you read them; read them again, then decide")
	case errors.Is(err, accounts.ErrMasterKeyRequired):
		problem(w, r, http.StatusConflict, codeMasterKeyRequired, "secret settings are kept encrypted: "+err.Error())
	case errors.Is(err, accounts.ErrMasterKeyMismatch):
		problem(w, r, http.StatusConflict, codeMasterKeyMismatch, err.Error())
	default:
		slog.ErrorContext(r.Context(), "v1: saving settings failed", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "saving the settings failed")
	}
}

// settingPointer is a key's JSON Pointer in the nested body
// ("composing.languages" → "/composing/languages").
func settingPointer(key string) string { return "/" + strings.ReplaceAll(key, ".", "/") }

func sortedRawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// v1SettingAction serves POST /v1/settings/{key}:copy-from-env,
// /v1/settings/feed.token:generate and /v1/settings/{group}:test.
func (a *API) v1SettingAction(w http.ResponseWriter, r *http.Request) {
	a.settingAction(w, r, settings.Owner)
}

// v1InstanceSettingAction serves POST
// /v1/instance/settings/{key}:copy-from-env.
func (a *API) v1InstanceSettingAction(w http.ResponseWriter, r *http.Request) {
	a.settingAction(w, r, settings.Instance)
}

func (a *API) settingAction(w http.ResponseWriter, r *http.Request, scope settings.Scope) {
	key, action, found := strings.Cut(r.PathValue("id"), ":")
	if !found {
		w.Header().Set("Allow", "POST")
		problem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "POST to a setting's :copy-from-env, :generate or a group's :test")
		return
	}
	svc, ok := a.settingsService(w, r)
	if !ok {
		return
	}
	switch action {
	case "copy-from-env":
		a.copySettingFromEnv(w, r, svc, scope, key)
	case "generate":
		a.generateSetting(w, r, svc, scope, key)
	case "test":
		a.testSettings(w, r, scope, key)
	default:
		problem(w, r, http.StatusNotFound, codeNotFound, "no such action :"+action+"; settings take :copy-from-env, :generate and :test")
	}
}

// copySettingFromEnv saves a setting's environment value in the app, so
// the variable can go without the setting falling back to its default.
func (a *API) copySettingFromEnv(w http.ResponseWriter, r *http.Request, svc *settings.Service, scope settings.Scope, key string) {
	if d, ok := svc.Def(key); !ok || d.Scope != scope {
		problem(w, r, http.StatusNotFound, codeNotFound, "no setting "+key+" here")
		return
	}
	if !a.mayWriteSettings(w, r, svc, scope, []string{key}) {
		return
	}
	p, _ := principalFrom(r.Context())
	snap, err := svc.CopyFromEnv(r.Context(), key, settingsActor(p))
	if errors.Is(err, settings.ErrNotFromEnv) {
		problem(w, r, http.StatusConflict, codeInvalidState, key+" "+err.Error())
		return
	}
	if err != nil {
		a.settingsError(w, r, scope, snap, err)
		return
	}
	w.Header().Set("ETag", snap.Version(scope))
	writeV1(w, http.StatusOK, settingsBody(svc, snap, scope))
}

// v1GeneratedSetting is what :generate answers: the new value, shown
// this once (the feed's token, to give to whoever reads the feed).
type v1GeneratedSetting struct {
	Value string `json:"value"`
}

// generateSetting makes a value Publisher can make (Def.Generate), saves
// it and answers it once (no-store; never stored as an answer).
func (a *API) generateSetting(w http.ResponseWriter, r *http.Request, svc *settings.Service, scope settings.Scope, key string) {
	d, ok := svc.Def(key)
	if !ok || d.Scope != scope || !d.Generate {
		problem(w, r, http.StatusNotFound, codeNotFound, "Publisher can't make a value for "+key)
		return
	}
	if !a.mayWriteSettings(w, r, svc, scope, []string{key}) {
		return
	}
	val, show, err := generateValue(key)
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: making a setting's value failed", "key", key, "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "making the value failed")
		return
	}
	p, _ := principalFrom(r.Context())
	snap, err := svc.Apply(r.Context(), settings.Change{Scope: scope, Values: map[string]any{key: val}, IfMatch: settingsIfMatch(r), By: settingsActor(p)})
	if err != nil {
		a.settingsError(w, r, scope, snap, err)
		return
	}
	w.Header().Set("ETag", snap.Version(scope))
	writeV1(w, http.StatusOK, v1GeneratedSetting{Value: show})
}

// generators make the values Publisher can make, by key: the value to
// save and what to show (the same, or its public half).
var generators = map[string]func() (save, show string, err error){
	settings.FeedToken: func() (string, string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", "", err
		}
		t := base64.RawURLEncoding.EncodeToString(b)
		return t, t, nil
	},
	// The key itself is never shown: its public key is what the Blossom
	// server's owner allows.
	settings.MediaBlossomKey: func() (string, string, error) {
		sk, npub := pubnostr.NewSigningKey()
		return sk, npub, nil
	},
}

func generateValue(key string) (save, show string, err error) {
	g, ok := generators[key]
	if !ok {
		return "", "", errors.New("no generator for " + key)
	}
	return g()
}

// v1SettingsTest is what :test answers: whether the server answered with
// success, else a class of failure (never its status or words: the
// address may be private, and its answer isn't the owner's to read).
type v1SettingsTest struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// SettingsTester sends a test to a group's server (alerts, feed; the
// instance's media), with the settings in effect: the HTTP status, or the
// transport's error. ok false: nothing is set to test.
type SettingsTester func(ctx context.Context, group string) (status int, set bool, err error)

func (a *API) testSettings(w http.ResponseWriter, r *http.Request, scope settings.Scope, group string) {
	groups := map[settings.Scope][]string{settings.Owner: {"alerts", "feed"}, settings.Instance: {"media"}}[scope]
	if !containsString(groups, group) || a.TestSettings == nil {
		problem(w, r, http.StatusNotFound, codeNotFound, "no test for "+group+" here; "+strings.Join(groups, " and ")+" can be tested")
		return
	}
	p, _ := principalFrom(r.Context())
	if wait := a.limits().tests.Take(p.OwnerID + ":" + string(scope) + ":" + group); wait > 0 {
		rateLimited(w, r, wait, "a test can run once every 10 seconds")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, set, err := a.TestSettings(ctx, group)
	if !set {
		problem(w, r, http.StatusConflict, codeInvalidState, "nothing is set to test: set its address first")
		return
	}
	out := v1SettingsTest{OK: err == nil && status/100 == 2}
	if !out.OK {
		out.Error = testErrorClass(err)
		slog.InfoContext(r.Context(), "v1: a settings test failed", "group", group, "class", out.Error, "status", status,
			"err", redact.URLError(err))
	}
	writeV1(w, http.StatusOK, out)
}

// checkMediaPublic runs the self-test turning media.public on needs
// (spec §7.6): this server's disk media fetched at PUBLIC_URL/media/, as
// a network fetching them would. On false it has answered.
func (a *API) checkMediaPublic(w http.ResponseWriter, r *http.Request) bool {
	const ptr = "/media/public"
	if a.DiskMedia == nil {
		invalid(w, r, []fieldError{{Pointer: ptr, Code: "not_reachable", Detail: "this server keeps no media on its disk"}})
		return false
	}
	// Its own window, apart from Test the media store's: testing the
	// store, then turning this on, is one owner's ordinary order.
	p, _ := principalFrom(r.Context())
	if wait := a.limits().tests.Take(p.OwnerID + ":" + string(settings.Instance) + ":media.public"); wait > 0 {
		rateLimited(w, r, wait, "the self-test runs once every 10 seconds")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := a.DiskMedia.CheckPublic(ctx, media.NoRedirects())
	if err == nil {
		return true
	}
	slog.InfoContext(r.Context(), "v1: the public media self-test failed", "err", redact.URLError(err))
	invalid(w, r, []fieldError{{Pointer: ptr, Code: "not_reachable", Detail: mediaPublicDetail(err)}})
	return false
}

// mediaPublicDetail says why the public self-test failed, in Publisher's
// words (what PUBLIC_URL answered, or the class of a transport error).
func mediaPublicDetail(err error) string {
	var pe *media.ProbeError
	switch {
	case errors.Is(err, media.ErrNoPublicURL):
		return err.Error()
	case errors.As(err, &pe):
		return pe.Error()
	}
	return "PUBLIC_URL/media/ couldn't be reached from this server (" + testErrorClass(err) + "): is PUBLIC_URL its public address?"
}

// testErrorClass is a failed test's class: unreachable, refused,
// timeout, http_error (it answered, but not with success; a redirect
// Publisher won't follow included) or blocked_destination (an address
// Publisher may not reach).
func testErrorClass(err error) string {
	var ne net.Error
	switch {
	case err == nil, errors.Is(err, destination.ErrRedirectRefused):
		return "http_error"
	case errors.Is(err, destination.ErrBlockedAddress):
		return "blocked_destination"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	}
	return "unreachable"
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
