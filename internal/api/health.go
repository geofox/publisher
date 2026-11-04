package api

import (
	"net/http"
)

// ─── /healthz ────────────────────────────────────────────────────────────

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
