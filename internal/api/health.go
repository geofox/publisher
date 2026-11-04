package api

import (
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/httpx"
)

// ─── /healthz ────────────────────────────────────────────────────────────

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ─── GET /readyz ─────────────────────────────────────────────────────────
//
// Readiness, as opposed to /healthz liveness: can this instance accept and
// persist work right now? 200 when every critical check passes (status "ok",
// or "degraded" when only a non-critical one fails), 503 "unavailable"
// otherwise. ?external=1 adds cached, never-critical upstream reachability
// checks. Open like /healthz (orchestrators can't log in); the body carries
// only check names, statuses and short error classes — details go to the log.

func (a *API) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.Ready == nil {
		httpx.WriteJSON(w, http.StatusOK, health.Report{Status: health.StatusOK, Checks: map[string]health.Result{}})
		return
	}
	ext := strings.ToLower(r.URL.Query().Get("external"))
	rep := a.Ready.Run(r.Context(), ext == "1" || ext == "true")
	code := http.StatusOK
	if rep.Status == health.StatusUnavailable {
		code = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, code, rep)
}
