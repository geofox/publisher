package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/verify"
)

// ─── POST /api/verify ────────────────────────────────────────────────────

func (a *API) handleVerify(w http.ResponseWriter, r *http.Request) {
	if a.Verify == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "verification not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVerifyRequestBytes)
	var req struct {
		Input    string `json:"input"`
		Platform string `json:"platform"`
		Expected string `json:"expected"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxVerifyRequestBytes))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "input is required")
		return
	}
	v := a.Verify.Verify(r.Context(), verify.Input{
		Raw: req.Input, Platform: req.Platform, Expected: req.Expected,
	})
	status := http.StatusOK
	if v.Status == verify.StatusError {
		status = http.StatusBadGateway
	}
	httpx.WriteJSON(w, status, v)
}
