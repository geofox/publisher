package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/httpx"
	pubnostr "github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/relaysync"
)

// ─── /api/sync ───────────────────────────────────────────────────────────

func (a *API) syncRelayLists() (nip65, secondary []string) {
	if a.np != nil {
		if w, err := a.np.ResolveWriteRelays(context.Background()); err == nil {
			nip65 = w
		}
	}
	secondary, _ = a.Store.SyncRelays()
	return
}

func (a *API) resolveSyncTargets() []relaysync.Target {
	nip65, secondary := a.syncRelayLists()
	return relaysync.ResolveTargets(nip65, secondary, a.HomeRelay)
}

// filteredSyncTargets returns the resolved targets, optionally narrowed to the
// given relay URLs (trailing-slash-normalized). Empty relays = all targets.
func (a *API) filteredSyncTargets(relays []string) []relaysync.Target {
	targets := a.resolveSyncTargets()
	if len(relays) == 0 {
		return targets
	}
	want := map[string]bool{}
	for _, u := range relays {
		want[strings.TrimRight(u, "/")] = true
	}
	out := targets[:0:0]
	for _, t := range targets {
		if want[strings.TrimRight(t.URL, "/")] {
			out = append(out, t)
		}
	}
	return out
}

// handleSyncTargets returns the resolved target set so the SPA can render the
// relay rows up front and scan/apply them one at a time (live progress).
func (a *API) handleSyncTargets(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"targets": a.resolveSyncTargets()})
}

func (a *API) handleSyncRelaysList(w http.ResponseWriter, r *http.Request) {
	nip65, secondary := a.syncRelayLists()
	httpx.WriteJSON(w, http.StatusOK, map[string][]string{"nip65": nip65, "secondary": secondary})
}

// validSecondaryRelay rejects non-ws(s), overlay, and the home relay.
func (a *API) validSecondaryRelay(u string) bool {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "ws://") && !strings.HasPrefix(u, "wss://") {
		return false
	}
	if pubnostr.IsOverlayRelay(u) {
		return false
	}
	return strings.TrimRight(u, "/") != strings.TrimRight(a.HomeRelay, "/")
}

func (a *API) handleSyncRelayAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !a.validSecondaryRelay(b.URL) {
		httpx.WriteError(w, http.StatusBadRequest, "relay must be ws(s)://, not overlay, not the home relay")
		return
	}
	// Store the normalized URL (no trailing slash) so it matches ResolveTargets'
	// dedup/exclusion — otherwise a trailing-slash relay lists but never syncs.
	if err := a.Store.AddSyncRelay(strings.TrimRight(strings.TrimSpace(b.URL), "/")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleSyncRelaysList(w, r)
}

func (a *API) handleSyncRelayRemove(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.Store.RemoveSyncRelay(strings.TrimRight(strings.TrimSpace(b.URL), "/")); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleSyncRelaysList(w, r)
}

func (a *API) handleSyncScan(w http.ResponseWriter, r *http.Request) {
	if a.Sync == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "sync not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var b struct {
		Relays []string `json:"relays"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b) // body optional (per-relay live scan sends one)
	out := a.Sync.Scan(r.Context(), a.filteredSyncTargets(b.Relays))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"relays": out})
}

func (a *API) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	if a.Sync == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "sync not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var b struct {
		Direction string   `json:"direction"`
		Relays    []string `json:"relays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.Direction != "pull" && b.Direction != "push" {
		httpx.WriteError(w, http.StatusBadRequest, "direction must be pull or push")
		return
	}
	out := a.Sync.Apply(r.Context(), a.filteredSyncTargets(b.Relays), b.Direction)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}
